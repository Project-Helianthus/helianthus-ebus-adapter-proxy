package ens

import (
	"context"
	"net"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundens "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/ens"
)

func TestListenerSupportsConcurrentSessions(t *testing.T) {
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("expected listener setup success, got %v", err)
	}

	connectEvents := make(chan SessionInfo, 8)
	disconnectEvents := make(chan SessionInfo, 8)
	errorEvents := make(chan error, 8)
	frameEvents := make(chan frameEvent, 8)

	listener, err := NewListener(
		tcpListener,
		func(_ context.Context, session SessionInfo, frame downstream.Frame) error {
			frameEvents <- frameEvent{
				sessionID: session.ID,
				frame:     frame,
			}
			return nil
		},
		Options{
			ReadTimeout: 200 * time.Millisecond,
		},
		Hooks{
			OnConnect: func(session SessionInfo) {
				connectEvents <- session
			},
			OnDisconnect: func(session SessionInfo, _ error) {
				disconnectEvents <- session
			},
			OnError: func(_ SessionInfo, err error) {
				errorEvents <- err
			},
		},
	)
	if err != nil {
		t.Fatalf("expected listener creation success, got %v", err)
	}

	serveContext, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- listener.Serve(serveContext)
	}()
	defer func() {
		_ = listener.Close()
		<-serveErr
	}()

	client1 := mustDialTCP(t, tcpListener.Addr().String())
	client2 := mustDialTCP(t, tcpListener.Addr().String())
	defer client1.Close()
	defer client2.Close()

	waitForEventCount(t, connectEvents, 2)

	writeENSByte(t, client1, 0x31)
	writeENSByte(t, client2, southboundens.ENSByteSync)

	receivedFrames := waitForFrameEvents(t, frameEvents, 2)
	receivedPayloads := make([]byte, 0, len(receivedFrames))
	for _, event := range receivedFrames {
		if event.frame.Command != byte(southboundens.ENSCommandData) {
			t.Fatalf("expected command 0x%02X, got 0x%02X", byte(southboundens.ENSCommandData), event.frame.Command)
		}
		receivedPayloads = append(receivedPayloads, event.frame.Payload[0])
	}
	sort.Slice(receivedPayloads, func(i, j int) bool {
		return receivedPayloads[i] < receivedPayloads[j]
	})
	if !reflect.DeepEqual(receivedPayloads, []byte{0x31, southboundens.ENSByteSync}) {
		t.Fatalf("expected payloads [0x31 0xAA], got %#v", receivedPayloads)
	}

	if err := client1.Close(); err != nil {
		t.Fatalf("expected client1 close success, got %v", err)
	}
	if err := client2.Close(); err != nil {
		t.Fatalf("expected client2 close success, got %v", err)
	}

	waitForEventCount(t, disconnectEvents, 2)

	metrics := listener.Metrics()
	if metrics.ActiveSessions != 0 {
		t.Fatalf("expected active sessions 0, got %d", metrics.ActiveSessions)
	}
	if metrics.TotalConnections != 2 {
		t.Fatalf("expected total connections 2, got %d", metrics.TotalConnections)
	}
	if metrics.TotalDisconnects != 2 {
		t.Fatalf("expected total disconnects 2, got %d", metrics.TotalDisconnects)
	}
	if metrics.TotalFrames != 2 {
		t.Fatalf("expected total frames 2, got %d", metrics.TotalFrames)
	}
	if metrics.TotalErrors != 0 {
		t.Fatalf("expected total errors 0, got %d", metrics.TotalErrors)
	}

	activeSessions := listener.Sessions()
	if len(activeSessions) != 0 {
		t.Fatalf("expected zero active sessions, got %d", len(activeSessions))
	}

	select {
	case err := <-errorEvents:
		t.Fatalf("expected no error hook events, got %v", err)
	default:
	}
}

func TestListenerMalformedFrameErrorAndRecovery(t *testing.T) {
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("expected listener setup success, got %v", err)
	}

	connectEvents := make(chan SessionInfo, 4)
	disconnectEvents := make(chan SessionInfo, 4)
	errorEvents := make(chan error, 4)
	frameEvents := make(chan downstream.Frame, 4)

	listener, err := NewListener(
		tcpListener,
		func(_ context.Context, _ SessionInfo, frame downstream.Frame) error {
			frameEvents <- frame
			return nil
		},
		Options{
			ReadTimeout: 200 * time.Millisecond,
		},
		Hooks{
			OnConnect: func(session SessionInfo) {
				connectEvents <- session
			},
			OnDisconnect: func(session SessionInfo, _ error) {
				disconnectEvents <- session
			},
			OnError: func(_ SessionInfo, err error) {
				errorEvents <- err
			},
		},
	)
	if err != nil {
		t.Fatalf("expected listener creation success, got %v", err)
	}

	serveContext, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- listener.Serve(serveContext)
	}()
	defer func() {
		_ = listener.Close()
		<-serveErr
	}()

	client := mustDialTCP(t, tcpListener.Addr().String())
	defer client.Close()

	waitForEventCount(t, connectEvents, 1)

	_, err = client.Write([]byte{southboundens.ENSByteEscape, 0x02})
	if err != nil {
		t.Fatalf("expected malformed write success, got %v", err)
	}

	writeENSByte(t, client, 0x44)

	waitForErrorCount(t, errorEvents, 1)

	frame := waitForFrameEvent(t, frameEvents)
	expectedFrame := downstream.Frame{
		Command: byte(southboundens.ENSCommandData),
		Payload: []byte{0x44},
	}
	if !reflect.DeepEqual(frame, expectedFrame) {
		t.Fatalf("expected recovered frame %#v, got %#v", expectedFrame, frame)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("expected client close success, got %v", err)
	}

	waitForEventCount(t, disconnectEvents, 1)

	metrics := listener.Metrics()
	if metrics.TotalErrors != 1 {
		t.Fatalf("expected total errors 1, got %d", metrics.TotalErrors)
	}
	if metrics.TotalFrames != 1 {
		t.Fatalf("expected total frames 1, got %d", metrics.TotalFrames)
	}
}

func TestListenerReadTimeoutKeepsSessionAlive(t *testing.T) {
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("expected listener setup success, got %v", err)
	}

	connectEvents := make(chan SessionInfo, 2)
	frameEvents := make(chan downstream.Frame, 2)

	listener, err := NewListener(
		tcpListener,
		func(_ context.Context, _ SessionInfo, frame downstream.Frame) error {
			frameEvents <- frame
			return nil
		},
		Options{
			ReadTimeout: 50 * time.Millisecond,
		},
		Hooks{
			OnConnect: func(session SessionInfo) {
				connectEvents <- session
			},
		},
	)
	if err != nil {
		t.Fatalf("expected listener creation success, got %v", err)
	}

	serveContext, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- listener.Serve(serveContext)
	}()
	defer func() {
		_ = listener.Close()
		<-serveErr
	}()

	client := mustDialTCP(t, tcpListener.Addr().String())
	defer client.Close()

	waitForEventCount(t, connectEvents, 1)

	time.Sleep(150 * time.Millisecond)

	writeENSByte(t, client, 0x51)

	frame := waitForFrameEvent(t, frameEvents)
	expectedFrame := downstream.Frame{
		Command: byte(southboundens.ENSCommandData),
		Payload: []byte{0x51},
	}
	if !reflect.DeepEqual(frame, expectedFrame) {
		t.Fatalf("expected frame %#v, got %#v", expectedFrame, frame)
	}

	metrics := listener.Metrics()
	if metrics.TotalErrors != 0 {
		t.Fatalf("expected total errors 0 after timeout handling, got %d", metrics.TotalErrors)
	}
}

func mustDialTCP(t *testing.T, address string) net.Conn {
	t.Helper()

	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("expected dial success, got %v", err)
	}

	return client
}

func writeENSByte(t *testing.T, client net.Conn, payload byte) {
	t.Helper()

	encoded := southboundens.EncodeENS([]byte{payload})
	_, err := client.Write(encoded)
	if err != nil {
		t.Fatalf("expected frame write success, got %v", err)
	}
}

func waitForEventCount(t *testing.T, events <-chan SessionInfo, expectedCount int) []SessionInfo {
	t.Helper()

	deadline := time.After(2 * time.Second)
	out := make([]SessionInfo, 0, expectedCount)
	for len(out) < expectedCount {
		select {
		case event := <-events:
			out = append(out, event)
		case <-deadline:
			t.Fatalf("timed out waiting for %d session events, got %d", expectedCount, len(out))
		}
	}

	return out
}

func waitForFrameEvents(t *testing.T, events <-chan frameEvent, expectedCount int) []frameEvent {
	t.Helper()

	deadline := time.After(2 * time.Second)
	out := make([]frameEvent, 0, expectedCount)
	for len(out) < expectedCount {
		select {
		case event := <-events:
			out = append(out, event)
		case <-deadline:
			t.Fatalf("timed out waiting for %d frame events, got %d", expectedCount, len(out))
		}
	}

	return out
}

func waitForFrameEvent(t *testing.T, events <-chan downstream.Frame) downstream.Frame {
	t.Helper()

	select {
	case frame := <-events:
		return frame
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for frame event")
		return downstream.Frame{}
	}
}

func waitForErrorCount(t *testing.T, events <-chan error, expectedCount int) []error {
	t.Helper()

	deadline := time.After(2 * time.Second)
	out := make([]error, 0, expectedCount)
	for len(out) < expectedCount {
		select {
		case event := <-events:
			out = append(out, event)
		case <-deadline:
			t.Fatalf("timed out waiting for %d error events, got %d", expectedCount, len(out))
		}
	}

	return out
}

type frameEvent struct {
	sessionID uint64
	frame     downstream.Frame
}
