package adapterproxy

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
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
