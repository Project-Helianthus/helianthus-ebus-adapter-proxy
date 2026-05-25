package adapterproxy

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

type recordingUpstream struct {
	mutex   sync.Mutex
	written []byte
}

func (upstream *recordingUpstream) Close() error { return nil }

func (upstream *recordingUpstream) ReadFrame() (downstream.Frame, error) {
	return downstream.Frame{}, io.EOF
}

func (upstream *recordingUpstream) WriteFrame(frame downstream.Frame) error {
	if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqSend {
		return nil
	}
	if len(frame.Payload) != 1 {
		return nil
	}
	upstream.mutex.Lock()
	upstream.written = append(upstream.written, frame.Payload[0])
	upstream.mutex.Unlock()
	return nil
}

func (upstream *recordingUpstream) SendInit(features byte) error { return nil }

func (upstream *recordingUpstream) snapshot() []byte {
	upstream.mutex.Lock()
	defer upstream.mutex.Unlock()
	return append([]byte(nil), upstream.written...)
}

func TestForwardUDPPlainDatagramWritesAllBytes(t *testing.T) {
	t.Parallel()

	upstream := &recordingUpstream{}
	server := &Server{
		cfg:      Config{UpstreamTransport: UpstreamUDPPlain},
		upstream: upstream,
		busToken: make(chan struct{}, 1),
	}
	server.busToken <- struct{}{}

	if err := server.forwardUDPPlainDatagram(context.Background(), []byte{0x31, 0xAA, 0x01}); err != nil {
		t.Fatalf("forwardUDPPlainDatagram error = %v", err)
	}

	got := upstream.snapshot()
	want := []byte{0x31, 0xAA, 0x01}
	if len(got) != len(want) {
		t.Fatalf("written len = %d; want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("written[%d] = 0x%02X; want 0x%02X", index, got[index], want[index])
		}
	}
}

type bridgingUpstream struct {
	mutex    sync.Mutex
	readCh   chan downstream.Frame
	started  []byte
	payloads []byte
}

func newBridgingUpstream() *bridgingUpstream {
	return &bridgingUpstream{readCh: make(chan downstream.Frame, 8)}
}

func (upstream *bridgingUpstream) Close() error {
	close(upstream.readCh)
	return nil
}

func (upstream *bridgingUpstream) ReadFrame() (downstream.Frame, error) {
	frame, ok := <-upstream.readCh
	if !ok {
		return downstream.Frame{}, io.EOF
	}
	return frame, nil
}

func (upstream *bridgingUpstream) WriteFrame(frame downstream.Frame) error {
	command := southboundenh.ENHCommand(frame.Command)
	upstream.mutex.Lock()
	defer upstream.mutex.Unlock()

	switch command {
	case southboundenh.ENHReqStart:
		if len(frame.Payload) == 1 {
			initiator := frame.Payload[0]
			upstream.started = append(upstream.started, initiator)
			upstream.readCh <- downstream.Frame{
				Command: byte(southboundenh.ENHResStarted),
				Payload: []byte{initiator},
			}
		}
	case southboundenh.ENHReqSend:
		if len(frame.Payload) == 1 {
			upstream.payloads = append(upstream.payloads, frame.Payload[0])
		}
	}
	return nil
}

func (upstream *bridgingUpstream) SendInit(features byte) error { return nil }

func (upstream *bridgingUpstream) snapshot() (started []byte, payloads []byte) {
	upstream.mutex.Lock()
	defer upstream.mutex.Unlock()
	return append([]byte(nil), upstream.started...), append([]byte(nil), upstream.payloads...)
}

func TestForwardUDPPlainDatagramBridgesStartForENHUpstream(t *testing.T) {
	t.Parallel()

	upstream := newBridgingUpstream()
	server := &Server{
		cfg:      Config{UpstreamTransport: UpstreamENH},
		upstream: upstream,
		busToken: make(chan struct{}, 1),
		synCh:    make(chan struct{}, 1),
	}
	server.busToken <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.waitGroup.Add(1)
	go server.runUpstreamReader(ctx)

	if err := server.forwardUDPPlainDatagram(context.Background(), []byte{0x31, 0x15, 0x07, 0x04, 0x00}); err != nil {
		t.Fatalf("forwardUDPPlainDatagram error = %v", err)
	}

	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	server.busOwned = time.Now().Add(-time.Second)
	server.mutex.Unlock()

	if owner != udpBridgeOwnerID {
		t.Fatalf("bus owner = %d; want udp bridge owner", owner)
	}
	if !dirty {
		t.Fatalf("busDirty = false; want true for active udp bridge owner")
	}

	select {
	case <-server.busToken:
		t.Fatalf("bus token released early; expected hold until idle SYN")
	default:
	}

	started, payloads := upstream.snapshot()
	if len(started) != 1 || started[0] != 0x31 {
		t.Fatalf("start frame payloads = %x; want [31]", started)
	}

	wantPayloads := []byte{0x15, 0x07, 0x04, 0x00}
	if len(payloads) != len(wantPayloads) {
		t.Fatalf("payload write len = %d; want %d", len(payloads), len(wantPayloads))
	}
	for index := range wantPayloads {
		if payloads[index] != wantPayloads[index] {
			t.Fatalf("payload[%d] = 0x%02X; want 0x%02X", index, payloads[index], wantPayloads[index])
		}
	}

	server.noteBusWireSymbol(ebusSyn)
	server.mutex.Lock()
	owner = server.busOwner
	dirty = server.busDirty
	server.mutex.Unlock()
	if owner != udpBridgeOwnerID {
		t.Fatalf("bus owner after first SYN = %d; want udp bridge owner", owner)
	}
	if dirty {
		t.Fatalf("busDirty after first SYN = true; want false boundary marker")
	}

	server.noteBusWireSymbol(ebusSyn)
	server.mutex.Lock()
	owner = server.busOwner
	server.mutex.Unlock()
	if owner != 0 {
		t.Fatalf("bus owner after idle SYN = %d; want 0", owner)
	}
	select {
	case <-server.busToken:
	default:
		t.Fatalf("bus token not released after idle SYN")
	}

	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestForwardUDPPlainDatagramParticipatesInStartArbitrationFIFO(t *testing.T) {
	upstream := newDeterministicStartUpstream()
	server := NewServer(Config{UpstreamTransport: UpstreamENH})
	server.upstream = upstream
	server.leaseManager = nil
	server.sessions = map[uint64]*session{
		1: {id: 1, sendCh: make(chan downstream.Frame, 8), done: make(chan struct{})},
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

	tcpDone := make(chan struct{})
	go func() {
		defer close(tcpDone)
		server.handleStart(ctx, 1, 0x71)
	}()

	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		_, hasTCP := server.startArbContenders[1]
		return hasTCP
	}) {
		t.Fatalf("expected TCP contender before UDP datagram")
	}

	udpDone := make(chan error, 1)
	go func() {
		udpDone <- server.forwardUDPPlainDatagram(ctx, []byte{0x31, 0x15})
	}()

	if !waitUntil(300*time.Millisecond, func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		_, hasTCP := server.startArbContenders[1]
		_, hasUDP := server.startArbContenders[udpBridgeOwnerID]
		return hasTCP && hasUDP
	}) {
		t.Fatalf("expected TCP and UDP contenders before boundary release")
	}

	server.releaseBusIfOwner(99)

	select {
	case frame := <-upstream.writeCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqStart {
			t.Fatalf("first upstream command = 0x%02X; want ENHReqStart", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x71 {
			t.Fatalf("first START payload = %x; want [71] for FIFO TCP winner", frame.Payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected TCP START before UDP bridge")
	}

	select {
	case <-tcpDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("TCP START did not complete")
	}

	select {
	case frame := <-upstream.writeCh:
		t.Fatalf("unexpected UDP write before TCP owner release: cmd=0x%02X payload=%x", frame.Command, frame.Payload)
	default:
	}

	server.releaseBusIfOwner(1)

	select {
	case frame := <-upstream.writeCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqStart {
			t.Fatalf("second upstream command = 0x%02X; want ENHReqStart", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x31 {
			t.Fatalf("second START payload = %x; want [31] for UDP bridge", frame.Payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected UDP bridge START after TCP owner release")
	}

	select {
	case frame := <-upstream.writeCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHReqSend {
			t.Fatalf("third upstream command = 0x%02X; want ENHReqSend", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x15 {
			t.Fatalf("UDP payload write = %x; want [15]", frame.Payload)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("expected UDP payload after UDP bridge START")
	}

	select {
	case err := <-udpDone:
		if err != nil {
			t.Fatalf("UDP datagram forward error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("UDP datagram forward did not complete")
	}

	server.releaseBusIfOwner(udpBridgeOwnerID)
	cancel()
	_ = upstream.Close()
	server.waitGroup.Wait()
}

func TestForwardUDPPlainDatagramBridgeStartTimeoutFallback(t *testing.T) {
	t.Parallel()

	upstream := &recordingUpstream{}
	server := &Server{
		cfg: Config{
			UpstreamTransport: UpstreamENH,
			UDPPlainStartWait: 20 * time.Millisecond,
		},
		upstream: upstream,
		busToken: make(chan struct{}, 1),
		synCh:    make(chan struct{}, 1),
	}
	server.busToken <- struct{}{}

	if err := server.forwardUDPPlainDatagram(context.Background(), []byte{0x31, 0x15, 0x07, 0x04, 0x00}); err != nil {
		t.Fatalf("forwardUDPPlainDatagram error = %v", err)
	}

	got := upstream.snapshot()
	want := []byte{0x15, 0x07, 0x04, 0x00}
	if len(got) != len(want) {
		t.Fatalf("written len = %d; want %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("written[%d] = 0x%02X; want 0x%02X", index, got[index], want[index])
		}
	}
}

func TestBroadcastUDPPlainByteWritesToRegisteredClient(t *testing.T) {
	t.Parallel()

	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP server error = %v", err)
	}
	t.Cleanup(func() { _ = serverConn.Close() })

	clientConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP client error = %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	server := &Server{
		udpListener: serverConn,
		udpClients: map[string]*udpClientEntry{
			clientConn.LocalAddr().String(): &udpClientEntry{addr: clientConn.LocalAddr().(*net.UDPAddr), lastSeen: time.Now()},
		},
	}

	server.broadcastUDPPlainByte(0x5A)

	buffer := make([]byte, 8)
	_ = clientConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	n, _, err := clientConn.ReadFromUDP(buffer)
	if err != nil {
		t.Fatalf("ReadFromUDP error = %v", err)
	}
	if n != 1 || buffer[0] != 0x5A {
		t.Fatalf("received = %x (n=%d); want [5a]", buffer[:n], n)
	}
}
