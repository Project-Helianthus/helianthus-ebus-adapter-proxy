package adapterproxy

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/sourcepolicy"
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
	initResponseWindow       = 2 * time.Second

	// enhCollisionBackoff is the minimum delay between an ENH arbitration
	// FAILED and releasing the bus token. The PIC16F firmware has a race in
	// protocol_state_dispatch where rapid START floods bypass the 60-tick
	// scan deadline and cause transient eBUS signal loss. 50ms lets the
	// firmware flush its FAILED response, apply the deadline, and reset the
	// UART state before the next START.
	enhCollisionBackoff = 50 * time.Millisecond

	// resettedStabilizationDelay is the delay before re-INITing the adapter
	// after a RESETTED event. Gives the adapter's eBUS transceiver time to
	// re-initialize before accepting new commands.
	resettedStabilizationDelay = 200 * time.Millisecond
)

type udpDatagram struct {
	payload []byte
}

// PX13: udpClientEntry tracks last-seen time for TTL-based eviction.
type udpClientEntry struct {
	addr     *net.UDPAddr
	lastSeen time.Time
}

const udpClientTTL = 5 * time.Minute

type Server struct {
	cfg Config

	listener net.Listener
	upstream upstream

	// PX42: wireWriteMu serializes logWireTX + upstream.WriteFrame so TX
	// entries in the wirelog match actual wire ordering across concurrent sessions.
	wireWriteMu sync.Mutex
	wireLog     *wireLogger
	synCh       chan struct{}

	udpListener  *net.UDPConn
	udpClientsMu sync.RWMutex
	udpClients   map[string]*udpClientEntry
	udpQueue     chan udpDatagram

	upstreamFeatures atomic.Uint32
	reinitGuard      chan struct{} // buffered(1), limits re-INIT to one in-flight
	initSentAtNano   atomic.Int64  // UnixNano of last SendInit; 0 = no pending INIT
	lastWireRXAtNano atomic.Int64

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

	busToken               chan struct{}
	busOwner               uint64
	busOwnerInitiator      byte
	awaitingFirstOwnerSend bool // PX-SYN-RACE: true between setBusOwner and first handleSend by owner
	ownerObserverAtStart   bool
	ownerObserverExpected  []byte
	ownerObserverSeen      []byte
	busDirty               bool
	busOwned               time.Time
	busWirePhase           busWirePhase
	requestBytesSeen       int
	requestDataLength      int
	requestSrc             byte
	requestDst             byte
	requestPB              byte
	requestSB              byte
	requestLEN             byte
	requestHeaderCaptured  bool
	responseBytesRemain    int
	targetResponderWindow  targetResponderWindow
	startArbSeq            uint64
	startArbGrantSession   uint64
	startArbContenders     map[uint64]*startArbContender

	pendingStartMu sync.Mutex
	pendingStart   *pendingStart

	pendingInfoMu  sync.Mutex
	pendingInfo    *pendingInfo
	pendingInfoSeq uint64
	infoCache      *adapterInfoCache

	leasesMu     sync.Mutex
	leaseManager *sourcepolicy.LeaseManager
	leasedBySess map[uint64]sourcepolicy.Lease

	upstreamLost     chan struct{}
	upstreamLostOnce sync.Once // CR-P1: guard against double-close

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
	seq       uint64 // GH-P1: monotonic counter to reject stale responses
	remaining int
	infoID    byte
	createdAt time.Time          // GH-P1: for timeout-based expiry
	frames    []downstream.Frame // accumulated response frames for caching
}

const pendingInfoTimeout = 5 * time.Second

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
	// PX1/PX33: WaitResponseLen is a SYN timeout boundary — SYN during
	// response-length phase means the responder timed out and the bus reset.
	// Without this, the idle grace path absorbs the SYN and delays release.
	case busWirePhaseWaitCmdAck, busWirePhaseWaitResponseLen, busWirePhaseWaitResponseBody, busWirePhaseWaitResponseAck:
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
