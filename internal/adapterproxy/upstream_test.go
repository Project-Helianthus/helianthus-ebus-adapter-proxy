package adapterproxy

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func TestUDPPlainUpstreamReadFrameHandlesLargeDatagram(t *testing.T) {
	t.Parallel()

	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	conn, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("DialUDP error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := &udpPlainUpstreamClient{
		conn:         conn,
		readTimeout:  200 * time.Millisecond,
		writeTimeout: 200 * time.Millisecond,
		buffer:       make([]byte, udpPlainReadBufferSize),
	}

	payload := bytes.Repeat([]byte{0x7E}, 4096)
	if _, err := server.WriteToUDP(payload, conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("WriteToUDP error = %v", err)
	}

	for index, want := range payload {
		frame, err := client.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame[%d] error = %v", index, err)
		}
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResReceived {
			t.Fatalf("ReadFrame[%d] command = 0x%02X; want ENHResReceived", index, frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != want {
			t.Fatalf("ReadFrame[%d] payload = %x; want [%02x]", index, frame.Payload, want)
		}
	}
}

func TestUDPPlainUpstreamWriteFrameRejectsUnsupportedCommand(t *testing.T) {
	t.Parallel()

	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	conn, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("DialUDP error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := &udpPlainUpstreamClient{
		conn:         conn,
		readTimeout:  200 * time.Millisecond,
		writeTimeout: 200 * time.Millisecond,
		buffer:       make([]byte, udpPlainReadBufferSize),
	}

	err = client.WriteFrame(downstream.Frame{
		Command: byte(southboundenh.ENHReqStart),
		Payload: []byte{0x31},
	})
	if err == nil {
		t.Fatalf("WriteFrame error = nil; want non-nil")
	}
}

func TestTCPPlainUpstreamRoundTrip(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverRead := make(chan byte, 1)
	serverWriteErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverWriteErr <- acceptErr
			return
		}
		defer conn.Close()

		buffer := make([]byte, 1)
		if _, readErr := conn.Read(buffer); readErr != nil {
			serverWriteErr <- readErr
			return
		}
		serverRead <- buffer[0]
		_, writeErr := conn.Write([]byte{0xAA})
		serverWriteErr <- writeErr
	}()

	client, err := dialUpstreamTCPPlain(
		context.Background(),
		listener.Addr().String(),
		time.Second,
		time.Second,
		time.Second,
	)
	if err != nil {
		t.Fatalf("dialUpstreamTCPPlain error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if writeErr := client.WriteFrame(downstream.Frame{
		Command: byte(southboundenh.ENHReqSend),
		Payload: []byte{0x31},
	}); writeErr != nil {
		t.Fatalf("WriteFrame error = %v", writeErr)
	}

	select {
	case value := <-serverRead:
		if value != 0x31 {
			t.Fatalf("server observed byte = 0x%02X; want 0x31", value)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("server did not receive client byte")
	}

	frame, err := client.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame error = %v", err)
	}
	if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResReceived {
		t.Fatalf("ReadFrame command = 0x%02X; want ENHResReceived", frame.Command)
	}
	if len(frame.Payload) != 1 || frame.Payload[0] != 0xAA {
		t.Fatalf("ReadFrame payload = %x; want [aa]", frame.Payload)
	}

	select {
	case writeErr := <-serverWriteErr:
		if writeErr != nil {
			t.Fatalf("server write error = %v", writeErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("server write completion timeout")
	}
}

func TestTCPPlainUpstreamWriteFrameRejectsUnsupportedCommand(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	client, err := dialUpstreamTCPPlain(
		context.Background(),
		listener.Addr().String(),
		time.Second,
		time.Second,
		time.Second,
	)
	if err != nil {
		t.Fatalf("dialUpstreamTCPPlain error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	writeErr := client.WriteFrame(downstream.Frame{
		Command: byte(southboundenh.ENHReqStart),
		Payload: []byte{0x31},
	})
	if writeErr == nil {
		t.Fatalf("WriteFrame error = nil; want non-nil")
	}
	want := "does not support command"
	if !bytes.Contains([]byte(writeErr.Error()), []byte(want)) {
		t.Fatalf("WriteFrame error = %q; want contains %q", writeErr.Error(), want)
	}

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("accept goroutine timeout")
	}
}

func TestDialUpstreamSupportsTCPPlain(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	upstreamClient, err := dialUpstream(
		context.Background(),
		UpstreamTCPPlain,
		listener.Addr().String(),
		time.Second,
		time.Second,
		time.Second,
	)
	if err != nil {
		t.Fatalf("dialUpstream error = %v", err)
	}
	t.Cleanup(func() { _ = upstreamClient.Close() })

	if _, ok := upstreamClient.(*tcpPlainUpstreamClient); !ok {
		t.Fatalf("dialUpstream returned %T; want *tcpPlainUpstreamClient", upstreamClient)
	}

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("accept goroutine timeout")
	}
}

func TestTCPPlainUpstreamWriteFrameRequiresSingleBytePayload(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	client, err := dialUpstreamTCPPlain(
		context.Background(),
		listener.Addr().String(),
		time.Second,
		time.Second,
		time.Second,
	)
	if err != nil {
		t.Fatalf("dialUpstreamTCPPlain error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	writeErr := client.WriteFrame(downstream.Frame{
		Command: byte(southboundenh.ENHReqSend),
		Payload: []byte{0x31, 0x15},
	})
	if writeErr == nil {
		t.Fatalf("WriteFrame error = nil; want non-nil")
	}
	if got := writeErr.Error(); got == "" || !bytes.Contains([]byte(got), []byte("exactly one byte")) {
		t.Fatalf("WriteFrame error = %q; want payload-size message", got)
	}
}

func TestTCPPlainUpstreamSendInitNoop(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	client, err := dialUpstreamTCPPlain(
		context.Background(),
		listener.Addr().String(),
		time.Second,
		time.Second,
		time.Second,
	)
	if err != nil {
		t.Fatalf("dialUpstreamTCPPlain error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if sendErr := client.SendInit(0x01); sendErr != nil {
		t.Fatalf("SendInit error = %v", sendErr)
	}

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("accept goroutine timeout")
	}
}

func TestTCPPlainUpstreamReadFrameOnClosedConnection(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		_ = conn.Close()
	}()

	client, err := dialUpstreamTCPPlain(
		context.Background(),
		listener.Addr().String(),
		time.Second,
		time.Second,
		time.Second,
	)
	if err != nil {
		t.Fatalf("dialUpstreamTCPPlain error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	_, readErr := client.ReadFrame()
	if readErr == nil {
		t.Fatalf("ReadFrame error = nil; want non-nil")
	}
}
