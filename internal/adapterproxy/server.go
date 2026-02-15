package adapterproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/sourcepolicy"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

const (
	defaultLeaseDuration = 30 * time.Minute
)

type Server struct {
	cfg Config

	listener net.Listener
	upstream *upstreamClient

	mutex    sync.Mutex
	sessions map[uint64]*session
	nextID   uint64

	busToken chan struct{}
	busOwner uint64

	pendingStartMu sync.Mutex
	pendingStart   *pendingStart

	leasesMu     sync.Mutex
	leaseManager *sourcepolicy.LeaseManager
	leasedBySess map[uint64]sourcepolicy.Lease

	waitGroup sync.WaitGroup
}

type pendingStart struct {
	sessionID uint64
	respCh    chan downstream.Frame
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
	if err := server.upstream.SendInit(0x00); err != nil {
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
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResResetted),
			Payload: []byte{data},
		})
	case southboundenh.ENHReqInfo:
		// Respond with a zero-length info payload.
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResInfo),
			Payload: []byte{0x00},
		})
	case southboundenh.ENHReqStart:
		go server.handleStart(ctx, sessionID, data)
	case southboundenh.ENHReqSend:
		server.handleSend(sessionID, data)
	default:
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
	}
}

func (server *Server) handleStart(ctx context.Context, sessionID uint64, initiator byte) {
	if !server.acquireLease(sessionID, initiator) {
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	select {
	case <-server.busToken:
	case <-ctx.Done():
		return
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
		server.releaseBusToken()
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	select {
	case response := <-respCh:
		server.clearPendingStart(sessionID)
		server.reply(sessionID, response)

		switch southboundenh.ENHCommand(response.Command) {
		case southboundenh.ENHResStarted:
			server.mutex.Lock()
			server.busOwner = sessionID
			server.mutex.Unlock()
			return
		default:
			server.releaseBusToken()
			return
		}
	case <-time.After(5 * time.Second):
		server.clearPendingStart(sessionID)
		server.releaseBusToken()
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	case <-ctx.Done():
		server.clearPendingStart(sessionID)
		server.releaseBusToken()
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

	if data == 0xAA {
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
			server.broadcast(frame)
		case southboundenh.ENHResStarted, southboundenh.ENHResFailed,
			southboundenh.ENHResErrorEBUS, southboundenh.ENHResErrorHost:
			if server.deliverPendingStart(frame) {
				continue
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

	select {
	case pending.respCh <- cloneFrame(frame):
	default:
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

func (server *Server) broadcast(frame downstream.Frame) {
	server.mutex.Lock()
	sessions := make([]*session, 0, len(server.sessions))
	for _, sess := range server.sessions {
		sessions = append(sessions, sess)
	}
	server.mutex.Unlock()

	for _, sess := range sessions {
		sess.enqueue(frame)
	}
}

func (server *Server) reply(sessionID uint64, frame downstream.Frame) {
	server.mutex.Lock()
	sess := server.sessions[sessionID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}

	sess.enqueue(frame)
}

func (server *Server) releaseBusIfOwner(sessionID uint64) {
	server.mutex.Lock()
	if server.busOwner != sessionID {
		server.mutex.Unlock()
		return
	}
	server.busOwner = 0
	server.mutex.Unlock()

	server.releaseBusToken()
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
