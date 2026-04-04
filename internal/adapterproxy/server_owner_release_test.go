package adapterproxy

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func TestReleaseBusIfIdleSynReleasesCleanOwner(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.busOwner = 7
	server.busDirty = false

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	server.releaseBusIfIdleSyn()

	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	server.mutex.Unlock()
	if owner != 0 {
		t.Fatalf("busOwner = %d; want 0", owner)
	}
	if dirty {
		t.Fatalf("busDirty = true; want false")
	}

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected released bus token")
	}
}

func TestReleaseBusIfIdleSynKeepsFreshOwner(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.busOwner = 7
	server.busDirty = false
	server.busOwned = time.Now().UTC()

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	server.releaseBusIfIdleSyn()

	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	server.mutex.Unlock()
	if owner != 7 {
		t.Fatalf("busOwner = %d; want 7", owner)
	}
	if dirty {
		t.Fatalf("busDirty = true; want false")
	}

	select {
	case <-server.busToken:
		t.Fatalf("bus token released; expected owner to be retained during grace")
	default:
	}
}

func TestReleaseBusIfIdleSynMarksBoundaryBeforeRelease(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.busOwner = 7
	server.busDirty = true

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	server.releaseBusIfIdleSyn()

	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	server.mutex.Unlock()
	if owner != 7 {
		t.Fatalf("busOwner = %d; want 7", owner)
	}
	if dirty {
		t.Fatalf("busDirty = true; want false after first SYN boundary")
	}

	select {
	case <-server.busToken:
		t.Fatalf("bus token released; expected owner to keep it while dirty")
	default:
	}

	server.releaseBusIfIdleSyn()

	server.mutex.Lock()
	owner = server.busOwner
	dirty = server.busDirty
	server.mutex.Unlock()
	if owner != 0 {
		t.Fatalf("busOwner = %d; want 0 after idle SYN", owner)
	}
	if dirty {
		t.Fatalf("busDirty = true; want false after release")
	}

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected released bus token after idle SYN")
	}
}

func TestNoteBusWireSymbolMarksDirtyForOwnedTraffic(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.busOwner = 3
	server.busDirty = false

	server.noteBusWireSymbol(0x15)

	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	server.mutex.Unlock()

	if owner != 3 {
		t.Fatalf("busOwner = %d; want 3", owner)
	}
	if !dirty {
		t.Fatalf("busDirty = false; want true")
	}
}

func TestNoteBusWireSymbolReleasesOnSynWhileWaitingCommandAck(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.setBusOwner(7, 0x31)

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	server.mutex.Lock()
	server.resetBusWirePhaseLocked(busWirePhaseCollectRequest)
	server.mutex.Unlock()

	request := []byte{0x31, 0x15, 0xB5, 0x00, 0x00, 0x42}
	for _, symbol := range request {
		server.noteBusWireSymbol(symbol)
	}

	server.mutex.Lock()
	phase := server.busWirePhase
	server.mutex.Unlock()
	if phase != busWirePhaseWaitCmdAck {
		t.Fatalf("busWirePhase = %s; want %s", phase, busWirePhaseWaitCmdAck)
	}

	server.noteBusWireSymbol(ebusSyn)

	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	server.mutex.Unlock()
	if owner != 0 {
		t.Fatalf("busOwner = %d; want 0", owner)
	}
	if dirty {
		t.Fatalf("busDirty = true; want false")
	}
	if got := server.synWaitCmdAckTO.Load(); got != 1 {
		t.Fatalf("synWaitCmdAckTO = %d; want 1", got)
	}
	if got := server.synWaitResponseTO.Load(); got != 0 {
		t.Fatalf("synWaitResponseTO = %d; want 0", got)
	}

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected released bus token after SYN while waiting for command ACK")
	}
}

func TestNoteBusWireSymbolReleasesOnSynWhileWaitingResponseBytes(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.setBusOwner(9, 0x31)

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	server.mutex.Lock()
	server.resetBusWirePhaseLocked(busWirePhaseCollectRequest)
	server.mutex.Unlock()

	// request: QQ ZZ PB SB NN D CRC
	request := []byte{0x31, 0x15, 0xB5, 0x09, 0x01, 0x22, 0x6C}
	for _, symbol := range request {
		server.noteBusWireSymbol(symbol)
	}
	server.noteBusWireSymbol(ebusACK) // command ACK
	server.noteBusWireSymbol(0x02)    // response length
	server.noteBusWireSymbol(0x44)    // first response byte

	server.mutex.Lock()
	phase := server.busWirePhase
	server.mutex.Unlock()
	if phase != busWirePhaseWaitResponseBody {
		t.Fatalf("busWirePhase = %s; want %s", phase, busWirePhaseWaitResponseBody)
	}

	server.noteBusWireSymbol(ebusSyn)

	server.mutex.Lock()
	owner := server.busOwner
	server.mutex.Unlock()
	if owner != 0 {
		t.Fatalf("busOwner = %d; want 0", owner)
	}
	if got := server.synWaitCmdAckTO.Load(); got != 0 {
		t.Fatalf("synWaitCmdAckTO = %d; want 0", got)
	}
	if got := server.synWaitResponseTO.Load(); got != 1 {
		t.Fatalf("synWaitResponseTO = %d; want 1", got)
	}

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected released bus token after SYN while waiting for response bytes")
	}
}

func TestNoteBusWireSymbolReleasesOnSynWhileWaitingResponseAck(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.setBusOwner(11, 0x31)

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	server.mutex.Lock()
	server.resetBusWirePhaseLocked(busWirePhaseCollectRequest)
	server.mutex.Unlock()

	// request: QQ ZZ PB SB NN D CRC
	request := []byte{0x31, 0x15, 0xB5, 0x09, 0x01, 0x22, 0x6C}
	for _, symbol := range request {
		server.noteBusWireSymbol(symbol)
	}
	server.noteBusWireSymbol(ebusACK) // command ACK
	server.noteBusWireSymbol(0x02)    // response length
	server.noteBusWireSymbol(0x44)    // response data #1
	server.noteBusWireSymbol(0x55)    // response data #2
	server.noteBusWireSymbol(0x99)    // response CRC

	server.mutex.Lock()
	phase := server.busWirePhase
	server.mutex.Unlock()
	if phase != busWirePhaseWaitResponseAck {
		t.Fatalf("busWirePhase = %s; want %s", phase, busWirePhaseWaitResponseAck)
	}

	server.noteBusWireSymbol(ebusSyn)

	server.mutex.Lock()
	owner := server.busOwner
	server.mutex.Unlock()
	if owner != 0 {
		t.Fatalf("busOwner = %d; want 0", owner)
	}
	if got := server.synWaitCmdAckTO.Load(); got != 0 {
		t.Fatalf("synWaitCmdAckTO = %d; want 0", got)
	}
	if got := server.synWaitResponseTO.Load(); got != 1 {
		t.Fatalf("synWaitResponseTO = %d; want 1", got)
	}

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected released bus token after SYN while waiting for response ACK")
	}
}

func TestDirectModePhaseTrackerCapturesRequestHeaderAndLength(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.setBusOwner(13, 0x31)

	server.mutex.Lock()
	server.resetBusWirePhaseLocked(busWirePhaseCollectRequest)
	server.mutex.Unlock()

	// request: SRC DST PB SB LEN D D CRC
	request := []byte{0x31, 0x15, 0xB5, 0x09, 0x02, 0x44, 0x55, 0x6C}
	for _, symbol := range request {
		server.noteBusWireSymbol(symbol)
	}

	server.mutex.Lock()
	phase := server.busWirePhase
	bytesSeen := server.requestBytesSeen
	dataLen := server.requestDataLength
	src := server.requestSrc
	dst := server.requestDst
	pb := server.requestPB
	sb := server.requestSB
	length := server.requestLEN
	headerCaptured := server.requestHeaderCaptured
	server.mutex.Unlock()

	if phase != busWirePhaseWaitCmdAck {
		t.Fatalf("busWirePhase = %s; want %s", phase, busWirePhaseWaitCmdAck)
	}
	if bytesSeen != len(request) {
		t.Fatalf("requestBytesSeen = %d; want %d", bytesSeen, len(request))
	}
	if dataLen != 2 {
		t.Fatalf("requestDataLength = %d; want 2", dataLen)
	}
	if src != 0x31 || dst != 0x15 || pb != 0xB5 || sb != 0x09 || length != 0x02 {
		t.Fatalf("captured header = src=0x%02X dst=0x%02X pb=0x%02X sb=0x%02X len=0x%02X; want 31 15 B5 09 02", src, dst, pb, sb, length)
	}
	if !headerCaptured {
		t.Fatal("requestHeaderCaptured = false; want true")
	}
}

func TestDirectModePhaseTrackerTransitionsRequestResponsePath(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.setBusOwner(14, 0x31)

	server.mutex.Lock()
	server.resetBusWirePhaseLocked(busWirePhaseCollectRequest)
	server.mutex.Unlock()

	// request with zero request data bytes: SRC DST PB SB LEN CRC
	for _, symbol := range []byte{0x31, 0x15, 0xB5, 0x00, 0x00, 0x42} {
		server.noteBusWireSymbol(symbol)
	}

	server.mutex.Lock()
	phase := server.busWirePhase
	server.mutex.Unlock()
	if phase != busWirePhaseWaitCmdAck {
		t.Fatalf("busWirePhase = %s; want %s", phase, busWirePhaseWaitCmdAck)
	}

	server.noteBusWireSymbol(ebusACK)
	server.mutex.Lock()
	phase = server.busWirePhase
	server.mutex.Unlock()
	if phase != busWirePhaseWaitResponseLen {
		t.Fatalf("busWirePhase after command ACK = %s; want %s", phase, busWirePhaseWaitResponseLen)
	}

	server.noteBusWireSymbol(0x02) // response length
	server.mutex.Lock()
	phase = server.busWirePhase
	remain := server.responseBytesRemain
	server.mutex.Unlock()
	if phase != busWirePhaseWaitResponseBody {
		t.Fatalf("busWirePhase after response LEN = %s; want %s", phase, busWirePhaseWaitResponseBody)
	}
	if remain != 3 {
		t.Fatalf("responseBytesRemain = %d; want 3", remain)
	}

	server.noteBusWireSymbol(0x77) // response data #1
	server.noteBusWireSymbol(0x88) // response data #2
	server.noteBusWireSymbol(0x99) // response CRC
	server.mutex.Lock()
	phase = server.busWirePhase
	server.mutex.Unlock()
	if phase != busWirePhaseWaitResponseAck {
		t.Fatalf("busWirePhase after response payload+CRC = %s; want %s", phase, busWirePhaseWaitResponseAck)
	}

	server.noteBusWireSymbol(ebusACK) // initiator confirms response
	server.mutex.Lock()
	phase = server.busWirePhase
	bytesSeen := server.requestBytesSeen
	headerCaptured := server.requestHeaderCaptured
	server.mutex.Unlock()
	if phase != busWirePhaseIdle {
		t.Fatalf("busWirePhase after response ACK = %s; want %s", phase, busWirePhaseIdle)
	}
	if bytesSeen != 0 {
		t.Fatalf("requestBytesSeen after reset = %d; want 0", bytesSeen)
	}
	if headerCaptured {
		t.Fatal("requestHeaderCaptured = true after reset; want false")
	}
}

func TestHandleSendDoesNotRefreshBusOwnershipTimestamp(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := NewServer(Config{})
	server.upstream = upstream
	server.sessions = map[uint64]*session{
		3: {id: 3, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
	}
	server.busOwner = 3
	server.busDirty = false
	server.busOwned = time.Time{}

	server.handleSend(3, 0x15)

	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	ownedAt := server.busOwned
	server.mutex.Unlock()

	if owner != 3 {
		t.Fatalf("busOwner = %d; want 3", owner)
	}
	if !dirty {
		t.Fatalf("busDirty = false; want true")
	}
	// busOwned must NOT be refreshed by handleSend — it stays as set by
	// setBusOwner so that maxOwnershipDuration and busIdleReleaseGrace
	// measure from initial ownership, preventing indefinite chaining.
	if !ownedAt.IsZero() {
		t.Fatalf("busOwned = %v; want zero (not refreshed by SEND)", ownedAt)
	}
}

func TestHandleStartReleasesOwnerOnTerminalUpstreamError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		command southboundenh.ENHCommand
	}{
		{name: "error_ebus", command: southboundenh.ENHResErrorEBUS},
		{name: "error_host", command: southboundenh.ENHResErrorHost},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			upstream := newFakeUpstream()
			server := NewServer(Config{UpstreamTransport: UpstreamENH})
			server.upstream = upstream
			server.sessions = map[uint64]*session{
				1: {id: 1, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
			}
			server.busOwner = 1
			server.busDirty = true

			select {
			case <-server.busToken:
			default:
				t.Fatalf("expected initial bus token")
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server.waitGroup.Add(1)
			go server.runUpstreamReader(ctx)

			startDone := make(chan struct{})
			go func() {
				server.handleStart(ctx, 1, 0x31)
				close(startDone)
			}()

			if !waitUntil(500*time.Millisecond, server.isStartPending) {
				t.Fatalf("pending start was not registered")
			}

			upstream.readCh <- downstream.Frame{
				Command: byte(testCase.command),
				Payload: []byte{0x00},
			}

			select {
			case <-startDone:
			case <-time.After(750 * time.Millisecond):
				t.Fatalf("handleStart did not return")
			}

			server.mutex.Lock()
			owner := server.busOwner
			dirty := server.busDirty
			server.mutex.Unlock()
			if owner != 0 {
				t.Fatalf("busOwner = %d; want 0", owner)
			}
			if dirty {
				t.Fatalf("busDirty = true; want false")
			}

			select {
			case <-server.busToken:
			default:
				t.Fatalf("expected released bus token")
			}

			select {
			case frame := <-server.sessions[1].sendCh:
				if southboundenh.ENHCommand(frame.Command) != testCase.command {
					t.Fatalf(
						"session reply command = 0x%02X; want 0x%02X",
						frame.Command,
						byte(testCase.command),
					)
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatalf("expected terminal error reply for session")
			}

			cancel()
			_ = upstream.Close()
			server.waitGroup.Wait()
		})
	}
}

func TestDeliverPendingStartENHStartedMismatchAbsorbedWhenMatchingArrivesBounded(t *testing.T) {
	t.Parallel()

	respCh := make(chan downstream.Frame, 1)
	server := &Server{
		cfg: Config{UpstreamTransport: UpstreamENH},
		sessions: map[uint64]*session{
			1: {id: 1, sendCh: make(chan downstream.Frame, 1), done: make(chan struct{})},
		},
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeENH,
			initiator: 0xF0,
		},
	}

	handled := server.deliverPendingStart(downstream.Frame{
		Command: byte(southboundenh.ENHResStarted),
		Payload: []byte{0x31},
	})
	if !handled {
		t.Fatal("deliverPendingStart = false; want true")
	}

	select {
	case frame := <-respCh:
		t.Fatalf("unexpected pending response before matching STARTED: cmd=0x%02X payload=%x", frame.Command, frame.Payload)
	default:
	}

	select {
	case frame := <-server.sessions[1].sendCh:
		t.Fatalf("unexpected session response before matching STARTED: cmd=0x%02X payload=%x", frame.Command, frame.Payload)
	default:
	}

	if !server.isStartPending() {
		t.Fatal("pending start was cleared after stale mismatch; want pending to remain active")
	}

	handled = server.deliverPendingStart(downstream.Frame{
		Command: byte(southboundenh.ENHResStarted),
		Payload: []byte{0xF0},
	})
	if !handled {
		t.Fatal("deliverPendingStart (matching STARTED) = false; want true")
	}

	select {
	case frame := <-respCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResStarted {
			t.Fatalf("respCh command = 0x%02X; want ENHResStarted", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0xF0 {
			t.Fatalf("respCh payload = %x; want [f0]", frame.Payload)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pending start response not delivered")
	}

	select {
	case frame := <-server.sessions[1].sendCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResStarted {
			t.Fatalf("session command = 0x%02X; want ENHResStarted", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0xF0 {
			t.Fatalf("session payload = %x; want [f0]", frame.Payload)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("session response not delivered")
	}

	if server.isStartPending() {
		t.Fatal("pending start remains set after delivery; want cleared")
	}
	if got := server.staleStartAbsorbed.Load(); got != 1 {
		t.Fatalf("staleStartAbsorbed = %d; want 1", got)
	}
	if got := server.staleStartExpired.Load(); got != 0 {
		t.Fatalf("staleStartExpired = %d; want 0", got)
	}
}

func TestDeliverPendingStartENHStartedMismatchExpiresBounded(t *testing.T) {
	t.Parallel()

	respCh := make(chan downstream.Frame, 1)
	server := &Server{
		cfg: Config{UpstreamTransport: UpstreamENH},
		sessions: map[uint64]*session{
			1: {id: 1, sendCh: make(chan downstream.Frame, 1), done: make(chan struct{})},
		},
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeENH,
			initiator: 0xF0,
		},
	}

	handled := server.deliverPendingStart(downstream.Frame{
		Command: byte(southboundenh.ENHResStarted),
		Payload: []byte{0x31},
	})
	if !handled {
		t.Fatal("deliverPendingStart = false; want true")
	}

	select {
	case frame := <-respCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResFailed {
			t.Fatalf("respCh command = 0x%02X; want ENHResFailed", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x31 {
			t.Fatalf("respCh payload = %x; want [31]", frame.Payload)
		}
	case <-time.After(startStaleAbsorbWindow + 300*time.Millisecond):
		t.Fatal("bounded stale expiry response not delivered")
	}

	select {
	case frame := <-server.sessions[1].sendCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResFailed {
			t.Fatalf("session command = 0x%02X; want ENHResFailed", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x31 {
			t.Fatalf("session payload = %x; want [31]", frame.Payload)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("session response not delivered")
	}

	if server.isStartPending() {
		t.Fatal("pending start remains set after bounded expiry; want cleared")
	}
	if got := server.staleStartAbsorbed.Load(); got != 0 {
		t.Fatalf("staleStartAbsorbed = %d; want 0", got)
	}
	if got := server.staleStartExpired.Load(); got != 1 {
		t.Fatalf("staleStartExpired = %d; want 1", got)
	}
}

type deterministicStartUpstream struct {
	readCh  chan downstream.Frame
	writeCh chan downstream.Frame
}

func newDeterministicStartUpstream() *deterministicStartUpstream {
	return &deterministicStartUpstream{
		readCh:  make(chan downstream.Frame, 32),
		writeCh: make(chan downstream.Frame, 32),
	}
}

func (upstream *deterministicStartUpstream) Close() error {
	close(upstream.readCh)
	return nil
}

func (upstream *deterministicStartUpstream) ReadFrame() (downstream.Frame, error) {
	frame, ok := <-upstream.readCh
	if !ok {
		return downstream.Frame{}, io.EOF
	}
	return frame, nil
}

func (upstream *deterministicStartUpstream) WriteFrame(frame downstream.Frame) error {
	upstream.writeCh <- frame
	if southboundenh.ENHCommand(frame.Command) == southboundenh.ENHReqStart && len(frame.Payload) == 1 {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResStarted),
			Payload: []byte{frame.Payload[0]},
		}
	}
	return nil
}

func (upstream *deterministicStartUpstream) SendInit(features byte) error {
	return nil
}

func TestHandleStartArbitrationSameBoundaryPrefersLowerInitiator(t *testing.T) {
	t.Parallel()

	upstream := newDeterministicStartUpstream()
	server := NewServer(Config{UpstreamTransport: UpstreamENH})
	server.upstream = upstream
	server.leaseManager = nil
	server.sessions = map[uint64]*session{
		1: {id: 1, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
		2: {id: 2, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
	}
	server.setBusOwner(99, 0x10)

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	highDone := make(chan struct{})
	lowDone := make(chan struct{})

	go func() {
		defer close(highDone)
		server.handleStart(ctx, 1, 0x71)
	}()
	go func() {
		defer close(lowDone)
		server.handleStart(ctx, 2, 0x31)
	}()

	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		return len(server.startArbContenders) == 2
	}) {
		t.Fatalf("expected two arbitration contenders before boundary release")
	}

	server.releaseBusIfOwner(99)

	select {
	case frame := <-upstream.writeCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqStart {
			t.Fatalf("first upstream command = 0x%02X; want ENHReqStart", frame.Command)
		}
		if len(frame.Payload) != 1 {
			t.Fatalf("first START payload len = %d; want 1", len(frame.Payload))
		}
		if frame.Payload[0] != 0x31 {
			t.Fatalf("first START initiator = 0x%02X; want 0x31", frame.Payload[0])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected first START write after boundary release")
	}

	select {
	case <-lowDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("lower-initiator START did not complete")
	}

	cancel()
	select {
	case <-highDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("higher-initiator START did not exit after cancellation")
	}

	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestHandleStartArbitrationRequeueAfterTimeoutStillPrefersLowerInitiator(t *testing.T) {
	t.Parallel()

	upstream := newDeterministicStartUpstream()
	server := NewServer(Config{UpstreamTransport: UpstreamENH})
	server.upstream = upstream
	server.leaseManager = nil
	server.sessions = map[uint64]*session{
		1: {id: 1, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
		2: {id: 2, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
	}
	server.setBusOwner(99, 0x10)

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	highDone := make(chan struct{})
	go func() {
		defer close(highDone)
		server.handleStart(ctx, 1, 0x71)
	}()

	ctxLow1, cancelLow1 := context.WithCancel(ctx)
	low1Done := make(chan struct{})
	go func() {
		defer close(low1Done)
		server.handleStart(ctxLow1, 2, 0x31)
	}()

	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		return len(server.startArbContenders) == 2
	}) {
		t.Fatalf("expected first low contender to join before cancellation")
	}

	cancelLow1()
	select {
	case <-low1Done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("first low contender did not exit after cancellation")
	}

	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		return len(server.startArbContenders) == 1
	}) {
		t.Fatalf("expected contender set to shrink after first low cancellation")
	}

	low2Done := make(chan struct{})
	go func() {
		defer close(low2Done)
		server.handleStart(ctx, 2, 0x31)
	}()

	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		return len(server.startArbContenders) == 2
	}) {
		t.Fatalf("expected requeued low contender before boundary release")
	}

	server.releaseBusIfOwner(99)

	select {
	case frame := <-upstream.writeCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqStart {
			t.Fatalf("first upstream command = 0x%02X; want ENHReqStart", frame.Command)
		}
		if len(frame.Payload) != 1 {
			t.Fatalf("first START payload len = %d; want 1", len(frame.Payload))
		}
		if frame.Payload[0] != 0x31 {
			t.Fatalf("first START initiator after requeue = 0x%02X; want 0x31", frame.Payload[0])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected first START write after boundary release")
	}

	select {
	case <-low2Done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("requeued low contender START did not complete")
	}

	cancel()
	select {
	case <-highDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("higher-initiator START did not exit after cancellation")
	}

	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestHandleStartArbitrationEqualInitiatorKeepsFIFO(t *testing.T) {
	t.Parallel()

	upstream := newDeterministicStartUpstream()
	server := NewServer(Config{UpstreamTransport: UpstreamENH})
	server.upstream = upstream
	server.leaseManager = nil
	server.sessions = map[uint64]*session{
		1: {id: 1, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
		2: {id: 2, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
	}
	server.setBusOwner(99, 0x10)

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	firstDone := make(chan struct{})
	secondDone := make(chan struct{})

	go func() {
		defer close(firstDone)
		server.handleStart(ctx, 1, 0x31)
	}()
	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		_, hasFirst := server.startArbContenders[1]
		return len(server.startArbContenders) == 1 && hasFirst
	}) {
		t.Fatalf("expected first equal-initiator contender before second joins")
	}
	go func() {
		defer close(secondDone)
		server.handleStart(ctx, 2, 0x31)
	}()

	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		return len(server.startArbContenders) == 2
	}) {
		t.Fatalf("expected two equal-initiator contenders before boundary release")
	}

	server.releaseBusIfOwner(99)

	select {
	case frame := <-upstream.writeCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqStart {
			t.Fatalf("first upstream command = 0x%02X; want ENHReqStart", frame.Command)
		}
		if len(frame.Payload) != 1 {
			t.Fatalf("first START payload len = %d; want 1", len(frame.Payload))
		}
		if frame.Payload[0] != 0x31 {
			t.Fatalf("first START initiator = 0x%02X; want 0x31", frame.Payload[0])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected first START write after boundary release")
	}

	select {
	case <-firstDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("first contender START did not complete")
	}
	select {
	case <-secondDone:
		t.Fatalf("second contender completed before cancellation; want FIFO winner to be session 1")
	default:
	}

	cancel()
	select {
	case <-secondDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("second contender START did not exit after cancellation")
	}

	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestHandleStartAutoJoinReusesLearnedInitiator(t *testing.T) {
	t.Parallel()

	upstream := newDeterministicStartUpstream()
	server := NewServer(Config{UpstreamTransport: UpstreamENH})
	server.upstream = upstream
	server.leaseManager = nil
	server.sessions = map[uint64]*session{
		1: {id: 1, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		server.handleStart(ctx, 1, 0x31)
	}()

	select {
	case frame := <-upstream.writeCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqStart {
			t.Fatalf("first upstream command = 0x%02X; want ENHReqStart", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x31 {
			t.Fatalf("first START payload = %x; want [31]", frame.Payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first START write not observed")
	}

	select {
	case <-firstDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first START did not complete")
	}

	server.releaseBusIfOwner(1)

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		server.handleStart(ctx, 1, 0x00)
	}()

	select {
	case frame := <-upstream.writeCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqStart {
			t.Fatalf("second upstream command = 0x%02X; want ENHReqStart", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x31 {
			t.Fatalf("second START payload = %x; want learned [31]", frame.Payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second START write not observed")
	}

	select {
	case <-secondDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second START did not complete")
	}

	mappings := server.SessionInitiatorMappings()
	if len(mappings) != 1 {
		t.Fatalf("learned mappings len = %d; want 1", len(mappings))
	}
	if mappings[0].SessionID != 1 || mappings[0].Initiator != 0x31 {
		t.Fatalf(
			"mapping[0] = session=%d initiator=0x%02X; want session=1 initiator=0x31",
			mappings[0].SessionID,
			mappings[0].Initiator,
		)
	}
	if mappings[0].Source != "start" {
		t.Fatalf("mapping[0] source = %q; want start", mappings[0].Source)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestHandleStartArbitrationUsesLearnedInitiatorIdentity(t *testing.T) {
	t.Parallel()

	upstream := newDeterministicStartUpstream()
	server := NewServer(Config{UpstreamTransport: UpstreamENH})
	server.upstream = upstream
	server.leaseManager = nil
	server.sessions = map[uint64]*session{
		1: {id: 1, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
		2: {id: 2, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
	}
	server.setBusOwner(99, 0x10)

	select {
	case <-server.busToken:
	default:
		t.Fatalf("expected initial bus token")
	}

	server.learnSessionInitiator(1, 0x71, "start")
	server.learnSessionInitiator(2, 0x31, "start")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	highDone := make(chan struct{})
	lowDone := make(chan struct{})
	go func() {
		defer close(highDone)
		server.handleStart(ctx, 1, 0x00)
	}()
	go func() {
		defer close(lowDone)
		server.handleStart(ctx, 2, 0x00)
	}()

	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		return len(server.startArbContenders) == 2
	}) {
		t.Fatalf("expected two auto-join contenders before boundary release")
	}

	server.releaseBusIfOwner(99)

	select {
	case frame := <-upstream.writeCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqStart {
			t.Fatalf("first upstream command = 0x%02X; want ENHReqStart", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x31 {
			t.Fatalf("first START payload = %x; want learned lower [31]", frame.Payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected first START write after boundary release")
	}

	select {
	case <-lowDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("lower learned-initiator START did not complete")
	}

	cancel()
	select {
	case <-highDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("higher learned-initiator START did not exit after cancellation")
	}

	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestSessionInitiatorMappingsReflectRequestLearningAndUnregister(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.sessions = map[uint64]*session{
		1: {id: 1, sendCh: make(chan downstream.Frame, 1), done: make(chan struct{})},
		2: {id: 2, sendCh: make(chan downstream.Frame, 1), done: make(chan struct{})},
	}

	server.learnSessionInitiator(2, 0x71, "start")
	server.setBusOwner(1, 0x00)

	server.mutex.Lock()
	server.resetBusWirePhaseLocked(busWirePhaseCollectRequest)
	server.mutex.Unlock()
	for _, symbol := range []byte{0x31, 0x15, 0xB5, 0x00, 0x00, 0x42} {
		server.noteBusWireSymbol(symbol)
	}

	mappings := server.SessionInitiatorMappings()
	if len(mappings) != 2 {
		t.Fatalf("learned mappings len = %d; want 2", len(mappings))
	}
	if mappings[0].SessionID != 1 || mappings[0].Initiator != 0x31 || mappings[0].Source != "request" {
		t.Fatalf(
			"mapping[0] = session=%d initiator=0x%02X source=%q; want session=1 initiator=0x31 source=request",
			mappings[0].SessionID,
			mappings[0].Initiator,
			mappings[0].Source,
		)
	}
	if mappings[1].SessionID != 2 || mappings[1].Initiator != 0x71 || mappings[1].Source != "start" {
		t.Fatalf(
			"mapping[1] = session=%d initiator=0x%02X source=%q; want session=2 initiator=0x71 source=start",
			mappings[1].SessionID,
			mappings[1].Initiator,
			mappings[1].Source,
		)
	}

	server.unregisterSession(1)
	server.unregisterSession(2)
	if got := len(server.SessionInitiatorMappings()); got != 0 {
		t.Fatalf("learned mappings after unregister len = %d; want 0", got)
	}
}

func waitUntil(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}
