package adapterproxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
	defaultLeaseDuration  = 30 * time.Minute
	ebusSyn               = byte(0xAA)
	udpPlainSynWait       = 5 * time.Second
	udpPlainStartWait     = 5 * time.Second
	udpPlainMaxAttempts   = 4
	udpPlainBackoffBase   = 25 * time.Millisecond
	udpPlainBackoffMax    = 400 * time.Millisecond
	udpNorthboundQueueCap = 1024
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

	backpressureDrops  atomic.Uint64
	backpressureCloses atomic.Uint64

	mutex    sync.Mutex
	sessions map[uint64]*session
	nextID   uint64

	busToken chan struct{}
	busOwner uint64
	busDirty bool

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

func NewServer(cfg Config) *Server {
	server := &Server{
		cfg:          cfg,
		sessions:     make(map[uint64]*session),
		busToken:     make(chan struct{}, 1),
		leasedBySess: make(map[uint64]sourcepolicy.Lease),
		synCh:        make(chan struct{}, 1),
		udpClients:   make(map[string]*net.UDPAddr),
		udpQueue:     make(chan udpDatagram, udpNorthboundQueueCap),
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

	if err := server.acquireLease(sessionID, initiator); err != nil {
		log.Printf(
			"session=%d lease_rejected initiator=0x%02X reason=%v",
			sessionID,
			initiator,
			err,
		)
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	if initiator == ebusSyn {
		server.handleStartCancel(sessionID)
		if server.cfg.UpstreamTransport != UpstreamUDPPlain {
			// Forward best-effort cancellation upstream. The enhanced protocol does not
			// mandate a response for START+SYN, so we must not block waiting for one.
			_ = server.upstream.WriteFrame(downstream.Frame{
				Command: byte(southboundenh.ENHReqStart),
				Payload: []byte{initiator},
			})
		}
		return
	}

	if server.cfg.UpstreamTransport == UpstreamUDPPlain {
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
	} else if server.cfg.Debug {
		log.Printf("session=%d start_reuse_owner=true", sessionID)
	}

	// If we acquired the bus token above, it is now held until SYN (end-of-message)
	// or an error/disconnect. If we are reusing an existing ownership, do not touch
	// the token here.

	select {
	case <-ctx.Done():
		if !ownedBySession {
			server.releaseBusToken()
		}
		return
	case <-sess.done:
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
			server.busDirty = false
			server.mutex.Unlock()
			return
		default:
			if !ownedBySession {
				server.releaseBusToken()
			}
			return
		}
	case <-time.After(5 * time.Second):
		server.clearPendingStart(sessionID)
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
			server.releaseBusToken()
		}
	}()

	for attempt := 0; attempt < udpPlainMaxAttempts; attempt++ {
		server.clearSynSignal()

		synWait := time.Now()
		select {
		case <-server.synCh:
		case <-time.After(udpPlainSynWait):
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResErrorHost),
				Payload: []byte{0x00},
			})
			return
		case <-ctx.Done():
			return
		case <-sess.done:
			return
		}
		if server.cfg.Debug {
			log.Printf("session=%d attempt=%d syn_wait=%s", sessionID, attempt+1, time.Since(synWait))
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
				server.busDirty = false
				server.mutex.Unlock()
				server.reply(sessionID, downstream.Frame{
					Command: byte(southboundenh.ENHResStarted),
					Payload: []byte{initiator},
				})
				return
			case southboundenh.ENHResFailed:
				if attempt+1 >= udpPlainMaxAttempts {
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
				backoff := udpPlainRetryBackoff(attempt)
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
		case <-time.After(udpPlainStartWait):
			server.clearPendingStart(sessionID)
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResErrorHost),
				Payload: []byte{0x00},
			})
			return
		case <-ctx.Done():
			server.clearPendingStart(sessionID)
			return
		case <-sess.done:
			server.clearPendingStart(sessionID)
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
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	server.mutex.Lock()
	server.busDirty = true
	server.mutex.Unlock()

	sendFrame := downstream.Frame{
		Command: byte(southboundenh.ENHReqSend),
		Payload: []byte{data},
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

	// In direct-mode usage, hosts terminate a telegram with SYN (0xAA).
	// Release our multiplexed bus lock at that boundary so other sessions can
	// arbitrate, matching the single-host behavior of a real adapter.
	if data == ebusSyn {
		server.releaseBusIfOwner(sessionID)
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

	select {
	case <-server.busToken:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer server.releaseBusToken()

	for _, symbol := range payload {
		server.logWireTX(symbol)
		if err := server.upstream.WriteFrame(downstream.Frame{
			Command: byte(southboundenh.ENHReqSend),
			Payload: []byte{symbol},
		}); err != nil {
			return err
		}
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
				server.logWireRX(frame.Payload[0])
				server.broadcastUDPPlainByte(frame.Payload[0])
				if frame.Payload[0] == ebusSyn {
					select {
					case server.synCh <- struct{}{}:
					default:
					}
					server.releaseBusIfIdleSyn()
				}

				if server.isStartPending() {
					if server.cfg.UpstreamTransport == UpstreamUDPPlain && server.deliverPendingStartFromArbByte(frame.Payload[0]) {
						continue
					}
					// Match ebusd-style behavior: while START is pending, ignore received bus bytes.
					continue
				}

				if server.cfg.UpstreamTransport == UpstreamUDPPlain && server.deliverPendingStartFromArbByte(frame.Payload[0]) {
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

func (server *Server) deliverPendingStart(frame downstream.Frame) bool {
	server.pendingStartMu.Lock()
	pending := server.pendingStart
	server.pendingStartMu.Unlock()
	if pending == nil {
		return false
	}

	if server.cfg.Debug {
		log.Printf(
			"session=%d upstream_start_result cmd=0x%02X data=0x%02X",
			pending.sessionID,
			frame.Command,
			frame.Payload[0],
		)
	}

	select {
	case pending.respCh <- cloneFrame(frame):
	default:
	}

	if pending.mode == pendingStartModeUDPPlain {
		return true
	}

	// Preserve upstream ordering for ENH upstream mode: enqueue the response
	// immediately on the owning session. The waiting START handler still uses
	// respCh for internal ownership state updates.
	server.reply(pending.sessionID, frame)

	return true
}

func (server *Server) isStartPending() bool {
	server.pendingStartMu.Lock()
	defer server.pendingStartMu.Unlock()
	return server.pendingStart != nil
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
	if pending == nil || pending.mode != pendingStartModeUDPPlain || pending.delivered {
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
	server.mutex.Unlock()

	server.releaseBusToken()
}

func (server *Server) releaseBusIfIdleSyn() {
	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	server.mutex.Unlock()

	if owner == 0 || !dirty {
		return
	}

	if server.cfg.Debug {
		log.Printf("session=%d release_reason=idle_syn", owner)
	}

	server.releaseBusIfOwner(owner)
}

func (server *Server) releaseBusToken() {
	select {
	case server.busToken <- struct{}{}:
	default:
	}
}

func (server *Server) acquireLease(sessionID uint64, initiator byte) error {
	if server.leaseManager == nil {
		return nil
	}

	server.leasesMu.Lock()
	defer server.leasesMu.Unlock()

	if existing, ok := server.leasedBySess[sessionID]; ok {
		if existing.Address == initiator {
			return nil
		}
		return fmt.Errorf(
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
		return err
	}

	server.leasedBySess[sessionID] = lease
	return nil
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

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed)
}
