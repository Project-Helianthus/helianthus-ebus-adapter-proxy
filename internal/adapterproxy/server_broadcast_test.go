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
		0x15, 0xB5, 0x24, 0x02, 0x08, 0x10, 0x00,
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
	observerSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols)+1)

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}

	wantObserver := append([]byte{0xF7}, wireSymbols...)
	if !equalBytes(observerSymbols, wantObserver) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, wantObserver)
	}
	if !containsGatewayStyleTransaction(observerSymbols, 0xF7, 0x15, 0xB5, 0x24) {
		t.Fatalf("observer symbols = % X; want reconstructible gateway-style transaction", observerSymbols)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderPreservesObserverContextForPendingENHSession(t *testing.T) {
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

	wireSymbols := []byte{
		0x15, 0xB5, 0x24, 0x02, 0x08, 0x10, 0x00,
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
	pendingObserverSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols)+1)

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}

	wantObserver := append([]byte{0xF7}, wireSymbols...)
	if !equalBytes(pendingObserverSymbols, wantObserver) {
		t.Fatalf("pending observer symbols = % X; want % X", pendingObserverSymbols, wantObserver)
	}
	if !containsGatewayStyleTransaction(pendingObserverSymbols, 0xF7, 0x15, 0xB5, 0x24) {
		t.Fatalf("pending observer symbols = % X; want reconstructible gateway-style transaction", pendingObserverSymbols)
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestRunUpstreamReaderPrefixesShorthandTargetThatLooksLikeInitiator(t *testing.T) {
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
	observerSymbols := readReceivedSymbols(t, server.sessions[2], len(wireSymbols)+1)

	if !equalBytes(activeSymbols, wireSymbols) {
		t.Fatalf("active symbols = % X; want % X", activeSymbols, wireSymbols)
	}

	wantObserver := append([]byte{0xF7}, wireSymbols...)
	if !equalBytes(observerSymbols, wantObserver) {
		t.Fatalf("observer symbols = % X; want % X", observerSymbols, wantObserver)
	}
	if !containsGatewayStyleTransaction(observerSymbols, 0xF7, 0x31, 0xB5, 0x24) {
		t.Fatalf("observer symbols = % X; want reconstructible overlapped-target transaction", observerSymbols)
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
