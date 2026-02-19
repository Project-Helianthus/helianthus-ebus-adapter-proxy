package adapterproxy

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
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
		udpClients: map[string]*net.UDPAddr{
			clientConn.LocalAddr().String(): clientConn.LocalAddr().(*net.UDPAddr),
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
