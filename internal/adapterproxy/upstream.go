package adapterproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

type upstream interface {
	Close() error
	ReadFrame() (downstream.Frame, error)
	WriteFrame(frame downstream.Frame) error
	SendInit(features byte) error
}

type upstreamClient struct {
	conn         net.Conn
	reader       *bufio.Reader
	readTimeout  time.Duration
	writeTimeout time.Duration

	parser  southboundenh.ENHParser
	encoder southboundenh.ENHEncoder

	writeMu sync.Mutex
}

func dialUpstream(
	ctx context.Context,
	transport UpstreamTransport,
	address string,
	timeout time.Duration,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) (upstream, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	switch transport {
	case UpstreamUDPPlain:
		return dialUpstreamUDPPlain(ctx, address, timeout, readTimeout, writeTimeout)
	case UpstreamENH, UpstreamENS, "":
		return dialUpstreamENH(ctx, address, timeout, readTimeout, writeTimeout)
	default:
		return nil, fmt.Errorf("unsupported upstream transport %q", transport)
	}
}

func (client *upstreamClient) Close() error {
	if client == nil || client.conn == nil {
		return nil
	}
	return client.conn.Close()
}

func (client *upstreamClient) ReadFrame() (downstream.Frame, error) {
	if client == nil || client.conn == nil {
		return downstream.Frame{}, io.EOF
	}

	if err := setReadDeadline(client.conn, client.readTimeout); err != nil {
		return downstream.Frame{}, err
	}

	return client.parser.Parse(client.reader)
}

func (client *upstreamClient) WriteFrame(frame downstream.Frame) error {
	if client == nil || client.conn == nil {
		return io.EOF
	}

	payload, err := encodeUpstreamFrame(client.encoder, frame)
	if err != nil {
		return err
	}

	client.writeMu.Lock()
	defer client.writeMu.Unlock()

	if err := setWriteDeadline(client.conn, client.writeTimeout); err != nil {
		return err
	}

	if err := writeAll(client.conn, payload); err != nil {
		return err
	}

	return nil
}

func (client *upstreamClient) SendInit(features byte) error {
	return client.WriteFrame(downstream.Frame{
		Command: byte(southboundenh.ENHReqInit),
		Payload: []byte{features},
	})
}

func dialUpstreamENH(
	ctx context.Context,
	address string,
	timeout time.Duration,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) (*upstreamClient, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	return &upstreamClient{
		conn:         conn,
		reader:       bufio.NewReaderSize(conn, 4096),
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
	}, nil
}

type udpPlainUpstreamClient struct {
	conn         *net.UDPConn
	readTimeout  time.Duration
	writeTimeout time.Duration

	readMu  sync.Mutex
	writeMu sync.Mutex

	pending []byte
	buffer  []byte
}

const udpPlainReadBufferSize = 65535

func dialUpstreamUDPPlain(
	ctx context.Context,
	address string,
	timeout time.Duration,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) (*udpPlainUpstreamClient, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return nil, err
	}

	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("udp dial did not return UDPConn")
	}

	return &udpPlainUpstreamClient{
		conn:         udpConn,
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
		buffer:       make([]byte, udpPlainReadBufferSize),
	}, nil
}

func (client *udpPlainUpstreamClient) Close() error {
	if client == nil || client.conn == nil {
		return nil
	}
	return client.conn.Close()
}

func (client *udpPlainUpstreamClient) ReadFrame() (downstream.Frame, error) {
	if client == nil || client.conn == nil {
		return downstream.Frame{}, io.EOF
	}

	client.readMu.Lock()
	defer client.readMu.Unlock()

	for {
		if len(client.pending) > 0 {
			value := client.pending[0]
			client.pending = client.pending[1:]
			return downstream.Frame{
				Command: byte(southboundenh.ENHResReceived),
				Payload: []byte{value},
			}, nil
		}

		if err := setReadDeadline(client.conn, client.readTimeout); err != nil {
			return downstream.Frame{}, err
		}

		n, err := client.conn.Read(client.buffer)
		if err != nil {
			return downstream.Frame{}, err
		}
		if n == 0 {
			continue
		}
		client.pending = append(client.pending, client.buffer[:n]...)
	}
}

func (client *udpPlainUpstreamClient) WriteFrame(frame downstream.Frame) error {
	if client == nil || client.conn == nil {
		return io.EOF
	}

	if len(frame.Payload) != 1 {
		return fmt.Errorf("udp-plain upstream requires exactly one byte payload, got %d", len(frame.Payload))
	}

	command := southboundenh.ENHCommand(frame.Command)
	switch command {
	case southboundenh.ENHReqSend:
	case southboundenh.ENHReqInit:
		return nil
	default:
		return fmt.Errorf("udp-plain upstream does not support command 0x%02X", frame.Command)
	}

	client.writeMu.Lock()
	defer client.writeMu.Unlock()

	if err := setWriteDeadline(client.conn, client.writeTimeout); err != nil {
		return err
	}

	_, err := client.conn.Write(frame.Payload)
	return err
}

func (client *udpPlainUpstreamClient) SendInit(features byte) error {
	return nil
}

func setReadDeadline(connection net.Conn, timeout time.Duration) error {
	if timeout <= 0 {
		return connection.SetReadDeadline(time.Time{})
	}
	return connection.SetReadDeadline(time.Now().Add(timeout))
}

func setWriteDeadline(connection net.Conn, timeout time.Duration) error {
	if timeout <= 0 {
		return connection.SetWriteDeadline(time.Time{})
	}
	return connection.SetWriteDeadline(time.Now().Add(timeout))
}

func writeAll(writer io.Writer, payload []byte) error {
	remaining := payload
	for len(remaining) > 0 {
		written, err := writer.Write(remaining)
		if err != nil {
			return err
		}
		if written <= 0 {
			return fmt.Errorf("short write")
		}
		remaining = remaining[written:]
	}
	return nil
}
