package adapterproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/sourcepolicy"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

const (
	defaultLeaseDuration = 30 * time.Minute
	ebusSyn              = byte(0xAA)
)

type Server struct {
	cfg Config

	listener net.Listener
	upstream *upstreamClient

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
}

type pendingInfo struct {
	sessionID uint64
	remaining int
}

func NewServer(cfg Config) *Server {
	server := &Server{
		cfg:          cfg,
		sessions:     make(map[uint64]*session),
		busToken:     make(chan struct{}, 1),
		leasedBySess: make(map[uint64]sourcepolicy.Lease),
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

	upstream, err := dialUpstream(ctx, server.cfg.UpstreamAddr, server.cfg.DialTimeout, server.cfg.ReadTimeout, server.cfg.WriteTimeout)
	if err != nil {
		return fmt.Errorf("dial upstream: %w", err)
	}
	server.upstream = upstream
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
	server.closeSessions()
	server.waitGroup.Wait()
	_ = upstream.Close()

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

	if !server.acquireLease(sessionID, initiator) {
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	if initiator == ebusSyn {
		server.handleStartCancel(sessionID)
		// Forward best-effort cancellation upstream. The enhanced protocol does not
		// mandate a response for START+SYN, so we must not block waiting for one.
		_ = server.upstream.WriteFrame(downstream.Frame{
			Command: byte(southboundenh.ENHReqStart),
			Payload: []byte{initiator},
		})
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
			if southboundenh.ENHCommand(frame.Command) == southboundenh.ENHResReceived && len(frame.Payload) == 1 && frame.Payload[0] == ebusSyn {
				server.releaseBusIfIdleSyn()
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

	// Preserve upstream ordering: enqueue the response immediately on the owning
	// session (mirrors the single-connection behavior of a real adapter). The
	// waiting START handler only uses respCh to update internal ownership state.
	server.reply(pending.sessionID, frame)

	select {
	case pending.respCh <- cloneFrame(frame):
	default:
	}

	return true
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

func (server *Server) acquireLease(sessionID uint64, initiator byte) bool {
	if server.leaseManager == nil {
		return true
	}

	server.leasesMu.Lock()
	defer server.leasesMu.Unlock()

	if existing, ok := server.leasedBySess[sessionID]; ok {
		return existing.Address == initiator
	}

	lease, err := server.leaseManager.Acquire(
		fmt.Sprintf("session/%d", sessionID),
		sourcepolicy.AcquireOptions{
			Candidates:        []uint8{initiator},
			AllowSoftReserved: true,
		},
	)
	if err != nil {
		return false
	}

	server.leasedBySess[sessionID] = lease
	return true
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
