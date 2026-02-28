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
