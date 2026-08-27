package adapterproxy

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/sourcepolicy"
)

func NewServer(cfg Config) *Server {
	if cfg.AutoJoinWarmup <= 0 {
		cfg.AutoJoinWarmup = defaultAutoJoinWarmup
	}
	if cfg.AutoJoinActivityWindow <= 0 {
		cfg.AutoJoinActivityWindow = cfg.AutoJoinWarmup
	}
	if cfg.UDPPlainRetryJitter < 0 {
		cfg.UDPPlainRetryJitter = 0
	}
	if cfg.UDPPlainRetryJitter > 1 {
		cfg.UDPPlainRetryJitter = 1
	}
	if cfg.UDPPlainRetryJitter == 0 {
		cfg.UDPPlainRetryJitter = defaultRetryJitter
	}
	if cfg.UDPPlainStartWait <= 0 {
		cfg.UDPPlainStartWait = udpPlainStartWaitDefault
	}

	server := &Server{
		cfg:                     cfg,
		sessions:                make(map[uint64]*session),
		busToken:                make(chan struct{}, 1),
		leasedBySess:            make(map[uint64]sourcepolicy.Lease),
		synCh:                   make(chan struct{}, 1),
		udpClients:              make(map[string]*udpClientEntry),
		udpQueue:                make(chan udpDatagram, udpNorthboundQueueCap),
		randomFloat64:           rand.Float64,
		startOfTelegram:         true,
		observedInitiatorAt:     make(map[byte]time.Time),
		collisionBySession:      make(map[uint64]byte),
		learnedBySession:        make(map[uint64]sessionInitiatorLearning),
		localRespondersByTarget: make(map[byte]targetResponderAssociation),
		startArbContenders:      make(map[uint64]*startArbContender),
		infoCache:               newAdapterInfoCache(),
		reinitGuard:             make(chan struct{}, 1),
		// PX7/PX66/CR-P1: Use close-based broadcast so all goroutines
		// that select on upstreamLost are notified (not just one receiver).
		upstreamLost: make(chan struct{}),
	}
	server.busToken <- struct{}{}

	// PX57: Honor Config.SourceAddressPolicy if set, defaulting to Soft.
	reservationMode := sourcepolicy.ReservationModeSoft
	if cfg.SourceAddressPolicy != "" {
		reservationMode = cfg.SourceAddressPolicy
	}
	policy, err := sourcepolicy.NewPolicy(sourcepolicy.Config{
		ReservationMode: reservationMode,
	})
	if err == nil {
		manager, managerErr := sourcepolicy.NewLeaseManager(policy, sourcepolicy.LeaseManagerOptions{
			LeaseDuration: defaultLeaseDuration,
		})
		if managerErr == nil {
			server.leaseManager = manager
		}
	}

	return server
}

func (server *Server) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// PX49/AT-02: Validate config before proceeding.
	if err := server.cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	upstream, err := dialUpstream(ctx, server.cfg.UpstreamTransport, server.cfg.UpstreamAddr, server.cfg.DialTimeout, server.cfg.ReadTimeout, server.cfg.WriteTimeout)
	if err != nil {
		return fmt.Errorf("dial upstream: %w", err)
	}
	server.upstream = upstream

	if strings.TrimSpace(server.cfg.WireLogPath) != "" {
		logFile, err := os.OpenFile(server.cfg.WireLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			_ = upstream.Close()
			return fmt.Errorf("open wire log: %w", err)
		}
		// CR-P2b: Seed written counter from existing file size so rotation
		// is enforced across process restarts.
		var existingSize int64
		if stat, statErr := logFile.Stat(); statErr == nil {
			existingSize = stat.Size()
		}
		server.wireLog = &wireLogger{
			file:    logFile,
			writer:  bufio.NewWriterSize(logFile, 16*1024),
			path:    server.cfg.WireLogPath,
			maxSize: server.cfg.WireLogMaxSize,
			written: existingSize,
		}
	}
	// Request additional infos up-front so downstream clients can query INFO without
	// being sensitive to proxy initialization ordering.
	server.initSentAtNano.Store(time.Now().UnixNano())
	if err := server.upstream.SendInit(0x01); err != nil {
		server.initSentAtNano.Store(0)
		// Best-effort: some adapters respond with RESETTED, others start streaming immediately.
	}

	listener, err := net.Listen("tcp", server.cfg.ListenAddr)
	if err != nil {
		_ = upstream.Close()
		return fmt.Errorf("listen %q: %w", server.cfg.ListenAddr, err)
	}
	server.listener = listener

	if strings.TrimSpace(server.cfg.UDPPlainListenAddr) != "" {
		udpAddress, err := net.ResolveUDPAddr("udp", server.cfg.UDPPlainListenAddr)
		if err != nil {
			_ = listener.Close()
			_ = upstream.Close()
			return fmt.Errorf("resolve udp listen %q: %w", server.cfg.UDPPlainListenAddr, err)
		}
		udpListener, err := net.ListenUDP("udp", udpAddress)
		if err != nil {
			_ = listener.Close()
			_ = upstream.Close()
			return fmt.Errorf("listen udp %q: %w", server.cfg.UDPPlainListenAddr, err)
		}
		server.udpListener = udpListener
		server.waitGroup.Add(2)
		go server.runUDPPlainReader(ctx)
		go server.runUDPPlainWriter(ctx)
	}

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	if server.cfg.AutoJoinWarmup > 0 {
		if server.cfg.Debug {
			log.Printf("auto_join_warmup=%s", server.cfg.AutoJoinWarmup)
		}
		warmupCancelled := false
		select {
		case <-ctx.Done():
			warmupCancelled = true
		case <-time.After(server.cfg.AutoJoinWarmup):
		}
		// R1: If ctx was cancelled during warmup, fall through to the cleanup
		// path instead of returning early — listener, upstream, and goroutines
		// must be cleaned up properly.
		if warmupCancelled {
			_ = listener.Close()
			if server.udpListener != nil {
				_ = server.udpListener.Close()
			}
			_ = upstream.Close()
			server.closeSessions()
			server.waitGroup.Wait()
			if server.wireLog != nil {
				_ = server.wireLog.Close()
			}
			return ctx.Err()
		}
		if selected, err := server.selectAutoInitiator(); err == nil {
			server.mutex.Lock()
			server.autoJoinInitiator = selected
			server.mutex.Unlock()
			if server.cfg.Debug {
				log.Printf("auto_join_selected initiator=0x%02X", selected)
			}
		} else if server.cfg.Debug {
			log.Printf("auto_join_select_error=%v", err)
		}
	}

	// PX59: Monitor upstream loss: close listener to unblock Accept.
	// Track goroutine in waitGroup so Serve() does not return while it runs.
	server.waitGroup.Add(1)
	go func() {
		defer server.waitGroup.Done()
		select {
		case <-server.upstreamLost:
			_ = listener.Close()
		case <-ctx.Done():
		}
	}()

	// PX45: Periodically expire stale leases even when the bus is quiet.
	// Without this, expired leases persist until the next Acquire/Renew call.
	if server.leaseManager != nil {
		server.waitGroup.Add(1)
		go func() {
			defer server.waitGroup.Done()
			ticker := time.NewTicker(defaultLeaseDuration / 2)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					server.leasesMu.Lock()
					expired := server.leaseManager.Expire()
					for _, lease := range expired {
						for sessID, sessLease := range server.leasedBySess {
							if sessLease.OwnerID == lease.OwnerID {
								delete(server.leasedBySess, sessID)
								break
							}
						}
					}
					server.leasesMu.Unlock()
				case <-ctx.Done():
					return
				case <-server.upstreamLost:
					return
				}
			}
		}()
	}

	upstreamDied := false
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			// Check if upstream died (triggered listener close).
			select {
			case <-server.upstreamLost:
				upstreamDied = true
			default:
			}
			if upstreamDied || isClosedNetworkError(err) {
				break
			}
			continue
		}

		// CR4-P2b: Enforce MaxConcurrentSessions at the adapter-proxy level.
		if server.cfg.MaxConcurrentSessions > 0 {
			server.mutex.Lock()
			active := len(server.sessions)
			server.mutex.Unlock()
			if active >= server.cfg.MaxConcurrentSessions {
				_ = connection.Close()
				continue
			}
		}

		// Inline-3/PX53: Rate-limit accepts on the real runtime path.
		// CR-P2: Also abort on upstreamLost or ctx cancellation.
		if server.cfg.AcceptRateLimit > 0 {
			rateLimitAbort := false
			select {
			case <-time.After(server.cfg.AcceptRateLimit):
			case <-ctx.Done():
				_ = connection.Close()
				rateLimitAbort = true
			case <-server.upstreamLost:
				_ = connection.Close()
				upstreamDied = true
				rateLimitAbort = true
			}
			if rateLimitAbort {
				break
			}
		}

		// P2: Guard against registration after upstream loss.
		select {
		case <-server.upstreamLost:
			_ = connection.Close()
			upstreamDied = true
		default:
		}
		if upstreamDied {
			break
		}

		sessionState := server.registerSession(connection)
		server.waitGroup.Add(2)
		go func() {
			defer server.waitGroup.Done()
			sessionState.runWriter(nil)
		}()
		go func() {
			defer server.waitGroup.Done()
			sessionState.runReader(
				func(frame downstream.Frame) {
					server.handleFrame(ctx, sessionState.id, frame)
				},
				nil,
			)
			server.unregisterSession(sessionState.id)
		}()
	}

	_ = listener.Close()
	if server.udpListener != nil {
		_ = server.udpListener.Close()
	}
	// GH-P1: Close upstream BEFORE waitGroup.Wait() so runUpstreamReader
	// unblocks from ReadFrame and can exit. Otherwise shutdown stalls
	// waiting for a goroutine that will never wake.
	_ = upstream.Close()
	server.closeSessions()
	server.waitGroup.Wait()
	if server.wireLog != nil {
		_ = server.wireLog.Close()
	}

	if upstreamDied {
		return ErrUpstreamLost
	}
	return nil
}
