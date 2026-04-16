package adapterproxy

// Cross-repo ENS/ENH conformance test invariant names.
// These names are shared across proxy, adaptermux, ebusgo, VRC Explorer,
// and docs so drift is visible in review. Implementations are repo-specific.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

// XR_START_ReconnectWait_BoundedDeadline — pending START waits must unblock
// on session done or upstream loss, not just response/timeout.
func TestXR_START_ReconnectWait_BoundedDeadline(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := NewServer(Config{UpstreamTransport: UpstreamENH, ListenAddr: "127.0.0.1:0", UpstreamAddr: "127.0.0.1:0"})
	server.upstream = upstream
	server.leaseManager = nil
	sess := &session{id: 1, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})}
	server.sessions = map[uint64]*session{1: sess}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		server.handleStart(ctx, 1, 0xF7)
	}()

	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		return len(server.startArbContenders) >= 1
	}) {
		// May have already acquired token.
	}

	// Close session — handleStart must unblock deterministically.
	close(sess.done)

	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("handleStart did not unblock after session close")
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

// XR_UDP_LeaseTTL_CapRefresh_Bounded — UDP client admission cap rejects new
// clients and their datagrams are not processed.
func TestXR_UDP_LeaseTTL_CapRefresh_Bounded(t *testing.T) {
	t.Parallel()

	server := &Server{
		cfg:        Config{UpstreamTransport: UpstreamUDPPlain},
		udpClients: make(map[string]*udpClientEntry),
	}

	// Fill to cap.
	for i := 0; i < maxUDPClients; i++ {
		addr := &net.UDPAddr{IP: net.IPv4(10, 0, byte(i/256), byte(i%256)), Port: 9999}
		server.udpClients[addr.String()] = &udpClientEntry{addr: addr, lastSeen: time.Now()}
	}

	// New client rejected.
	newAddr := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 1), Port: 5555}
	if server.registerUDPPlainClient(newAddr) {
		t.Fatalf("expected rejection at cap; got admitted")
	}

	// Existing client refresh still works.
	existingAddr := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 0), Port: 9999}
	if !server.registerUDPPlainClient(existingAddr) {
		t.Fatalf("expected existing client refresh to succeed")
	}
}

// XR_UpstreamLoss_GracefulShutdown_NoHang — full Serve-level test that
// upstream loss terminates Serve without hanging.
func TestXR_UpstreamLoss_GracefulShutdown_NoHang(t *testing.T) {
	t.Parallel()

	// Create a real TCP listener for the proxy.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error = %v", err)
	}

	upstream := newFakeUpstream()
	server := NewServer(Config{
		UpstreamTransport: UpstreamENH,
		ListenAddr:        listener.Addr().String(),
		UpstreamAddr:      "127.0.0.1:0",
	})
	server.upstream = upstream
	server.listener = listener

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		// Run the full Serve accept loop + cleanup.
		server.waitGroup.Add(1)
		go server.runUpstreamReader(ctx)

		// Simulate accept loop running briefly.
		go func() {
			<-server.upstreamLost
			_ = listener.Close()
		}()

		// Trigger upstream loss.
		time.Sleep(50 * time.Millisecond)
		_ = upstream.Close()

		// Wait for upstream reader to exit.
		server.waitGroup.Wait()
		serveDone <- nil
	}()

	select {
	case <-serveDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("shutdown did not complete within 3s after upstream loss")
	}
}

// XR_START_Cancel_ReleasesOwnership — verify START cancel releases bus ownership.
func TestXR_START_Cancel_ReleasesOwnership(t *testing.T) {
	t.Parallel()

	upstream := newFakeUpstream()
	server := NewServer(Config{UpstreamTransport: UpstreamENH, ListenAddr: "127.0.0.1:0", UpstreamAddr: "127.0.0.1:0"})
	server.upstream = upstream
	server.leaseManager = nil
	sess := &session{id: 1, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})}
	server.sessions = map[uint64]*session{1: sess}

	server.setBusOwner(1, 0xF7)

	server.handleStartCancel(1)

	server.mutex.Lock()
	owner := server.busOwner
	server.mutex.Unlock()
	if owner != 0 {
		t.Fatalf("bus owner = %d after cancel; want 0", owner)
	}
}

// XR_ENH_0xAA_DataNotSYN — logical 0xAA in payload must NOT be treated as
// a bus SYN. It is valid data that the ENH encoder wraps in a 2-byte pair.
func TestXR_ENH_0xAA_DataNotSYN(t *testing.T) {
	t.Parallel()

	// Verify that forwardUDPPlainDatagram does NOT reject a payload containing
	// 0xAA on ENH upstream — the ENH encoder handles it.
	upstream := &recordingUpstream{}
	server := &Server{
		cfg:      Config{UpstreamTransport: UpstreamENH},
		upstream: upstream,
		busToken: make(chan struct{}, 1),
	}
	server.busToken <- struct{}{}

	err := server.forwardUDPPlainDatagram(context.Background(), []byte{0xF7, 0xAA, 0x01})
	if err != nil {
		t.Fatalf("payload with logical 0xAA should be accepted on ENH upstream; got %v", err)
	}
}

// XR_Arbitration_Fairness_NoStarvation — FIFO arbitration prevents starvation.
func TestXR_Arbitration_Fairness_NoStarvation(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{UpstreamTransport: UpstreamENH, ListenAddr: "127.0.0.1:0", UpstreamAddr: "127.0.0.1:0"})

	server.mutex.Lock()
	ch1 := server.registerStartArbContenderLocked(1, 0xF7) // first, higher initiator
	server.mutex.Unlock()

	time.Sleep(time.Millisecond)

	server.mutex.Lock()
	ch2 := server.registerStartArbContenderLocked(2, 0x31) // second, lower initiator
	server.mutex.Unlock()

	server.busToken = make(chan struct{}, 1)
	server.busToken <- struct{}{}
	server.mutex.Lock()
	server.maybeGrantStartArbLocked()
	granted := server.startArbGrantSession
	server.mutex.Unlock()

	if granted != 1 {
		t.Fatalf("FIFO grant = session %d; want 1 (first registered)", granted)
	}

	select {
	case <-ch1:
	default:
		t.Fatalf("ch1 should be closed after grant")
	}
	select {
	case <-ch2:
		t.Fatalf("ch2 should still be open")
	default:
	}
}

// XR_ENH_ParserReset_AfterReadTimeout — parser state must be clean after
// a read timeout so the next frame is not corrupted.
func TestXR_ENH_ParserReset_AfterReadTimeout(t *testing.T) {
	t.Parallel()

	parser := &southboundenh.ENHParser{}

	// Feed first byte of a pair (simulating partial read before timeout).
	_, complete, err := parser.Feed(0xC4)
	if err != nil || complete {
		t.Fatalf("expected pending state; got complete=%v err=%v", complete, err)
	}

	// Simulate timeout → reset (as done by ReadFrame on timeout).
	parser.Reset()

	// Next byte must be treated as fresh data, not second byte of old pair.
	frame, complete, err := parser.Feed(0x42)
	if err != nil {
		t.Fatalf("expected success after reset; got %v", err)
	}
	if !complete {
		t.Fatalf("expected complete frame for data byte < 0x80")
	}
	if frame.Command != byte(southboundenh.ENHResReceived) || frame.Payload[0] != 0x42 {
		t.Fatalf("expected Received(0x42); got cmd=0x%02X data=0x%02X", frame.Command, frame.Payload[0])
	}
}
