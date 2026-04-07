package adapterproxy

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

type fakeUpstream struct {
	readCh chan downstream.Frame
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{readCh: make(chan downstream.Frame, 8)}
}

func (upstream *fakeUpstream) Close() error {
	close(upstream.readCh)
	return nil
}

func (upstream *fakeUpstream) ReadFrame() (downstream.Frame, error) {
	frame, ok := <-upstream.readCh
	if !ok {
		return downstream.Frame{}, io.EOF
	}
	return frame, nil
}

func (upstream *fakeUpstream) WriteFrame(frame downstream.Frame) error {
	return nil
}

func (upstream *fakeUpstream) SendInit(features byte) error {
	return nil
}

func TestRunUpstreamReaderBroadcastsReceivedToAllSessions(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:      Config{UpstreamTransport: UpstreamENH},
		upstream: upstream,
		sessions: map[uint64]*session{
			1: {id: 1, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
			2: {id: 2, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
		},
		synCh: make(chan struct{}, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x42},
	}

	for _, sessionID := range []uint64{1, 2} {
		sessionState := server.sessions[sessionID]
		select {
		case frame := <-sessionState.sendCh:
			if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResReceived {
				t.Fatalf("session=%d command = 0x%02X; want ENHResReceived", sessionID, frame.Command)
			}
			if len(frame.Payload) != 1 || frame.Payload[0] != 0x42 {
				t.Fatalf("session=%d payload = %x; want [42]", sessionID, frame.Payload)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("session=%d did not receive broadcast", sessionID)
		}
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderSuppressesReceivedWhileStartPending(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	respCh := make(chan downstream.Frame, 1)
	server := &Server{
		cfg:      Config{UpstreamTransport: UpstreamUDPPlain},
		upstream: upstream,
		sessions: map[uint64]*session{
			1: {id: 1, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
			2: {id: 2, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
		},
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeUDPPlain,
			initiator: 0x31,
		},
		synCh: make(chan struct{}, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x31},
	}

	select {
	case frame := <-respCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResStarted {
			t.Fatalf("pending command = 0x%02X; want ENHResStarted", frame.Command)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("pending start response not delivered")
	}

	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		select {
		case <-server.sessions[2].sendCh:
			t.Errorf("unexpected broadcast while start pending")
		case <-time.After(120 * time.Millisecond):
		}
	}()
	waitGroup.Wait()

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderBroadcastsReceivedWhileStartPendingENH(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	respCh := make(chan downstream.Frame, 1)
	server := &Server{
		cfg:      Config{UpstreamTransport: UpstreamENH},
		upstream: upstream,
		sessions: map[uint64]*session{
			1: {id: 1, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
			2: {id: 2, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
		},
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeENH,
			initiator: 0x31,
		},
		synCh: make(chan struct{}, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x42},
	}

	select {
	case frame := <-server.sessions[2].sendCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResReceived {
			t.Fatalf("command = 0x%02X; want ENHResReceived", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x42 {
			t.Fatalf("payload = %x; want [42]", frame.Payload)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("expected broadcast while ENH start pending")
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderPreservesReconstructibleObserverContextForActiveTraffic(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		collisionBySession:   make(map[uint64]byte),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)
	server.handleSend(1, 0xB5)

	wireSymbols := []byte{
		0x15, 0xB5, 0x24, 0x02, 0x08, 0x10, 0x00,
		0x00, 0x03, 0x01, 0x42, 0x99, 0x00,
		0xAA,
	}
	for _, symbol := range wireSymbols[:2] {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	assertNoReceivedFrames(t, server.sessions[2])

	for _, symbol := range wireSymbols[2:] {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], len(wireSymbols))
	observerSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols)+2)

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}

	wantObserver := append([]byte{0xF7, 0x15, 0xB5, ebusSyn}, wireSymbols[2:]...)
	if !equalBytes(observerSymbols, wantObserver) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, wantObserver)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderFlushesBufferedObserverRequestOnTerminalSyn(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		collisionBySession:   make(map[uint64]byte),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)
	server.handleSend(1, 0xB5)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x15},
	}
	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0xB5},
	}

	assertNoReceivedFrames(t, server.sessions[2])

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{ebusSyn},
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], 3)
	observerSymbols := readReceivedSymbols(t, server.sessions[2], 4)

	if !equalBytes(activeSymbols, []byte{0x15, 0xB5, ebusSyn}) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, []byte{0x15, 0xB5, ebusSyn})
	}

	wantObserver := []byte{0xF7, 0x15, 0xB5, ebusSyn}
	if !equalBytes(observerSymbols, wantObserver) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, wantObserver)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderDoesNotReplayUnconfirmedOwnerBytesOnTerminalSyn(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		collisionBySession:   make(map[uint64]byte),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)
	server.handleSend(1, 0xB5)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x15},
	}

	assertNoReceivedFrames(t, server.sessions[2])

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{ebusSyn},
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], 2)
	observerSymbols := readReceivedSymbols(t, server.sessions[2], 3)

	if !equalBytes(activeSymbols, []byte{0x15, ebusSyn}) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, []byte{0x15, ebusSyn})
	}

	wantObserver := []byte{0xF7, 0x15, ebusSyn}
	if !equalBytes(observerSymbols, wantObserver) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, wantObserver)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderAbortBeforeSynPreservesProvenWireBytesToObservers(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		collisionBySession:   make(map[uint64]byte),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)
	server.handleSend(1, 0xB5)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x15},
	}

	assertNoReceivedFrames(t, server.sessions[2])

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResErrorEBUS),
		Payload: []byte{0x31},
	}

	ownerSymbols := readReceivedSymbols(t, server.sessions[1], 1)
	if !equalBytes(ownerSymbols, []byte{0x15}) {
		t.Fatalf("owner symbols = % X; want % X", ownerSymbols, []byte{0x15})
	}

	ownerFrame := readFrame(t, server.sessions[1])
	if southboundenh.ENHCommand(ownerFrame.Command) != southboundenh.ENHResErrorEBUS {
		t.Fatalf("owner command = 0x%02X; want ENHResErrorEBUS", ownerFrame.Command)
	}
	if !equalBytes(ownerFrame.Payload, []byte{0x31}) {
		t.Fatalf("owner payload = % X; want 31", ownerFrame.Payload)
	}

	observerSymbols := readReceivedSymbols(t, server.sessions[2], 1)
	if !equalBytes(observerSymbols, []byte{0x15}) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, []byte{0x15})
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderEOFBeforeSynPreservesProvenWireBytesToObservers(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		collisionBySession:   make(map[uint64]byte),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)
	server.handleSend(1, 0xB5)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x15},
	}

	assertNoReceivedFrames(t, server.sessions[2])

	ownerSymbols := readReceivedSymbols(t, server.sessions[1], 1)
	if !equalBytes(ownerSymbols, []byte{0x15}) {
		t.Fatalf("owner symbols = % X; want % X", ownerSymbols, []byte{0x15})
	}

	_ = upstream.Close()
	server.waitGroup.Wait()

	observerSymbols := readReceivedSymbols(t, server.sessions[2], 1)
	if !equalBytes(observerSymbols, []byte{0x15}) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, []byte{0x15})
	}
}

func TestUnregisterSessionPreservesProvenWireBytesToObservers(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		collisionBySession:   make(map[uint64]byte),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)
	server.handleSend(1, 0xB5)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x15},
	}

	assertNoReceivedFrames(t, server.sessions[2])

	server.unregisterSession(1)

	observerSymbols := readReceivedSymbols(t, server.sessions[2], 1)
	if !equalBytes(observerSymbols, []byte{0x15}) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, []byte{0x15})
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderDoesNotSuppressForeignInterleavingByteDuringReplaySuppression(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)
	server.handleSend(1, 0xB5)

	wireSymbols := []byte{0x31, 0x15, 0xB5, ebusSyn}
	for _, symbol := range wireSymbols {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], len(wireSymbols))
	observerSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols))

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}
	if !equalBytes(observerSymbols, wireSymbols) {
		t.Fatalf("observer symbols = % X; want % X (foreign interleaving must pass raw and abort replay)", observerSymbols, wireSymbols)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderReplaysSuppressedWireBytesRawWhenLaterByteMismatches(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)
	server.handleSend(1, 0xB5)

	wireSymbols := []byte{0x15, 0x31, ebusSyn}
	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{wireSymbols[0]},
	}

	assertNoReceivedFrames(t, server.sessions[2])

	for _, symbol := range wireSymbols[1:] {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], len(wireSymbols))
	observerSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols))

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}
	if !equalBytes(observerSymbols, wireSymbols) {
		t.Fatalf("observer symbols = % X; want % X (suppressed proven wire bytes must be restored raw on later mismatch)", observerSymbols, wireSymbols)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderPreservesObserverContextForPendingENHSession(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
		pendingStart: &pendingStart{
			sessionID: 2,
			respCh:    make(chan downstream.Frame, 1),
			mode:      pendingStartModeENH,
			initiator: 0x31,
		},
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)

	wireSymbols := []byte{
		0x15,
	}
	for _, symbol := range wireSymbols {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	assertNoReceivedFrames(t, server.sessions[2])

	server.handleSend(1, 0xB5)
	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0xB5},
	}

	assertNoReceivedFrames(t, server.sessions[2])

	wireSymbols = append(wireSymbols,
		0xB5, 0x24, 0x02, 0x08, 0x10, 0x00,
		0x00, 0x03, 0x01, 0x42, 0x99, 0x00,
		0xAA,
	)
	for _, symbol := range wireSymbols[2:] {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], len(wireSymbols))
	pendingObserverSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols)+2)

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}

	wantObserver := append([]byte{0xF7, 0x15, 0xB5, ebusSyn}, wireSymbols[2:]...)
	if !equalBytes(pendingObserverSymbols, wantObserver) {
		t.Fatalf("pending observer symbols = % X; want % X", pendingObserverSymbols, wantObserver)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderPrefixesShorthandTargetThatLooksLikeInitiator(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		ownerObserverAtStart: true,
		busDirty:             true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x31)
	server.handleSend(1, 0xB5)

	wireSymbols := []byte{
		0x31, 0xB5, 0x24, 0x02, 0x08, 0x10, 0x00,
		0x00, 0x03, 0x01, 0x42, 0x99, 0x00,
		0xAA,
	}
	for _, symbol := range wireSymbols {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], len(wireSymbols))
	observerSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols)+2)

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}

	wantObserver := append([]byte{0xF7, 0x31, 0xB5, ebusSyn}, wireSymbols[2:]...)
	if !equalBytes(observerSymbols, wantObserver) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, wantObserver)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderDoesNotPrefixFullForeignTelegramAtBoundary(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                 Config{UpstreamTransport: UpstreamENH},
		upstream:            upstream,
		sessions:            map[uint64]*session{},
		synCh:               make(chan struct{}, 1),
		startOfTelegram:     true,
		observedInitiatorAt: make(map[byte]time.Time),
		busOwner:            1,
		busOwnerInitiator:   0xF7,
		busDirty:            true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	wireSymbols := []byte{
		0x31, 0x08, 0xB5, 0x24, 0x02, 0x15, 0x10, 0x00,
		0x00, 0x03, 0x01, 0x42, 0x99, 0x00,
		0xAA,
	}
	for _, symbol := range wireSymbols {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], len(wireSymbols))
	observerSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols))

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}
	if !equalBytes(observerSymbols, wireSymbols) {
		t.Fatalf("observer symbols = % X; want % X (no synthetic prefix on foreign full telegram)", observerSymbols, wireSymbols)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderDoesNotPrefixForeignTelegramWhenFirstByteMatchesOwnerShorthand(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                 Config{UpstreamTransport: UpstreamENH},
		upstream:            upstream,
		sessions:            map[uint64]*session{},
		synCh:               make(chan struct{}, 1),
		startOfTelegram:     true,
		observedInitiatorAt: make(map[byte]time.Time),
		busOwner:            1,
		busOwnerInitiator:   0xF7,
		busDirty:            true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	wireSymbols := []byte{
		0x31, 0x08, 0xB5, 0x24, 0x02, 0x15, 0x10, 0x00,
		0x00, 0x03, 0x01, 0x42, 0x99, 0x00,
		0xAA,
	}
	for _, symbol := range wireSymbols {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], len(wireSymbols))
	observerSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols))

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}
	if !equalBytes(observerSymbols, wireSymbols) {
		t.Fatalf("observer symbols = % X; want % X (no synthetic prefix on overlapping foreign telegram)", observerSymbols, wireSymbols)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderPreservesObserverContextWhenFirstRXPrecedesSecondSend(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := &Server{
		cfg:                  Config{UpstreamTransport: UpstreamENH},
		upstream:             upstream,
		sessions:             map[uint64]*session{},
		synCh:                make(chan struct{}, 1),
		startOfTelegram:      true,
		observedInitiatorAt:  make(map[byte]time.Time),
		busOwner:             1,
		busOwnerInitiator:    0xF7,
		busDirty:             true,
		ownerObserverAtStart: true,
	}
	server.sessions[1] = &session{id: 1, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}
	server.sessions[2] = &session{id: 2, sendCh: make(chan downstream.Frame, 32), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	server.handleSend(1, 0x15)
	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x15},
	}

	assertNoReceivedFrames(t, server.sessions[2])

	server.handleSend(1, 0xB5)
	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0xB5},
	}

	assertNoReceivedFrames(t, server.sessions[2])

	wireSymbols := []byte{
		0x15, 0xB5, 0x24, 0x02, 0x08, 0x10, 0x00,
		0x00, 0x03, 0x01, 0x42, 0x99, 0x00,
		0xAA,
	}
	for _, symbol := range wireSymbols[2:] {
		upstream.readCh <- downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		}
	}

	activeSymbols := readReceivedSymbols(t, server.sessions[1], len(wireSymbols))
	observerSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols)+2)

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}

	wantObserver := append([]byte{0xF7, 0x15, 0xB5, ebusSyn}, wireSymbols[2:]...)
	if !equalBytes(observerSymbols, wantObserver) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, wantObserver)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func readReceivedSymbols(t *testing.T, sessionState *session, count int) []byte {
	t.Helper()

	symbols := make([]byte, 0, count)
	for len(symbols) < count {
		select {
		case frame := <-sessionState.sendCh:
			if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResReceived {
				t.Fatalf("command = 0x%02X; want ENHResReceived", frame.Command)
			}
			if len(frame.Payload) != 1 {
				t.Fatalf("payload len = %d; want 1", len(frame.Payload))
			}
			symbols = append(symbols, frame.Payload[0])
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out waiting for %d received symbols; got %d", count, len(symbols))
		}
	}

	return symbols
}

func readFrame(t *testing.T, sessionState *session) downstream.Frame {
	t.Helper()

	select {
	case frame := <-sessionState.sendCh:
		return frame
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timed out waiting for frame")
		return downstream.Frame{}
	}
}

func assertNoReceivedFrames(t *testing.T, sessionState *session) {
	t.Helper()

	select {
	case frame := <-sessionState.sendCh:
		t.Fatalf("unexpected frame while observer replay should still be buffered: cmd=0x%02X payload=% X", frame.Command, frame.Payload)
	case <-time.After(75 * time.Millisecond):
	}
}

func equalBytes(left []byte, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsGatewayStyleTransaction(symbols []byte, initiator byte, target byte, primary byte, secondary byte) bool {
	if len(symbols) < 5 {
		return false
	}

	for index := 0; index <= len(symbols)-5; index++ {
		if symbols[index] != initiator ||
			symbols[index+1] != target ||
			symbols[index+2] != primary ||
			symbols[index+3] != secondary {
			continue
		}
		for tail := index + 4; tail < len(symbols); tail++ {
			if symbols[tail] == ebusSyn {
				return true
			}
		}
	}

	return false
}

// --- RESETTED recovery tests ---

type initRecordingUpstream struct {
	readCh    chan downstream.Frame
	initCalls chan byte
}

func newInitRecordingUpstream() *initRecordingUpstream {
	return &initRecordingUpstream{
		readCh:    make(chan downstream.Frame, 8),
		initCalls: make(chan byte, 4),
	}
}

func (u *initRecordingUpstream) Close() error {
	close(u.readCh)
	return nil
}

func (u *initRecordingUpstream) ReadFrame() (downstream.Frame, error) {
	frame, ok := <-u.readCh
	if !ok {
		return downstream.Frame{}, io.EOF
	}
	return frame, nil
}

func (u *initRecordingUpstream) WriteFrame(frame downstream.Frame) error {
	return nil
}

func (u *initRecordingUpstream) SendInit(features byte) error {
	u.initCalls <- features
	return nil
}

func TestRunUpstreamReaderResettedAbortsPendingStart(t *testing.T) {
	t.Parallel()

	upstream := newInitRecordingUpstream()
	respCh := make(chan downstream.Frame, 1)
	server := &Server{
		cfg:      Config{UpstreamTransport: UpstreamENH},
		upstream: upstream,
		sessions: map[uint64]*session{
			1: {id: 1, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
		},
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeENH,
			initiator: 0x31,
		},
		synCh:     make(chan struct{}, 1),
		busToken:  make(chan struct{}, 1),
		infoCache: newAdapterInfoCache(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResResetted),
		Payload: []byte{0x01},
	}

	select {
	case frame := <-respCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResFailed {
			t.Fatalf("pending start response command = 0x%02X; want ENHResFailed", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x31 {
			t.Fatalf("pending start response payload = %x; want [31]", frame.Payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pending start response not delivered after RESETTED")
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderResettedAbortsPendingInfo(t *testing.T) {
	t.Parallel()

	upstream := newInitRecordingUpstream()
	server := &Server{
		cfg:      Config{UpstreamTransport: UpstreamENH},
		upstream: upstream,
		sessions: map[uint64]*session{
			1: {id: 1, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
		},
		pendingInfo: &pendingInfo{
			sessionID: 1,
			remaining: -1,
			infoID:    0x00,
		},
		synCh:     make(chan struct{}, 1),
		busToken:  make(chan struct{}, 1),
		infoCache: newAdapterInfoCache(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResResetted),
		Payload: []byte{0x01},
	}

	select {
	case frame := <-server.sessions[1].sendCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResErrorHost {
			t.Fatalf("pending info abort command = 0x%02X; want ENHResErrorHost", frame.Command)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pending info error not delivered after RESETTED")
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderResettedSendsReInit(t *testing.T) {
	t.Parallel()

	upstream := newInitRecordingUpstream()
	server := &Server{
		cfg:      Config{UpstreamTransport: UpstreamENH},
		upstream: upstream,
		sessions: map[uint64]*session{
			1: {id: 1, sendCh: make(chan downstream.Frame, 4), done: make(chan struct{})},
		},
		synCh:     make(chan struct{}, 1),
		busToken:  make(chan struct{}, 1),
		infoCache: newAdapterInfoCache(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	upstream.readCh <- downstream.Frame{
		Command: byte(southboundenh.ENHResResetted),
		Payload: []byte{0x01},
	}

	select {
	case features := <-upstream.initCalls:
		if features != 0x01 {
			t.Fatalf("SendInit features = 0x%02X; want 0x01", features)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SendInit not called after RESETTED")
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}
