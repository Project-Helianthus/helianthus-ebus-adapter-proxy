package adapterproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/sourcepolicy"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

const (
	defaultLeaseDuration     = 30 * time.Minute
	ebusSyn                  = byte(0xAA)
	udpBridgeOwnerID         = ^uint64(0)
	busIdleReleaseGrace      = 400 * time.Millisecond
	udpPlainSynWait          = 5 * time.Second
	udpPlainBootstrapWait    = 250 * time.Millisecond
	udpPlainStartWaitDefault = 5 * time.Second
	udpPlainMaxAttempts      = 4
	udpPlainBackoffBase      = 25 * time.Millisecond
	udpPlainBackoffMax       = 400 * time.Millisecond
	defaultRetryJitter       = 0.2
	defaultAutoJoinWarmup    = 5 * time.Second
	udpNorthboundQueueCap    = 1024
)

type udpDatagram struct {
	payload []byte
}

type Server struct {
	cfg Config

	listener net.Listener
	upstream upstream

	wireLog *wireLogger
	synCh   chan struct{}

	udpListener  *net.UDPConn
	udpClientsMu sync.RWMutex
	udpClients   map[string]*net.UDPAddr
	udpQueue     chan udpDatagram

	upstreamFeatures atomic.Uint32
	lastWireRXAtNano atomic.Int64

	backpressureDrops  atomic.Uint64
	backpressureCloses atomic.Uint64

	randomFloat64 func() float64

	mutex    sync.Mutex
	sessions map[uint64]*session
	nextID   uint64

	autoJoinInitiator byte
	startOfTelegram   bool

	observedMu          sync.Mutex
	observedInitiatorAt map[byte]time.Time
	collisionBySession  map[uint64]byte

	busToken chan struct{}
	busOwner uint64
	busDirty bool
	busOwned time.Time

	pendingStartMu sync.Mutex
	pendingStart   *pendingStart

	pendingInfoMu sync.Mutex
	pendingInfo   *pendingInfo

	leasesMu     sync.Mutex
	leaseManager *sourcepolicy.LeaseManager
	leasedBySess map[uint64]sourcepolicy.Lease

	waitGroup sync.WaitGroup
}

type pendingStart struct {
	sessionID uint64
	respCh    chan downstream.Frame
	mode      pendingStartMode
	initiator byte
	delivered bool
}

type pendingInfo struct {
	sessionID uint64
	remaining int
}

type pendingStartMode uint8

const (
	pendingStartModeENH pendingStartMode = iota
	pendingStartModeUDPPlain
)

var preferredInitiatorAddresses = []byte{
	0xF7, 0xF3, 0xF1, 0xF0,
	0x7F, 0x77, 0x73, 0x71, 0x70,
	0x3F, 0x37, 0x33, 0x31, 0x30,
	0x1F, 0x17, 0x13, 0x11, 0x10,
	0x0F, 0x07, 0x03, 0x01,
}

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
		cfg:                 cfg,
		sessions:            make(map[uint64]*session),
		busToken:            make(chan struct{}, 1),
		leasedBySess:        make(map[uint64]sourcepolicy.Lease),
		synCh:               make(chan struct{}, 1),
		udpClients:          make(map[string]*net.UDPAddr),
		udpQueue:            make(chan udpDatagram, udpNorthboundQueueCap),
		randomFloat64:       rand.Float64,
		startOfTelegram:     true,
		observedInitiatorAt: make(map[byte]time.Time),
		collisionBySession:  make(map[uint64]byte),
	}
	server.busToken <- struct{}{}

	policy, err := sourcepolicy.NewPolicy(sourcepolicy.Config{
		ReservationMode: sourcepolicy.ReservationModeSoft,
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
		server.wireLog = &wireLogger{
			file:   logFile,
			writer: bufio.NewWriterSize(logFile, 16*1024),
		}
	}
	// Request additional infos up-front so downstream clients can query INFO without
	// being sensitive to proxy initialization ordering.
	if err := server.upstream.SendInit(0x01); err != nil {
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(server.cfg.AutoJoinWarmup):
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

	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if isClosedNetworkError(err) {
				break
			}
			continue
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
	server.closeSessions()
	server.waitGroup.Wait()
	_ = upstream.Close()
	if server.wireLog != nil {
		_ = server.wireLog.Close()
	}

	return nil
}

func (server *Server) registerSession(connection net.Conn) *session {
	server.mutex.Lock()
	server.nextID++
	id := server.nextID
	sessionState := newSession(id, connection, server.cfg.ReadTimeout, server.cfg.WriteTimeout)
	server.sessions[id] = sessionState
	server.mutex.Unlock()
	return sessionState
}

func (server *Server) unregisterSession(sessionID uint64) {
	server.mutex.Lock()
	delete(server.sessions, sessionID)
	server.mutex.Unlock()

	server.releaseBusIfOwner(sessionID)
	server.releaseLease(sessionID)
	server.clearSessionCollision(sessionID)

	server.pendingStartMu.Lock()
	if server.pendingStart != nil && server.pendingStart.sessionID == sessionID {
		server.pendingStart = nil
	}
	server.pendingStartMu.Unlock()

	server.pendingInfoMu.Lock()
	if server.pendingInfo != nil && server.pendingInfo.sessionID == sessionID {
		server.pendingInfo = nil
	}
	server.pendingInfoMu.Unlock()
}

func (server *Server) closeSessions() {
	server.mutex.Lock()
	sessions := make([]*session, 0, len(server.sessions))
	for _, sess := range server.sessions {
		sessions = append(sessions, sess)
	}
	server.sessions = make(map[uint64]*session)
	server.mutex.Unlock()

	for _, sess := range sessions {
		_ = sess.Close()
	}
}

func (server *Server) handleFrame(ctx context.Context, sessionID uint64, frame downstream.Frame) {
	command := southboundenh.ENHCommand(frame.Command)
	if len(frame.Payload) != 1 {
		return
	}
	data := frame.Payload[0]

	switch command {
	case southboundenh.ENHReqInit:
		initFeatures := server.initResponseFeatures(data)
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResResetted),
			Payload: []byte{initFeatures},
		})
	case southboundenh.ENHReqInfo:
		server.handleInfo(sessionID, data)
	case southboundenh.ENHReqStart:
		server.handleStart(ctx, sessionID, data)
	case southboundenh.ENHReqSend:
		server.handleSend(sessionID, data)
	default:
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
	}
}

func (server *Server) initResponseFeatures(requested byte) byte {
	upstream := byte(server.upstreamFeatures.Load())
	if upstream != 0 {
		return upstream
	}
	return requested
}

func (server *Server) handleStart(ctx context.Context, sessionID uint64, initiator byte) {
	server.mutex.Lock()
	sess := server.sessions[sessionID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}

	if server.cfg.Debug {
		log.Printf("session=%d start initiator=0x%02X", sessionID, initiator)
	}

	if initiator == ebusSyn {
		server.handleStartCancel(sessionID)
		if !server.isWirePlainUpstream() {
			// Forward best-effort cancellation upstream. The enhanced protocol does not
			// mandate a response for START+SYN, so we must not block waiting for one.
			_ = server.upstream.WriteFrame(downstream.Frame{
				Command: byte(southboundenh.ENHReqStart),
				Payload: []byte{initiator},
			})
		}
		return
	}

	if initiator == 0x00 {
		if leasedAddress, ok := server.sessionLeaseAddress(sessionID); ok {
			initiator = leasedAddress
			if server.cfg.Debug {
				log.Printf("session=%d auto_join_reuse_lease=0x%02X", sessionID, leasedAddress)
			}
		} else {
			selected, err := server.selectAutoInitiator()
			if err != nil {
				server.reply(sessionID, downstream.Frame{
					Command: byte(southboundenh.ENHResErrorHost),
					Payload: []byte{0x00},
				})
				return
			}
			initiator = selected
		}
		if server.cfg.Debug {
			log.Printf("session=%d auto_join_initiator=0x%02X", sessionID, initiator)
		}
	}

	selectedInitiator, err := server.acquireLease(sessionID, initiator)
	if err != nil {
		log.Printf(
			"session=%d lease_rejected initiator=0x%02X reason=%v",
			sessionID,
			initiator,
			err,
		)
		var conflict sourcepolicy.LeaseConflictError
		if errors.As(err, &conflict) && conflict.Code == sourcepolicy.LeaseConflictCodeAddressInUse {
			winner := conflict.Address
			if winner == 0x00 {
				winner = initiator
			}
			server.markSessionCollision(sessionID, winner)
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResFailed),
				Payload: []byte{winner},
			})
			return
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}
	initiator = selectedInitiator

	if server.isWirePlainUpstream() {
		server.handleStartUDPPlain(ctx, sessionID, initiator)
		return
	}

	ownedBySession := func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		return server.busOwner == sessionID
	}()

	if !ownedBySession {
		waitStart := time.Now()
		select {
		case <-server.busToken:
		case <-ctx.Done():
			return
		case <-sess.done:
			return
		}

		if server.cfg.Debug {
			log.Printf("session=%d start_wait=%s", sessionID, time.Since(waitStart))
		}
	} else {
		server.mutex.Lock()
		if server.busOwner == sessionID {
			server.busDirty = true
			server.busOwned = time.Now().UTC()
		}
		server.mutex.Unlock()
		if server.cfg.Debug {
			log.Printf("session=%d start_reuse_owner=true", sessionID)
		}
	}

	// If we acquired the bus token above, it is now held until SYN (end-of-message)
	// or an error/disconnect. If we are reusing an existing ownership, do not touch
	// the token here.

	select {
	case <-ctx.Done():
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		if !ownedBySession {
			server.releaseBusToken()
		}
		return
	case <-sess.done:
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		if !ownedBySession {
			server.releaseBusToken()
		}
		return
	default:
	}

	respCh := make(chan downstream.Frame, 1)
	server.pendingStartMu.Lock()
	server.pendingStart = &pendingStart{
		sessionID: sessionID,
		respCh:    respCh,
		mode:      pendingStartModeENH,
		initiator: initiator,
	}
	server.pendingStartMu.Unlock()

	startFrame := downstream.Frame{
		Command: byte(southboundenh.ENHReqStart),
		Payload: []byte{initiator},
	}
	if err := server.upstream.WriteFrame(startFrame); err != nil {
		server.clearPendingStart(sessionID)
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		if !ownedBySession {
			server.releaseBusToken()
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	select {
	case response := <-respCh:
		server.clearPendingStart(sessionID)
		if server.cfg.Debug {
			log.Printf(
				"session=%d start_resp cmd=0x%02X data=0x%02X",
				sessionID,
				response.Command,
				response.Payload[0],
			)
		}
		switch southboundenh.ENHCommand(response.Command) {
		case southboundenh.ENHResStarted:
			server.mutex.Lock()
			server.busOwner = sessionID
			server.busDirty = true
			server.busOwned = time.Now().UTC()
			server.mutex.Unlock()
			server.clearSessionCollision(sessionID)
			return
		default:
			if southboundenh.ENHCommand(response.Command) == southboundenh.ENHResFailed {
				if len(response.Payload) == 1 {
					server.markSessionCollision(sessionID, response.Payload[0])
				} else {
					server.markSessionCollision(sessionID, 0x00)
				}
			}
			if !ownedBySession {
				server.releaseLease(sessionID)
			}
			if southboundenh.ENHCommand(response.Command) == southboundenh.ENHResFailed ||
				southboundenh.ENHCommand(response.Command) == southboundenh.ENHResErrorEBUS ||
				southboundenh.ENHCommand(response.Command) == southboundenh.ENHResErrorHost {
				server.releaseBusIfOwner(sessionID)
			}
			if !ownedBySession {
				server.releaseBusToken()
			}
			return
		}
	case <-time.After(5 * time.Second):
		server.clearPendingStart(sessionID)
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		server.releaseBusIfOwner(sessionID)
		if !ownedBySession {
			server.releaseBusToken()
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	case <-ctx.Done():
		server.clearPendingStart(sessionID)
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		server.releaseBusIfOwner(sessionID)
		if !ownedBySession {
			server.releaseBusToken()
		}
		return
	}
}

func (server *Server) handleStartCancel(sessionID uint64) {
	server.pendingStartMu.Lock()
	pending := server.pendingStart
	if pending != nil && pending.sessionID == sessionID {
		server.pendingStart = nil
	}
	server.pendingStartMu.Unlock()

	server.releaseBusIfOwner(sessionID)
	server.releaseLease(sessionID)
}

func (server *Server) handleStartUDPPlain(ctx context.Context, sessionID uint64, initiator byte) {
	server.mutex.Lock()
	sess := server.sessions[sessionID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}

	ownedBySession := func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		return server.busOwner == sessionID
	}()

	if ownedBySession {
		server.clearSessionCollision(sessionID)
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResStarted),
			Payload: []byte{initiator},
		})
		return
	}

	waitStart := time.Now()
	select {
	case <-server.busToken:
	case <-ctx.Done():
		return
	case <-sess.done:
		return
	}
	if server.cfg.Debug {
		log.Printf("session=%d start_wait=%s", sessionID, time.Since(waitStart))
	}

	defer func() {
		server.mutex.Lock()
		owner := server.busOwner
		server.mutex.Unlock()
		if owner != sessionID {
			server.releaseLease(sessionID)
			server.releaseBusToken()
		}
	}()

	for attempt := 0; attempt < udpPlainMaxAttempts; attempt++ {
		server.clearSynSignal()

		waitedForSyn, ok := server.waitForUDPPlainIdleSyn(ctx, sess, sessionID, attempt+1)
		if !ok {
			return
		}
		if server.cfg.Debug && waitedForSyn {
			log.Printf("session=%d attempt=%d udp_plain_syn_acquired=true", sessionID, attempt+1)
		}

		respCh := make(chan downstream.Frame, 1)
		server.pendingStartMu.Lock()
		server.pendingStart = &pendingStart{
			sessionID: sessionID,
			respCh:    respCh,
			mode:      pendingStartModeUDPPlain,
			initiator: initiator,
		}
		server.pendingStartMu.Unlock()

		server.logWireTX(initiator)
		if err := server.upstream.WriteFrame(downstream.Frame{
			Command: byte(southboundenh.ENHReqSend),
			Payload: []byte{initiator},
		}); err != nil {
			server.clearPendingStart(sessionID)
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResErrorHost),
				Payload: []byte{0x00},
			})
			return
		}

		select {
		case response := <-respCh:
			server.clearPendingStart(sessionID)
			switch southboundenh.ENHCommand(response.Command) {
			case southboundenh.ENHResStarted:
				server.mutex.Lock()
				server.busOwner = sessionID
				server.busDirty = true
				server.busOwned = time.Now().UTC()
				server.mutex.Unlock()
				server.clearSessionCollision(sessionID)
				server.reply(sessionID, downstream.Frame{
					Command: byte(southboundenh.ENHResStarted),
					Payload: []byte{initiator},
				})
				return
			case southboundenh.ENHResFailed:
				if attempt+1 >= udpPlainMaxAttempts {
					if len(response.Payload) == 1 {
						server.markSessionCollision(sessionID, response.Payload[0])
					} else {
						server.markSessionCollision(sessionID, 0x00)
					}
					server.reply(sessionID, downstream.Frame{
						Command: byte(southboundenh.ENHResFailed),
						Payload: append([]byte(nil), response.Payload...),
					})
					return
				}
				if server.cfg.Debug && len(response.Payload) == 1 {
					log.Printf(
						"session=%d attempt=%d arbitration_failed=0x%02X retrying=true",
						sessionID,
						attempt+1,
						response.Payload[0],
					)
				}
				backoff := udpPlainRetryBackoffWithJitter(attempt, server.cfg.UDPPlainRetryJitter, server.randomFloat64)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				case <-sess.done:
					return
				}
				continue
			default:
				return
			}
		case <-time.After(server.cfg.UDPPlainStartWait):
			server.clearPendingStart(sessionID)
			if server.cfg.DisableUDPPlainStartFallback {
				server.releaseLease(sessionID)
				server.reply(sessionID, downstream.Frame{
					Command: byte(southboundenh.ENHResErrorHost),
					Payload: []byte{0x00},
				})
				return
			}
			if server.cfg.Debug {
				log.Printf("session=%d start_timeout_fallback=true", sessionID)
			}
			server.mutex.Lock()
			server.busOwner = sessionID
			server.busDirty = true
			server.busOwned = time.Now().UTC()
			server.mutex.Unlock()
			server.clearSessionCollision(sessionID)
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResStarted),
				Payload: []byte{initiator},
			})
			return
		case <-ctx.Done():
			server.clearPendingStart(sessionID)
			server.releaseLease(sessionID)
			return
		case <-sess.done:
			server.clearPendingStart(sessionID)
			server.releaseLease(sessionID)
			return
		}
	}
}

func (server *Server) handleInfo(sessionID uint64, infoID byte) {
	server.mutex.Lock()
	sess := server.sessions[sessionID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}

	server.pendingInfoMu.Lock()
	server.pendingInfo = &pendingInfo{
		sessionID: sessionID,
		remaining: -1,
	}
	server.pendingInfoMu.Unlock()

	infoFrame := downstream.Frame{
		Command: byte(southboundenh.ENHReqInfo),
		Payload: []byte{infoID},
	}
	if err := server.upstream.WriteFrame(infoFrame); err != nil {
		server.clearPendingInfo(sessionID)
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}
}

func (server *Server) handleSend(sessionID uint64, data byte) {
	server.mutex.Lock()
	owner := server.busOwner
	server.mutex.Unlock()

	if owner != sessionID {
		if server.cfg.Debug {
			log.Printf("session=%d send_rejected owner=%d symbol=0x%02X", sessionID, owner, data)
		}
		if winner, ok := server.takeSessionCollision(sessionID); ok {
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResFailed),
				Payload: []byte{winner},
			})
			return
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	server.mutex.Lock()
	server.busDirty = true
	server.busOwned = time.Now().UTC()
	server.mutex.Unlock()

	sendFrame := downstream.Frame{
		Command: byte(southboundenh.ENHReqSend),
		Payload: []byte{data},
	}
	if server.cfg.Debug {
		log.Printf("session=%d send symbol=0x%02X", sessionID, data)
	}
	server.logWireTX(data)
	if err := server.upstream.WriteFrame(sendFrame); err != nil {
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		server.releaseBusIfOwner(sessionID)
		return
	}

}

func (server *Server) runUDPPlainReader(ctx context.Context) {
	defer server.waitGroup.Done()

	if server.udpListener == nil {
		return
	}

	buffer := make([]byte, udpPlainReadBufferSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := setReadDeadline(server.udpListener, server.cfg.ReadTimeout); err != nil {
			continue
		}

		n, remoteAddr, err := server.udpListener.ReadFromUDP(buffer)
		if err != nil {
			if isTimeoutError(err) {
				continue
			}
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			continue
		}
		if n == 0 || remoteAddr == nil {
			continue
		}

		server.registerUDPPlainClient(remoteAddr)
		payload := append([]byte(nil), buffer[:n]...)
		if server.cfg.Debug {
			log.Printf(
				"udp_plain_rx client=%s len=%d first=0x%02X",
				remoteAddr.String(),
				len(payload),
				payload[0],
			)
		}

		select {
		case server.udpQueue <- udpDatagram{payload: payload}:
		default:
			if server.cfg.Debug {
				log.Printf("udp_plain_queue_full dropped_datagram_len=%d", len(payload))
			}
		}
	}
}

func (server *Server) runUDPPlainWriter(ctx context.Context) {
	defer server.waitGroup.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case datagram := <-server.udpQueue:
			if len(datagram.payload) == 0 {
				continue
			}
			if err := server.forwardUDPPlainDatagram(ctx, datagram.payload); err != nil {
				if server.cfg.Debug {
					log.Printf("udp_plain_forward_failed len=%d err=%v", len(datagram.payload), err)
				}
			}
		}
	}
}

func (server *Server) forwardUDPPlainDatagram(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if server.cfg.Debug {
		log.Printf(
			"udp_plain_forward begin len=%d first=0x%02X wire_plain_upstream=%t",
			len(payload),
			payload[0],
			server.isWirePlainUpstream(),
		)
	}

	select {
	case <-server.busToken:
	case <-ctx.Done():
		return ctx.Err()
	}

	releaseToken := true
	defer func() {
		if releaseToken {
			server.releaseBusToken()
		}
	}()

	if !server.isWirePlainUpstream() {
		initiator := payload[0]
		if err := server.startUDPPlainBridge(initiator); err != nil {
			return err
		}
		server.mutex.Lock()
		server.busOwner = udpBridgeOwnerID
		server.busDirty = true
		server.busOwned = time.Now().UTC()
		server.mutex.Unlock()
		releaseToken = false
		payload = payload[1:]
		if len(payload) == 0 {
			return nil
		}
	}

	for _, symbol := range payload {
		server.logWireTX(symbol)
		if err := server.upstream.WriteFrame(downstream.Frame{
			Command: byte(southboundenh.ENHReqSend),
			Payload: []byte{symbol},
		}); err != nil {
			if !releaseToken {
				server.releaseBusIfOwner(udpBridgeOwnerID)
				releaseToken = true
			}
			return err
		}
	}
	if server.cfg.Debug {
		log.Printf("udp_plain_forward done len=%d", len(payload))
	}
	return nil
}

func (server *Server) registerUDPPlainClient(remoteAddr *net.UDPAddr) {
	if remoteAddr == nil {
		return
	}
	clientAddress := remoteAddr.String()

	server.udpClientsMu.Lock()
	server.udpClients[clientAddress] = remoteAddr
	server.udpClientsMu.Unlock()
}

func (server *Server) startUDPPlainBridge(initiator byte) error {
	respCh := make(chan downstream.Frame, 1)

	server.pendingStartMu.Lock()
	if server.pendingStart != nil {
		server.pendingStartMu.Unlock()
		return fmt.Errorf("upstream start already pending")
	}
	server.pendingStart = &pendingStart{
		sessionID: 0,
		respCh:    respCh,
		mode:      pendingStartModeUDPPlain,
		initiator: initiator,
	}
	server.pendingStartMu.Unlock()

	if err := server.upstream.WriteFrame(downstream.Frame{
		Command: byte(southboundenh.ENHReqStart),
		Payload: []byte{initiator},
	}); err != nil {
		server.clearPendingStart(0)
		return err
	}

	select {
	case response := <-respCh:
		command := southboundenh.ENHCommand(response.Command)
		switch command {
		case southboundenh.ENHResStarted:
			return nil
		case southboundenh.ENHResFailed:
			if len(response.Payload) == 1 {
				return fmt.Errorf("upstream start failed (winner=0x%02X)", response.Payload[0])
			}
			return fmt.Errorf("upstream start failed")
		default:
			return fmt.Errorf("upstream start unexpected response 0x%02X", response.Command)
		}
	case <-time.After(server.cfg.UDPPlainStartWait):
		server.clearPendingStart(0)
		if !server.cfg.DisableUDPPlainStartFallback {
			if server.cfg.Debug {
				log.Printf("udp_plain_bridge_start_timeout_fallback=true initiator=0x%02X", initiator)
			}
			return nil
		}
		return fmt.Errorf("upstream start timeout")
	}
}

func (server *Server) runUpstreamReader(ctx context.Context) {
	defer server.waitGroup.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		frame, err := server.upstream.ReadFrame()
		if err != nil {
			if isTimeoutError(err) {
				continue
			}
			if errors.Is(err, io.EOF) || isClosedNetworkError(err) {
				return
			}
			continue
		}

		switch southboundenh.ENHCommand(frame.Command) {
		case southboundenh.ENHResReceived, southboundenh.ENHResResetted:
			if southboundenh.ENHCommand(frame.Command) == southboundenh.ENHResResetted && len(frame.Payload) == 1 {
				server.upstreamFeatures.Store(uint32(frame.Payload[0]))
			}
			if southboundenh.ENHCommand(frame.Command) == southboundenh.ENHResReceived && len(frame.Payload) == 1 {
				server.lastWireRXAtNano.Store(time.Now().UTC().UnixNano())
				if server.cfg.Debug {
					log.Printf("wire_rx symbol=0x%02X", frame.Payload[0])
				}
				server.logWireRX(frame.Payload[0])
				server.broadcastUDPPlainByte(frame.Payload[0])
				server.noteObservedInitiatorByte(frame.Payload[0])
				server.noteBusWireSymbol(frame.Payload[0])
				if frame.Payload[0] == ebusSyn {
					select {
					case server.synCh <- struct{}{}:
					default:
					}
				}

				if server.shouldUseWireArbitrationResult() && server.isStartPending() && server.deliverPendingStartFromArbByte(frame.Payload[0]) {
					continue
				}
			}
			server.broadcast(frame)
		case southboundenh.ENHResInfo:
			if server.deliverPendingInfo(frame) {
				continue
			}
		case southboundenh.ENHResErrorEBUS, southboundenh.ENHResErrorHost:
			if server.deliverPendingStart(frame) {
				continue
			}
			server.deliverUpstreamError(frame)
		case southboundenh.ENHResStarted, southboundenh.ENHResFailed:
			if server.deliverPendingStart(frame) {
				continue
			}
			if southboundenh.ENHCommand(frame.Command) == southboundenh.ENHResFailed {
				server.deliverUpstreamFailed(frame)
			}
		default:
		}
	}
}

func (server *Server) waitForUDPPlainIdleSyn(
	ctx context.Context,
	sess *session,
	sessionID uint64,
	attempt int,
) (bool, bool) {
	waitTimeout := udpPlainSynWait
	bootstrapMode := !server.hasRecentWireRX(udpPlainSynWait)
	if bootstrapMode {
		waitTimeout = udpPlainBootstrapWait
	}

	synWait := time.Now()
	select {
	case <-server.synCh:
		if server.cfg.Debug {
			log.Printf("session=%d attempt=%d syn_wait=%s", sessionID, attempt, time.Since(synWait))
		}
		return true, true
	case <-time.After(waitTimeout):
		if bootstrapMode {
			if server.cfg.Debug {
				log.Printf(
					"session=%d attempt=%d syn_wait_bootstrap_timeout=%s proceeding_without_syn=true",
					sessionID,
					attempt,
					waitTimeout,
				)
			}
			return false, true
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return true, false
	case <-ctx.Done():
		return false, false
	case <-sess.done:
		return false, false
	}
}

func (server *Server) hasRecentWireRX(window time.Duration) bool {
	if window <= 0 {
		return false
	}

	lastSeen := server.lastWireRXAtNano.Load()
	if lastSeen <= 0 {
		return false
	}

	return time.Since(time.Unix(0, lastSeen)) <= window
}

func (server *Server) deliverPendingStart(frame downstream.Frame) bool {
	server.pendingStartMu.Lock()
	pending := server.pendingStart
	if pending == nil {
		server.pendingStartMu.Unlock()
		return false
	}
	// Clear pending immediately once a START result frame is consumed so that
	// subsequent wire bytes are not dropped by the "start pending" fast-path.
	server.pendingStart = nil
	server.pendingStartMu.Unlock()

	frameData := byte(0x00)
	if len(frame.Payload) > 0 {
		frameData = frame.Payload[0]
	}
	if server.cfg.Debug {
		log.Printf(
			"session=%d upstream_start_result cmd=0x%02X data=0x%02X",
			pending.sessionID,
			frame.Command,
			frameData,
		)
	}

	forwarded := cloneFrame(frame)
	if pending.mode == pendingStartModeENH &&
		southboundenh.ENHCommand(forwarded.Command) == southboundenh.ENHResStarted &&
		frameData != pending.initiator {
		forwarded.Command = byte(southboundenh.ENHResFailed)
		forwarded.Payload = []byte{frameData}
	}

	select {
	case pending.respCh <- cloneFrame(forwarded):
	default:
	}

	if pending.mode == pendingStartModeUDPPlain {
		return true
	}

	// Preserve upstream ordering for ENH upstream mode: enqueue the response
	// immediately on the owning session. The waiting START handler still uses
	// respCh for internal ownership state updates.
	server.reply(pending.sessionID, forwarded)

	return true
}

func (server *Server) isStartPending() bool {
	server.pendingStartMu.Lock()
	defer server.pendingStartMu.Unlock()
	return server.pendingStart != nil
}

func (server *Server) shouldUseWireArbitrationResult() bool {
	return server.isWirePlainUpstream()
}

func (server *Server) isWirePlainUpstream() bool {
	switch server.cfg.UpstreamTransport {
	case UpstreamUDPPlain, UpstreamTCPPlain:
		return true
	default:
		return false
	}
}

func (server *Server) clearSynSignal() {
	for {
		select {
		case <-server.synCh:
		default:
			return
		}
	}
}

func udpPlainRetryBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := udpPlainBackoffBase << attempt
	if delay > udpPlainBackoffMax {
		return udpPlainBackoffMax
	}
	return delay
}

func udpPlainRetryBackoffWithJitter(
	attempt int,
	jitterFactor float64,
	randomFloat64 func() float64,
) time.Duration {
	backoff := udpPlainRetryBackoff(attempt)
	if jitterFactor <= 0 {
		return backoff
	}
	if jitterFactor > 1 {
		jitterFactor = 1
	}
	if randomFloat64 == nil {
		randomFloat64 = rand.Float64
	}

	// Uniform jitter in [-jitterFactor, +jitterFactor].
	jitter := (randomFloat64()*2 - 1) * jitterFactor
	jittered := time.Duration(float64(backoff) * (1 + jitter))
	if jittered <= 0 {
		jittered = time.Millisecond
	}
	if jittered > udpPlainBackoffMax {
		return udpPlainBackoffMax
	}
	return jittered
}

func (server *Server) logWireRX(value byte) {
	if server.wireLog == nil {
		return
	}
	server.wireLog.LogLine("RX %02X", value)
}

func (server *Server) logWireTX(value byte) {
	if server.wireLog == nil {
		return
	}
	server.wireLog.LogLine("TX %02X", value)
}

func (server *Server) deliverPendingStartFromArbByte(byteValue byte) bool {
	if byteValue == ebusSyn {
		return false
	}

	server.pendingStartMu.Lock()
	pending := server.pendingStart
	allowFromWire := false
	if pending != nil {
		allowFromWire = pending.mode == pendingStartModeUDPPlain
	}
	if pending == nil || !allowFromWire || pending.delivered {
		server.pendingStartMu.Unlock()
		return false
	}
	pending.delivered = true
	initiator := pending.initiator
	server.pendingStartMu.Unlock()

	result := downstream.Frame{
		Command: byte(southboundenh.ENHResFailed),
		Payload: []byte{byteValue},
	}
	if byteValue == initiator {
		result.Command = byte(southboundenh.ENHResStarted)
		result.Payload = []byte{initiator}
	}

	return server.deliverPendingStart(result)
}

func (server *Server) deliverPendingInfo(frame downstream.Frame) bool {
	server.pendingInfoMu.Lock()
	pending := server.pendingInfo
	server.pendingInfoMu.Unlock()
	if pending == nil {
		return false
	}

	server.mutex.Lock()
	sess := server.sessions[pending.sessionID]
	server.mutex.Unlock()
	if sess == nil {
		server.clearPendingInfo(pending.sessionID)
		return false
	}

	server.reply(pending.sessionID, frame)

	server.pendingInfoMu.Lock()
	defer server.pendingInfoMu.Unlock()
	if server.pendingInfo == nil || server.pendingInfo.sessionID != pending.sessionID {
		return true
	}

	if len(frame.Payload) != 1 {
		return true
	}

	if server.pendingInfo.remaining < 0 {
		server.pendingInfo.remaining = int(frame.Payload[0])
		if server.pendingInfo.remaining <= 0 {
			server.pendingInfo = nil
		}
		return true
	}

	server.pendingInfo.remaining--
	if server.pendingInfo.remaining <= 0 {
		server.pendingInfo = nil
	}
	return true
}

func (server *Server) clearPendingStart(sessionID uint64) {
	server.pendingStartMu.Lock()
	if server.pendingStart != nil && server.pendingStart.sessionID == sessionID {
		server.pendingStart = nil
	}
	server.pendingStartMu.Unlock()
}

func (server *Server) clearPendingInfo(sessionID uint64) {
	server.pendingInfoMu.Lock()
	if server.pendingInfo != nil && server.pendingInfo.sessionID == sessionID {
		server.pendingInfo = nil
	}
	server.pendingInfoMu.Unlock()
}

func (server *Server) broadcast(frame downstream.Frame) {
	server.mutex.Lock()
	sessions := make([]*session, 0, len(server.sessions))
	for _, sess := range server.sessions {
		sessions = append(sessions, sess)
	}
	server.mutex.Unlock()

	for _, sess := range sessions {
		server.enqueueOrClose(sess, frame, "broadcast")
	}
}

func (server *Server) broadcastUDPPlainByte(value byte) {
	if server.udpListener == nil {
		return
	}

	server.udpClientsMu.RLock()
	clients := make([]*net.UDPAddr, 0, len(server.udpClients))
	for _, clientAddress := range server.udpClients {
		clients = append(clients, clientAddress)
	}
	server.udpClientsMu.RUnlock()

	for _, clientAddress := range clients {
		if clientAddress == nil {
			continue
		}
		_, err := server.udpListener.WriteToUDP([]byte{value}, clientAddress)
		if err != nil {
			server.removeUDPPlainClient(clientAddress.String())
		}
	}
}

func (server *Server) removeUDPPlainClient(clientAddress string) {
	if strings.TrimSpace(clientAddress) == "" {
		return
	}
	server.udpClientsMu.Lock()
	delete(server.udpClients, clientAddress)
	server.udpClientsMu.Unlock()
}

func (server *Server) reply(sessionID uint64, frame downstream.Frame) {
	server.mutex.Lock()
	sess := server.sessions[sessionID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}

	server.enqueueOrClose(sess, frame, "reply")
}

func (server *Server) enqueueOrClose(sess *session, frame downstream.Frame, reason string) {
	select {
	case <-sess.done:
		return
	default:
	}

	if sess.enqueue(frame) {
		return
	}

	dropped := server.backpressureDrops.Add(1)
	closed := server.backpressureCloses.Add(1)
	if server.cfg.Debug {
		log.Printf(
			"session=%d outbound_backpressure reason=%s dropped=%d closed=%d queue_len=%d queue_cap=%d",
			sess.id,
			reason,
			dropped,
			closed,
			len(sess.sendCh),
			cap(sess.sendCh),
		)
	}

	_ = sess.Close()
}

func (server *Server) deliverUpstreamError(frame downstream.Frame) {
	server.mutex.Lock()
	owner := server.busOwner
	server.mutex.Unlock()

	if owner != 0 {
		winner := byte(0x00)
		if len(frame.Payload) == 1 {
			winner = frame.Payload[0]
		}
		server.markSessionCollision(owner, winner)
		server.reply(owner, frame)
		server.releaseBusIfOwner(owner)
		return
	}

	server.broadcast(frame)
}

func (server *Server) deliverUpstreamFailed(frame downstream.Frame) {
	server.mutex.Lock()
	owner := server.busOwner
	server.mutex.Unlock()

	if owner != 0 {
		server.reply(owner, frame)
		server.releaseBusIfOwner(owner)
		return
	}

	server.broadcast(frame)
}

func (server *Server) releaseBusIfOwner(sessionID uint64) {
	server.mutex.Lock()
	if server.busOwner != sessionID {
		server.mutex.Unlock()
		return
	}
	server.busOwner = 0
	server.busDirty = false
	server.busOwned = time.Time{}
	server.mutex.Unlock()

	server.releaseBusToken()
}

func (server *Server) releaseBusIfIdleSyn() {
	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	ownedAt := server.busOwned
	if owner == 0 {
		server.mutex.Unlock()
		return
	}
	if !ownedAt.IsZero() && time.Since(ownedAt) < busIdleReleaseGrace {
		server.mutex.Unlock()
		return
	}
	if dirty {
		// First SYN after activity marks a telegram boundary. Keep ownership
		// until a subsequent idle SYN is observed.
		server.busDirty = false
		server.mutex.Unlock()
		return
	}
	server.mutex.Unlock()

	if server.cfg.Debug {
		log.Printf("session=%d release_reason=idle_syn", owner)
	}

	server.releaseBusIfOwner(owner)
}

func (server *Server) noteBusWireSymbol(symbol byte) {
	if symbol == ebusSyn {
		server.releaseBusIfIdleSyn()
		return
	}

	server.mutex.Lock()
	if server.busOwner != 0 {
		server.busDirty = true
	}
	server.mutex.Unlock()
}

func (server *Server) releaseBusToken() {
	select {
	case server.busToken <- struct{}{}:
	default:
	}
}

func (server *Server) acquireLease(sessionID uint64, initiator byte) (byte, error) {
	if server.leaseManager == nil {
		return initiator, nil
	}

	server.leasesMu.Lock()
	defer server.leasesMu.Unlock()

	if existing, ok := server.leasedBySess[sessionID]; ok {
		if existing.Address == initiator {
			return existing.Address, nil
		}
		return 0, fmt.Errorf(
			"session already leased initiator 0x%02X (requested 0x%02X)",
			existing.Address,
			initiator,
		)
	}

	lease, err := server.leaseManager.Acquire(
		fmt.Sprintf("session/%d", sessionID),
		sourcepolicy.AcquireOptions{
			Candidates:        []uint8{initiator},
			AllowSoftReserved: true,
		},
	)
	if err != nil {
		return 0, err
	}

	server.leasedBySess[sessionID] = lease
	return lease.Address, nil
}

func (server *Server) releaseLease(sessionID uint64) {
	if server.leaseManager == nil {
		return
	}

	server.leasesMu.Lock()
	lease, ok := server.leasedBySess[sessionID]
	if ok {
		delete(server.leasedBySess, sessionID)
	}
	server.leasesMu.Unlock()

	if ok {
		_, _ = server.leaseManager.Release(lease.OwnerID)
	}
}

func (server *Server) sessionLeaseAddress(sessionID uint64) (byte, bool) {
	if server.leaseManager == nil {
		return 0, false
	}

	server.leasesMu.Lock()
	defer server.leasesMu.Unlock()

	lease, ok := server.leasedBySess[sessionID]
	if !ok {
		return 0, false
	}
	return lease.Address, true
}

func (server *Server) noteObservedInitiatorByte(byteValue byte) {
	server.observedMu.Lock()
	defer server.observedMu.Unlock()

	if byteValue == ebusSyn {
		server.startOfTelegram = true
		return
	}
	if !server.startOfTelegram {
		return
	}
	server.startOfTelegram = false
	if !isInitiatorAddress(byteValue) {
		return
	}

	now := time.Now().UTC()
	server.observedInitiatorAt[byteValue] = now
	server.pruneObservedInitiatorsLocked(now.Add(-server.cfg.AutoJoinActivityWindow))
}

func (server *Server) pruneObservedInitiatorsLocked(cutoff time.Time) {
	for address, seenAt := range server.observedInitiatorAt {
		if !seenAt.After(cutoff) {
			delete(server.observedInitiatorAt, address)
		}
	}
}

func (server *Server) selectAutoInitiator() (byte, error) {
	now := time.Now().UTC()
	observedSet := make(map[byte]struct{})

	server.observedMu.Lock()
	server.pruneObservedInitiatorsLocked(now.Add(-server.cfg.AutoJoinActivityWindow))
	for address := range server.observedInitiatorAt {
		observedSet[address] = struct{}{}
	}
	server.observedMu.Unlock()

	leasedSet := make(map[byte]struct{})
	server.leasesMu.Lock()
	for _, lease := range server.leasedBySess {
		leasedSet[lease.Address] = struct{}{}
	}
	server.leasesMu.Unlock()

	server.mutex.Lock()
	previous := server.autoJoinInitiator
	server.mutex.Unlock()
	if previous != 0 {
		if _, observed := observedSet[previous]; !observed {
			if _, leased := leasedSet[previous]; !leased {
				return previous, nil
			}
		}
	}

	for _, candidate := range preferredInitiatorAddresses {
		if _, observed := observedSet[candidate]; observed {
			continue
		}
		if _, leased := leasedSet[candidate]; leased {
			continue
		}
		return candidate, nil
	}
	return 0, fmt.Errorf("no initiator address available for auto join")
}

func (server *Server) markSessionCollision(sessionID uint64, winner byte) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.collisionBySession[sessionID] = winner
}

func (server *Server) clearSessionCollision(sessionID uint64) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	delete(server.collisionBySession, sessionID)
}

func (server *Server) takeSessionCollision(sessionID uint64) (byte, bool) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	winner, ok := server.collisionBySession[sessionID]
	if !ok {
		return 0, false
	}
	delete(server.collisionBySession, sessionID)
	return winner, true
}

func isInitiatorAddress(address byte) bool {
	return initiatorPart(address&0x0F) > 0 && initiatorPart((address&0xF0)>>4) > 0
}

func initiatorPart(bits byte) byte {
	switch bits {
	case 0x0:
		return 1
	case 0x1:
		return 2
	case 0x3:
		return 3
	case 0x7:
		return 4
	case 0xF:
		return 5
	default:
		return 0
	}
}

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed)
}
