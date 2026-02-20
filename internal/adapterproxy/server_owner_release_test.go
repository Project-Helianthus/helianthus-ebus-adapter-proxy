package adapterproxy

import (
	"context"
	"testing"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
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

func TestDeliverPendingStartENHStartedMismatchBecomesFailed(t *testing.T) {
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
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pending start response not delivered")
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
		t.Fatal("pending start remains set after delivery; want cleared")
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
