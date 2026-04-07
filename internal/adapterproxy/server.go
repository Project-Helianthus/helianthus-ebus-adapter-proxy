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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	emutargets "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/emulation/targets"
	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/sourcepolicy"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

// ErrUpstreamLost is returned by Serve when the upstream adapter connection
// drops unexpectedly. Callers should reconnect by creating a new Server.
var ErrUpstreamLost = errors.New("upstream connection lost")

const (
	defaultLeaseDuration     = 30 * time.Minute
	ebusSyn                  = byte(0xAA)
	ebusACK                  = byte(0x00)
	ebusNACK                 = byte(0xFF)
	udpBridgeOwnerID         = ^uint64(0)
	busIdleReleaseGrace      = 50 * time.Millisecond
	startStaleAbsorbWindow   = 50 * time.Millisecond
	maxOwnershipDuration     = 2 * time.Second
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

	upstreamFeatures    atomic.Uint32
	reinitGuard         chan struct{} // buffered(1), limits re-INIT to one in-flight
	expectingInitResp   atomic.Bool  // true when we sent INIT and expect RESETTED response
	lastWireRXAtNano    atomic.Int64

	backpressureDrops   atomic.Uint64
	backpressureCloses  atomic.Uint64
	staleStartAbsorbed  atomic.Uint64
	staleStartExpired   atomic.Uint64
	synWaitCmdAckTO     atomic.Uint64
	synWaitResponseTO   atomic.Uint64
	lateResponderReject atomic.Uint64

	randomFloat64 func() float64

	mutex    sync.Mutex
	sessions map[uint64]*session
	nextID   uint64

	autoJoinInitiator byte
	startOfTelegram   bool

	observedMu              sync.Mutex
	observedInitiatorAt     map[byte]time.Time
	collisionBySession      map[uint64]byte
	learnedBySession        map[uint64]sessionInitiatorLearning
	localRespondersByTarget map[byte]targetResponderAssociation

	busToken              chan struct{}
	busOwner              uint64
	busOwnerInitiator     byte
	ownerObserverAtStart  bool
	ownerObserverExpected []byte
	ownerObserverSeen     []byte
	busDirty              bool
	busOwned              time.Time
	busWirePhase          busWirePhase
	requestBytesSeen      int
	requestDataLength     int
	requestSrc            byte
	requestDst            byte
	requestPB             byte
	requestSB             byte
	requestLEN            byte
	requestHeaderCaptured bool
	responseBytesRemain   int
	targetResponderWindow targetResponderWindow
	startArbSeq           uint64
	startArbGrantSession  uint64
	startArbContenders    map[uint64]*startArbContender

	pendingStartMu sync.Mutex
	pendingStart   *pendingStart

	pendingInfoMu sync.Mutex
	pendingInfo   *pendingInfo
	infoCache     *adapterInfoCache

	leasesMu     sync.Mutex
	leaseManager *sourcepolicy.LeaseManager
	leasedBySess map[uint64]sourcepolicy.Lease

	upstreamLost chan struct{}

	waitGroup sync.WaitGroup
}

type pendingStart struct {
	sessionID     uint64
	respCh        chan downstream.Frame
	mode          pendingStartMode
	initiator     byte
	delivered     bool
	staleObserved bool
	staleWinner   byte
	staleDeadline time.Time
}

type pendingInfo struct {
	sessionID uint64
	remaining int
	infoID    byte
	frames    []downstream.Frame // accumulated response frames for caching
}

type startArbContender struct {
	sessionID uint64
	initiator byte
	seq       uint64
	grantCh   chan struct{}
}

type sessionInitiatorLearning struct {
	Initiator byte
	LearnedAt time.Time
	Source    string
}

// SessionInitiatorMapping provides an admin/status snapshot entry for learned
// initiator identity per connected session.
type SessionInitiatorMapping struct {
	SessionID uint64
	Initiator byte
	LearnedAt time.Time
	Source    string
}

type pendingStartMode uint8

const (
	pendingStartModeENH pendingStartMode = iota
	pendingStartModeUDPPlain
)

type busWirePhase uint8

const (
	busWirePhaseIdle busWirePhase = iota
	busWirePhaseCollectRequest
	busWirePhaseWaitCmdAck
	busWirePhaseWaitResponseLen
	busWirePhaseWaitResponseBody
	busWirePhaseWaitResponseAck
)

type targetResponderMode uint8

const (
	targetResponderModeLocal targetResponderMode = iota
	targetResponderModeChildExperimental
)

func (mode targetResponderMode) String() string {
	switch mode {
	case targetResponderModeChildExperimental:
		return "child_experimental"
	default:
		return "local"
	}
}

type targetResponderAssociation struct {
	targetAddress byte
	sessionID     uint64
	mode          targetResponderMode
}

type targetResponderWindow struct {
	open               bool
	targetAddress      byte
	ownerSessionID     uint64
	responderSessionID uint64
	mode               targetResponderMode
	openedAt           time.Time
}

func (phase busWirePhase) String() string {
	switch phase {
	case busWirePhaseCollectRequest:
		return "collect_request"
	case busWirePhaseWaitCmdAck:
		return "wait_cmd_ack"
	case busWirePhaseWaitResponseLen:
		return "wait_response_len"
	case busWirePhaseWaitResponseBody:
		return "wait_response_body"
	case busWirePhaseWaitResponseAck:
		return "wait_response_ack"
	default:
		return "idle"
	}
}

func (phase busWirePhase) isSynTimeoutBoundary() bool {
	switch phase {
	case busWirePhaseWaitCmdAck, busWirePhaseWaitResponseBody, busWirePhaseWaitResponseAck:
		return true
	default:
		return false
	}
}

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
		cfg:                     cfg,
		sessions:                make(map[uint64]*session),
		busToken:                make(chan struct{}, 1),
		leasedBySess:            make(map[uint64]sourcepolicy.Lease),
		synCh:                   make(chan struct{}, 1),
		udpClients:              make(map[string]*net.UDPAddr),
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
		upstreamLost:            make(chan struct{}),
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
	server.expectingInitResp.Store(true)
	if err := server.upstream.SendInit(0x01); err != nil {
		server.expectingInitResp.Store(false)
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

	// Monitor upstream loss: close listener to unblock Accept.
	go func() {
		select {
		case <-server.upstreamLost:
			_ = listener.Close()
		case <-ctx.Done():
		}
	}()

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

	if upstreamDied {
		return ErrUpstreamLost
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
	delete(server.learnedBySession, sessionID)
	server.clearLocalResponderAssociationsForSessionLocked(sessionID)
	if server.targetResponderWindow.open && server.targetResponderWindow.responderSessionID == sessionID {
		server.targetResponderWindow = targetResponderWindow{}
	}
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
		log.Printf("session=%d frame_dropped cmd=0x%02X payload_len=%d", sessionID, frame.Command, len(frame.Payload))
		return
	}
	data := frame.Payload[0]

	if sessionID != 2 { // Log non-gateway frames
		log.Printf("session=%d frame cmd=0x%02X data=0x%02X", sessionID, frame.Command, data)
	}

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

	log.Printf("session=%d handle_start initiator=0x%02X", sessionID, initiator)

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
		if learnedAddress, ok := server.sessionLearnedInitiator(sessionID); ok {
			initiator = learnedAddress
			if server.cfg.Debug {
				log.Printf("session=%d auto_join_reuse_learned=0x%02X", sessionID, learnedAddress)
			}
		} else if leasedAddress, ok := server.sessionLeaseAddress(sessionID); ok {
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
		if server.busOwner != sessionID {
			return false
		}
		// Prevent indefinite ownership chaining: force token re-acquisition
		// after maxOwnershipDuration so other sessions get a fair chance.
		if !server.busOwned.IsZero() && time.Since(server.busOwned) > maxOwnershipDuration {
			return false
		}
		return true
	}()

	if !ownedBySession {
		// If we were the owner but exceeded maxOwnershipDuration, release first.
		server.releaseBusIfOwner(sessionID)

		waitStart := time.Now()
		if !server.waitForStartArbitration(ctx, sess, sessionID, initiator) {
			return
		}

		if server.cfg.Debug {
			log.Printf("session=%d start_wait=%s", sessionID, time.Since(waitStart))
		}
	} else {
		server.mutex.Lock()
		if server.busOwner == sessionID {
			server.busDirty = true
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
			server.setBusOwner(sessionID, initiator)
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
				server.setBusOwner(sessionID, initiator)
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
			server.setBusOwner(sessionID, initiator)
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

	// Serve identity IDs from cache if available.
	if cached := server.infoCache.get(infoID); cached != nil {
		for _, f := range cached {
			server.reply(sessionID, f)
		}
		return
	}

	server.pendingInfoMu.Lock()
	server.pendingInfo = &pendingInfo{
		sessionID: sessionID,
		remaining: -1,
		infoID:    infoID,
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
	allowResponderSend, lateResponderSend, lateTarget, lateReason := server.evaluateTargetResponderSendLocked(sessionID)
	server.mutex.Unlock()

	if owner != sessionID && !allowResponderSend {
		if lateResponderSend {
			server.lateResponderReject.Add(1)
			log.Printf(
				"session=%d target_responder_late_reject target=0x%02X reason=%s",
				sessionID,
				lateTarget,
				lateReason,
			)
		}
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

	queuedObserverFrames := false
	if owner == sessionID {
		queuedObserverFrames = server.queueOwnerObserverReplay(sessionID, data)
	}

	server.mutex.Lock()
	server.busDirty = true
	// busOwned is only set in setBusOwner — not reset per SEND byte.
	// This ensures maxOwnershipDuration and busIdleReleaseGrace measure
	// from initial ownership, preventing indefinite chaining.
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
		if queuedObserverFrames {
			server.rollbackOwnerObserverReplay(sessionID)
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		server.releaseBusIfOwner(sessionID)
		return
	}

	if allowResponderSend {
		if server.cfg.Debug {
			log.Printf("session=%d target_responder_send_accepted target=0x%02X symbol=0x%02X", sessionID, lateTarget, data)
		}
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
		if err := server.startUDPPlainBridge(ctx, initiator); err != nil {
			return err
		}
		server.setBusOwner(udpBridgeOwnerID, initiator)
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

func (server *Server) startUDPPlainBridge(ctx context.Context, initiator byte) error {
	respCh := make(chan downstream.Frame, 1)
	startMode := pendingStartModeENH
	if server.isWirePlainUpstream() {
		startMode = pendingStartModeUDPPlain
	}

	server.pendingStartMu.Lock()
	if server.pendingStart != nil {
		server.pendingStartMu.Unlock()
		return fmt.Errorf("upstream start already pending")
	}
	server.pendingStart = &pendingStart{
		sessionID: 0,
		respCh:    respCh,
		mode:      startMode,
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
	case <-ctx.Done():
		server.clearPendingStart(0)
		return ctx.Err()
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
				log.Printf("upstream_connection_lost error=%q", err)
				observerFrames, skipPendingID := server.takeObserverReplayForAbort()
				if len(observerFrames) > 0 {
					server.broadcastObserverFrames(observerFrames, skipPendingID)
				}
				select {
				case server.upstreamLost <- struct{}{}:
				default:
				}
				return
			}
			continue
		}

		switch southboundenh.ENHCommand(frame.Command) {
		case southboundenh.ENHResResetted:
			features := byte(0x00)
			if len(frame.Payload) == 1 {
				features = frame.Payload[0]
				server.upstreamFeatures.Store(uint32(features))
			}
			server.infoCache.invalidateAll()
			log.Printf("upstream_resetted features=0x%02X", features)

			// Abort pending START — adapter reset means arbitration is void.
			// Use ErrorHost (not FAILED) to avoid false collision marking in handleStart.
			server.pendingStartMu.Lock()
			if ps := server.pendingStart; ps != nil {
				server.pendingStart = nil
				server.pendingStartMu.Unlock()
				log.Printf("session=%d resetted_abort_pending_start initiator=0x%02X", ps.sessionID, ps.initiator)
				abortFrame := downstream.Frame{
					Command: byte(southboundenh.ENHResErrorHost),
					Payload: []byte{0x00},
				}
				select {
				case ps.respCh <- cloneFrame(abortFrame):
				default:
				}
				server.reply(ps.sessionID, abortFrame)
			} else {
				server.pendingStartMu.Unlock()
			}

			// Abort pending INFO — adapter reset invalidates in-flight info.
			server.pendingInfoMu.Lock()
			if pi := server.pendingInfo; pi != nil {
				server.pendingInfo = nil
				server.pendingInfoMu.Unlock()
				log.Printf("session=%d resetted_abort_pending_info infoID=0x%02X", pi.sessionID, pi.infoID)
				server.reply(pi.sessionID, downstream.Frame{
					Command: byte(southboundenh.ENHResErrorHost),
					Payload: []byte{0x00},
				})
			} else {
				server.pendingInfoMu.Unlock()
			}

			// Release bus if owned — adapter reset invalidates bus ownership.
			if owner := server.currentBusOwner(); owner != 0 {
				log.Printf("session=%d resetted_release_bus_owner", owner)
				server.releaseBusIfOwner(owner)
			}

			// Re-INIT upstream — adapter needs fresh handshake after reset.
			// Skip if this RESETTED is itself a response to our INIT (avoids
			// INIT→RESETTED→INIT feedback loop). Guard limits concurrency.
			if server.expectingInitResp.CompareAndSwap(true, false) {
				log.Printf("resetted_is_init_response reinit_skipped=true")
			} else {
				select {
				case server.reinitGuard <- struct{}{}:
					server.expectingInitResp.Store(true)
					go func() {
						defer func() { <-server.reinitGuard }()
						if err := server.upstream.SendInit(0x01); err != nil {
							server.expectingInitResp.Store(false)
							log.Printf("resetted_reinit_failed error=%q", err)
						}
					}()
				default:
					log.Printf("resetted_reinit_skipped already_in_flight=true")
				}
			}

			server.broadcast(frame)
		case southboundenh.ENHResReceived:
			if len(frame.Payload) == 1 {
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

				observerFrames, skipPendingID, suppressObservers := server.takeObserverReplayForReceived(frame.Payload[0])
				if len(observerFrames) > 0 {
					server.broadcastObserverFrames(observerFrames, skipPendingID)
				}
				if suppressObservers {
					server.broadcastReceivedToOwner(frame, server.currentBusOwner())
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

	frameData := byte(0x00)
	if len(frame.Payload) > 0 {
		frameData = frame.Payload[0]
	}
	command := southboundenh.ENHCommand(frame.Command)
	now := time.Now()

	if pending.mode == pendingStartModeENH &&
		command == southboundenh.ENHResStarted &&
		frameData != pending.initiator {
		if !pending.staleObserved {
			pending.staleObserved = true
			pending.staleWinner = frameData
			pending.staleDeadline = now.Add(startStaleAbsorbWindow)
			server.pendingStartMu.Unlock()
			log.Printf(
				"session=%d start_stale_absorb_wait requested=0x%02X adapter_won=0x%02X window=%s",
				pending.sessionID,
				pending.initiator,
				frameData,
				startStaleAbsorbWindow,
			)
			server.schedulePendingStartStaleExpiry(pending)
			return true
		}

		pending.staleWinner = frameData
		if now.Before(pending.staleDeadline) {
			remaining := time.Until(pending.staleDeadline)
			server.pendingStartMu.Unlock()
			log.Printf(
				"session=%d start_stale_absorb_wait requested=0x%02X adapter_won=0x%02X remaining=%s",
				pending.sessionID,
				pending.initiator,
				frameData,
				remaining,
			)
			return true
		}

		server.pendingStart = nil
		server.pendingStartMu.Unlock()
		log.Printf(
			"session=%d start_stale_absorb_expired requested=0x%02X adapter_won=0x%02X window=%s -> converting STARTED to FAILED",
			pending.sessionID,
			pending.initiator,
			frameData,
			startStaleAbsorbWindow,
		)
		server.staleStartExpired.Add(1)

		forwarded := downstream.Frame{
			Command: byte(southboundenh.ENHResFailed),
			Payload: []byte{frameData},
		}
		select {
		case pending.respCh <- cloneFrame(forwarded):
		default:
		}
		server.reply(pending.sessionID, forwarded)
		return true
	}

	// Clear pending once a terminal START result frame is consumed so that
	// subsequent wire bytes are not dropped by the "start pending" fast-path.
	server.pendingStart = nil
	hadStaleAbsorb := pending.mode == pendingStartModeENH &&
		pending.staleObserved &&
		command == southboundenh.ENHResStarted &&
		frameData == pending.initiator
	staleWinner := pending.staleWinner
	server.pendingStartMu.Unlock()

	log.Printf(
		"session=%d upstream_start_result cmd=0x%02X data=0x%02X initiator=0x%02X",
		pending.sessionID,
		frame.Command,
		frameData,
		pending.initiator,
	)

	if hadStaleAbsorb {
		server.staleStartAbsorbed.Add(1)
		log.Printf(
			"session=%d start_stale_absorbed requested=0x%02X adapter_won=0x%02X",
			pending.sessionID,
			pending.initiator,
			staleWinner,
		)
	}

	forwarded := cloneFrame(frame)
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

func (server *Server) schedulePendingStartStaleExpiry(pending *pendingStart) {
	time.AfterFunc(startStaleAbsorbWindow, func() {
		server.expirePendingStartStale(pending)
	})
}

func (server *Server) expirePendingStartStale(expected *pendingStart) {
	server.pendingStartMu.Lock()
	pending := server.pendingStart
	if pending == nil || pending != expected || !pending.staleObserved {
		server.pendingStartMu.Unlock()
		return
	}
	if time.Now().Before(pending.staleDeadline) {
		server.pendingStartMu.Unlock()
		return
	}

	server.pendingStart = nil
	winner := pending.staleWinner
	server.pendingStartMu.Unlock()

	log.Printf(
		"session=%d start_stale_absorb_expired requested=0x%02X adapter_won=0x%02X window=%s -> converting STARTED to FAILED",
		pending.sessionID,
		pending.initiator,
		winner,
		startStaleAbsorbWindow,
	)
	server.staleStartExpired.Add(1)

	failed := downstream.Frame{
		Command: byte(southboundenh.ENHResFailed),
		Payload: []byte{winner},
	}
	select {
	case pending.respCh <- cloneFrame(failed):
	default:
	}

	if pending.mode == pendingStartModeUDPPlain {
		return
	}

	server.reply(pending.sessionID, failed)
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

	// Collect frames for identity caching.
	if isIdentityID(server.pendingInfo.infoID) {
		server.pendingInfo.frames = append(server.pendingInfo.frames, frame)
	}

	if len(frame.Payload) != 1 {
		return true
	}

	if server.pendingInfo.remaining < 0 {
		server.pendingInfo.remaining = int(frame.Payload[0])
		if server.pendingInfo.remaining <= 0 {
			// Cache completed identity response.
			if isIdentityID(server.pendingInfo.infoID) {
				server.infoCache.put(server.pendingInfo.infoID, server.pendingInfo.frames)
			}
			server.pendingInfo = nil
		}
		return true
	}

	server.pendingInfo.remaining--
	if server.pendingInfo.remaining <= 0 {
		// Cache completed identity response.
		if isIdentityID(server.pendingInfo.infoID) {
			server.infoCache.put(server.pendingInfo.infoID, server.pendingInfo.frames)
		}
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

func (server *Server) takeObserverReplayForReceived(symbol byte) ([]downstream.Frame, uint64, bool) {
	server.mutex.Lock()
	owner := server.busOwner
	suppress := false
	var frames []downstream.Frame
	if symbol == ebusSyn {
		if len(server.ownerObserverSeen) > 0 {
			frames = appendObserverRequestSegmentFrames(
				frames,
				server.busOwnerInitiator,
				server.ownerObserverSeen,
				server.ownerObserverAtStart,
			)
		}
		server.ownerObserverExpected = nil
		server.ownerObserverSeen = nil
		if owner != 0 {
			server.ownerObserverAtStart = true
		} else {
			server.ownerObserverAtStart = false
		}
		server.mutex.Unlock()
		if len(frames) == 0 {
			return nil, 0, false
		}
		return frames, server.pendingUDPPlainStartSessionID(), true
	}
	if len(server.ownerObserverExpected) > 0 {
		if symbol == server.ownerObserverExpected[0] {
			server.ownerObserverExpected = server.ownerObserverExpected[1:]
			server.ownerObserverSeen = append(server.ownerObserverSeen, symbol)
			suppress = true
			server.mutex.Unlock()
			return nil, 0, suppress
		}
		if len(server.ownerObserverSeen) > 0 {
			frames = appendRawObserverFrames(frames, server.ownerObserverSeen)
		}
		server.ownerObserverExpected = nil
		server.ownerObserverSeen = nil
		server.ownerObserverAtStart = false
	}
	if len(server.ownerObserverSeen) > 0 {
		frames = appendObserverRequestSegmentFrames(
			frames,
			server.busOwnerInitiator,
			server.ownerObserverSeen,
			server.ownerObserverAtStart,
		)
		server.ownerObserverSeen = nil
		server.ownerObserverAtStart = false
	}
	server.mutex.Unlock()

	if len(frames) == 0 {
		return nil, 0, suppress
	}
	return frames, server.pendingUDPPlainStartSessionID(), suppress
}

func (server *Server) broadcastObserverFrames(
	frames []downstream.Frame,
	skipPendingID uint64,
) {
	if len(frames) == 0 {
		return
	}

	server.mutex.Lock()
	ownerID := server.busOwner
	sessions := make([]*session, 0, len(server.sessions))
	for _, sess := range server.sessions {
		sessions = append(sessions, sess)
	}
	server.mutex.Unlock()

	for _, sess := range sessions {
		if sess.id == ownerID || sess.id == skipPendingID {
			continue
		}
		for _, frame := range frames {
			server.enqueueOrClose(sess, frame, "broadcast_observer_prefix")
		}
	}
}

func (server *Server) currentBusOwner() uint64 {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	return server.busOwner
}

func (server *Server) broadcastReceivedToOwner(frame downstream.Frame, ownerID uint64) {
	if ownerID == 0 {
		return
	}
	server.mutex.Lock()
	sess := server.sessions[ownerID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}
	server.enqueueOrClose(sess, frame, "broadcast_owner_only")
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
	var observerFrames []downstream.Frame
	var sessions []*session

	server.mutex.Lock()
	if server.busOwner != sessionID {
		server.mutex.Unlock()
		return
	}
	if len(server.ownerObserverSeen) > 0 {
		observerFrames = appendRawObserverFrames(nil, server.ownerObserverSeen)
		for _, sess := range server.sessions {
			if sess.id == sessionID {
				continue
			}
			sessions = append(sessions, sess)
		}
	}
	server.busOwner = 0
	server.busOwnerInitiator = 0
	server.ownerObserverAtStart = false
	server.ownerObserverExpected = nil
	server.ownerObserverSeen = nil
	server.busDirty = false
	server.busOwned = time.Time{}
	server.targetResponderWindow = targetResponderWindow{}
	server.resetBusWirePhaseLocked(busWirePhaseIdle)
	server.mutex.Unlock()

	if len(observerFrames) > 0 {
		skipPendingID := server.pendingUDPPlainStartSessionID()
		for _, sess := range sessions {
			if sess.id == skipPendingID {
				continue
			}
			for _, frame := range observerFrames {
				server.enqueueOrClose(sess, frame, "broadcast_observer_release")
			}
		}
	}

	server.releaseBusToken()
}

func (server *Server) setBusOwner(sessionID uint64, initiator byte) {
	server.mutex.Lock()
	server.busOwner = sessionID
	server.busOwnerInitiator = initiator
	server.ownerObserverAtStart = true
	server.ownerObserverExpected = nil
	server.ownerObserverSeen = nil
	server.busDirty = true
	server.busOwned = time.Now().UTC()
	server.targetResponderWindow = targetResponderWindow{}
	server.resetBusWirePhaseLocked(busWirePhaseIdle)
	server.learnSessionInitiatorLocked(sessionID, initiator, "start")
	server.maybeAssociateTargetResponderLocked(sessionID, initiator)
	server.mutex.Unlock()
}

func (server *Server) queueOwnerObserverReplay(sessionID uint64, data byte) bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if server.busOwner != sessionID {
		return false
	}
	if server.busWirePhase == busWirePhaseIdle {
		server.resetBusWirePhaseLocked(busWirePhaseCollectRequest)
	}
	server.ownerObserverExpected = append(server.ownerObserverExpected, data)
	return true
}

func (server *Server) rollbackOwnerObserverReplay(sessionID uint64) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if server.busOwner != sessionID || len(server.ownerObserverExpected) == 0 {
		return
	}
	server.ownerObserverExpected = server.ownerObserverExpected[:len(server.ownerObserverExpected)-1]
}

func appendObserverReplayFrames(
	dst []downstream.Frame,
	initiator byte,
	symbols []byte,
	includeInitiator bool,
) []downstream.Frame {
	if len(symbols) == 0 {
		return dst
	}
	if includeInitiator {
		dst = append(dst, downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{initiator},
		})
	}
	return appendRawObserverFrames(dst, symbols)
}

func appendObserverRequestSegmentFrames(
	dst []downstream.Frame,
	initiator byte,
	symbols []byte,
	includeInitiator bool,
) []downstream.Frame {
	dst = appendObserverReplayFrames(dst, initiator, symbols, includeInitiator)
	return appendRawObserverFrames(dst, []byte{ebusSyn})
}

func appendRawObserverFrames(dst []downstream.Frame, symbols []byte) []downstream.Frame {
	for _, symbol := range symbols {
		dst = append(dst, downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		})
	}
	return dst
}

func (server *Server) takeObserverReplayForAbort() ([]downstream.Frame, uint64) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if len(server.ownerObserverSeen) == 0 {
		server.ownerObserverExpected = nil
		server.ownerObserverSeen = nil
		server.ownerObserverAtStart = false
		return nil, 0
	}

	frames := appendRawObserverFrames(nil, server.ownerObserverSeen)
	server.ownerObserverExpected = nil
	server.ownerObserverSeen = nil
	server.ownerObserverAtStart = false
	return frames, server.pendingUDPPlainStartSessionID()
}

func (server *Server) pendingUDPPlainStartSessionID() uint64 {
	server.pendingStartMu.Lock()
	defer server.pendingStartMu.Unlock()
	if server.pendingStart != nil && server.pendingStart.mode == pendingStartModeUDPPlain {
		return server.pendingStart.sessionID
	}
	return 0
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
		if server.releaseBusIfSynWhileWaiting() {
			return
		}
		server.releaseBusIfIdleSyn()
		return
	}

	server.mutex.Lock()
	if server.busOwner != 0 {
		server.busDirty = true
		server.advanceBusWirePhaseLocked(symbol)
	}
	server.mutex.Unlock()
}

func (server *Server) releaseBusIfSynWhileWaiting() bool {
	server.mutex.Lock()
	owner := server.busOwner
	phase := server.busWirePhase
	if owner == 0 || !phase.isSynTimeoutBoundary() {
		server.mutex.Unlock()
		return false
	}
	server.resetBusWirePhaseLocked(busWirePhaseIdle)
	server.mutex.Unlock()

	if phase == busWirePhaseWaitCmdAck {
		server.synWaitCmdAckTO.Add(1)
	} else {
		server.synWaitResponseTO.Add(1)
	}
	log.Printf("session=%d syn_while_waiting_timeout phase=%s -> release_owner=true", owner, phase)
	server.releaseBusIfOwner(owner)
	return true
}

func (server *Server) resetBusWirePhaseLocked(phase busWirePhase) {
	server.busWirePhase = phase
	server.requestBytesSeen = 0
	server.requestDataLength = -1
	server.requestSrc = 0
	server.requestDst = 0
	server.requestPB = 0
	server.requestSB = 0
	server.requestLEN = 0
	server.requestHeaderCaptured = false
	server.responseBytesRemain = 0
	if phase == busWirePhaseIdle {
		server.targetResponderWindow = targetResponderWindow{}
	}
}

func (server *Server) advanceBusWirePhaseLocked(symbol byte) {
	switch server.busWirePhase {
	case busWirePhaseIdle:
		return
	case busWirePhaseCollectRequest:
		server.requestBytesSeen++
		switch server.requestBytesSeen {
		case 1:
			server.requestSrc = symbol
		case 2:
			server.requestDst = symbol
		case 3:
			server.requestPB = symbol
		case 4:
			server.requestSB = symbol
		case 5:
			server.requestLEN = symbol
			server.requestHeaderCaptured = true
			server.requestDataLength = int(symbol)
			return
		}
		if server.requestDataLength < 0 {
			return
		}
		if server.requestBytesSeen >= 6+server.requestDataLength {
			server.learnSessionInitiatorLocked(server.busOwner, server.requestSrc, "request")
			server.busWirePhase = busWirePhaseWaitCmdAck
			server.maybeOpenTargetResponderWindowLocked(server.requestDst)
		}
	case busWirePhaseWaitCmdAck:
		switch symbol {
		case ebusACK:
			server.busWirePhase = busWirePhaseWaitResponseLen
		case ebusNACK:
			// NACK closes the exchange path without target response bytes.
			server.resetBusWirePhaseLocked(busWirePhaseIdle)
		}
	case busWirePhaseWaitResponseLen:
		server.responseBytesRemain = int(symbol) + 1 // response bytes + CRC
		server.busWirePhase = busWirePhaseWaitResponseBody
	case busWirePhaseWaitResponseBody:
		if server.responseBytesRemain > 0 {
			server.responseBytesRemain--
		}
		if server.responseBytesRemain <= 0 {
			server.busWirePhase = busWirePhaseWaitResponseAck
			server.targetResponderWindow = targetResponderWindow{}
		}
	case busWirePhaseWaitResponseAck:
		// Any non-SYN symbol here is the initiator response ACK/NACK.
		server.resetBusWirePhaseLocked(busWirePhaseIdle)
	}
}

func (server *Server) releaseBusToken() {
	select {
	case server.busToken <- struct{}{}:
	default:
	}
	server.mutex.Lock()
	server.maybeGrantStartArbLocked()
	server.mutex.Unlock()
}

func (server *Server) waitForStartArbitration(
	ctx context.Context,
	sess *session,
	sessionID uint64,
	initiator byte,
) bool {
	server.mutex.Lock()
	grantCh := server.registerStartArbContenderLocked(sessionID, initiator)
	server.mutex.Unlock()

	defer func() {
		server.mutex.Lock()
		server.unregisterStartArbContenderLocked(sessionID)
		server.mutex.Unlock()
	}()

	select {
	case <-grantCh:
	case <-ctx.Done():
		return false
	case <-sess.done:
		return false
	}

	select {
	case <-server.busToken:
		return true
	case <-ctx.Done():
		return false
	case <-sess.done:
		return false
	}
}

func (server *Server) registerStartArbContenderLocked(sessionID uint64, initiator byte) chan struct{} {
	server.startArbSeq++
	contender := &startArbContender{
		sessionID: sessionID,
		initiator: initiator,
		seq:       server.startArbSeq,
		grantCh:   make(chan struct{}),
	}
	server.startArbContenders[sessionID] = contender
	server.maybeGrantStartArbLocked()
	return contender.grantCh
}

func (server *Server) unregisterStartArbContenderLocked(sessionID uint64) {
	delete(server.startArbContenders, sessionID)
	if server.startArbGrantSession == sessionID {
		server.startArbGrantSession = 0
	}
	server.maybeGrantStartArbLocked()
}

func (server *Server) maybeGrantStartArbLocked() {
	if server.startArbGrantSession != 0 {
		return
	}
	if server.busOwner != 0 {
		return
	}
	if len(server.busToken) == 0 {
		return
	}
	winner := server.pickStartArbWinnerLocked()
	if winner == nil {
		return
	}
	server.startArbGrantSession = winner.sessionID
	close(winner.grantCh)
}

func (server *Server) pickStartArbWinnerLocked() *startArbContender {
	var winner *startArbContender
	for _, contender := range server.startArbContenders {
		if winner == nil {
			winner = contender
			continue
		}
		if contender.initiator < winner.initiator {
			winner = contender
			continue
		}
		if contender.initiator == winner.initiator && contender.seq < winner.seq {
			winner = contender
		}
	}
	return winner
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

func (server *Server) learnSessionInitiator(sessionID uint64, initiator byte, source string) {
	server.mutex.Lock()
	server.learnSessionInitiatorLocked(sessionID, initiator, source)
	server.mutex.Unlock()
}

func (server *Server) learnSessionInitiatorLocked(sessionID uint64, initiator byte, source string) {
	if sessionID == 0 || sessionID == udpBridgeOwnerID {
		return
	}
	if !isInitiatorAddress(initiator) {
		return
	}
	if _, ok := server.sessions[sessionID]; !ok {
		return
	}
	if source == "" {
		source = "unknown"
	}

	server.learnedBySession[sessionID] = sessionInitiatorLearning{
		Initiator: initiator,
		LearnedAt: time.Now().UTC(),
		Source:    source,
	}
}

func (server *Server) sessionLearnedInitiator(sessionID uint64) (byte, bool) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	learning, ok := server.learnedBySession[sessionID]
	if !ok {
		return 0, false
	}
	return learning.Initiator, true
}

// SessionInitiatorMappings exposes learned session->initiator identity for
// status/admin surfaces.
func (server *Server) SessionInitiatorMappings() []SessionInitiatorMapping {
	server.mutex.Lock()
	mappings := make([]SessionInitiatorMapping, 0, len(server.learnedBySession))
	for sessionID, learning := range server.learnedBySession {
		mappings = append(mappings, SessionInitiatorMapping{
			SessionID: sessionID,
			Initiator: learning.Initiator,
			LearnedAt: learning.LearnedAt,
			Source:    learning.Source,
		})
	}
	server.mutex.Unlock()

	sort.Slice(mappings, func(i, j int) bool {
		return mappings[i].SessionID < mappings[j].SessionID
	})
	return mappings
}

func (server *Server) registerLocalTargetResponder(targetAddress byte, sessionID uint64) {
	server.mutex.Lock()
	if server.localRespondersByTarget == nil {
		server.localRespondersByTarget = make(map[byte]targetResponderAssociation)
	}
	server.localRespondersByTarget[targetAddress] = targetResponderAssociation{
		targetAddress: targetAddress,
		sessionID:     sessionID,
		mode:          targetResponderModeLocal,
	}
	server.mutex.Unlock()
}

func (server *Server) registerExperimentalChildTargetResponder(targetAddress byte, sessionID uint64) {
	server.mutex.Lock()
	if server.localRespondersByTarget == nil {
		server.localRespondersByTarget = make(map[byte]targetResponderAssociation)
	}
	server.localRespondersByTarget[targetAddress] = targetResponderAssociation{
		targetAddress: targetAddress,
		sessionID:     sessionID,
		mode:          targetResponderModeChildExperimental,
	}
	server.mutex.Unlock()
}

func (server *Server) clearLocalResponderAssociationsForSessionLocked(sessionID uint64) {
	for targetAddress, association := range server.localRespondersByTarget {
		if association.sessionID == sessionID {
			delete(server.localRespondersByTarget, targetAddress)
		}
	}
}

func (server *Server) maybeOpenTargetResponderWindowLocked(targetAddress byte) {
	association, ok := server.localRespondersByTarget[targetAddress]
	if !ok {
		return
	}
	if association.mode == targetResponderModeChildExperimental && !server.cfg.EnableExperimentalChildTargetResponder {
		return
	}
	if association.sessionID == 0 || association.sessionID == server.busOwner {
		return
	}
	if _, ok := server.sessions[association.sessionID]; !ok {
		return
	}

	server.targetResponderWindow = targetResponderWindow{
		open:               true,
		targetAddress:      targetAddress,
		ownerSessionID:     server.busOwner,
		responderSessionID: association.sessionID,
		mode:               association.mode,
		openedAt:           time.Now().UTC(),
	}

	if server.cfg.Debug {
		log.Printf(
			"session=%d target_responder_window_open target=0x%02X responder=%d mode=%s",
			server.busOwner,
			targetAddress,
			association.sessionID,
			association.mode,
		)
	}
}

func (server *Server) maybeAssociateTargetResponderLocked(sessionID uint64, initiator byte) {
	if sessionID == 0 || sessionID == udpBridgeOwnerID {
		return
	}
	if _, ok := server.sessions[sessionID]; !ok {
		return
	}

	if targetAddress, ok := builtInLocalTargetForInitiator(initiator); ok {
		server.localRespondersByTarget[targetAddress] = targetResponderAssociation{
			targetAddress: targetAddress,
			sessionID:     sessionID,
			mode:          targetResponderModeLocal,
		}
		return
	}

	targetAddress, ok := companionTargetAddress(initiator)
	if !ok {
		return
	}
	server.localRespondersByTarget[targetAddress] = targetResponderAssociation{
		targetAddress: targetAddress,
		sessionID:     sessionID,
		mode:          targetResponderModeChildExperimental,
	}
}

func (server *Server) targetResponderWindowAllowsPhaseLocked() bool {
	switch server.busWirePhase {
	case busWirePhaseWaitCmdAck, busWirePhaseWaitResponseLen, busWirePhaseWaitResponseBody:
		return true
	default:
		return false
	}
}

func (server *Server) lookupTargetResponderBySessionLocked(sessionID uint64) (targetResponderAssociation, bool) {
	for _, association := range server.localRespondersByTarget {
		if association.sessionID == sessionID {
			return association, true
		}
	}
	return targetResponderAssociation{}, false
}

func (server *Server) evaluateTargetResponderSendLocked(sessionID uint64) (allow bool, late bool, targetAddress byte, reason string) {
	if sessionID == 0 {
		return false, false, 0, ""
	}
	if server.targetResponderWindow.open &&
		server.targetResponderWindow.responderSessionID == sessionID &&
		server.targetResponderWindow.ownerSessionID == server.busOwner &&
		server.targetResponderWindowAllowsPhaseLocked() {
		return true, false, server.targetResponderWindow.targetAddress, ""
	}

	association, ok := server.lookupTargetResponderBySessionLocked(sessionID)
	if !ok {
		return false, false, 0, ""
	}
	if association.mode == targetResponderModeChildExperimental && !server.cfg.EnableExperimentalChildTargetResponder {
		return false, true, association.targetAddress, "experimental_child_disabled"
	}
	if !server.targetResponderWindow.open {
		return false, true, association.targetAddress, "window_not_open"
	}
	if server.targetResponderWindow.responderSessionID != sessionID {
		return false, true, association.targetAddress, "not_assigned_for_active_target"
	}
	if server.targetResponderWindow.ownerSessionID != server.busOwner {
		return false, true, association.targetAddress, "owner_mismatch"
	}
	if !server.targetResponderWindowAllowsPhaseLocked() {
		return false, true, association.targetAddress, "outside_responder_phase"
	}

	return false, true, association.targetAddress, "unknown"
}

func builtInLocalTargetForInitiator(initiator byte) (byte, bool) {
	// Built-in local emulation profile support starts with VR90-like pairings.
	// For this issue, we keep a single deterministic pairing.
	if initiator == 0x10 {
		return emutargets.BuiltInProfileVR90TargetAddress, true
	}
	return 0, false
}

func companionTargetAddress(initiator byte) (byte, bool) {
	if !isInitiatorAddress(initiator) {
		return 0, false
	}
	target := uint16(initiator) + 0x05
	if target > 0xFE {
		return 0, false
	}
	if target == 0x00 || target == 0xFF {
		return 0, false
	}
	return byte(target), true
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
