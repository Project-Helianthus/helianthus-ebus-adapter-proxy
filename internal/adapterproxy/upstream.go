package adapterproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
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
	case UpstreamTCPPlain:
		return dialUpstreamTCPPlain(ctx, address, timeout, readTimeout, writeTimeout)
	case UpstreamENH, UpstreamENS, "":
		// PX51/PX58: The ENS wire codec cannot carry control frames, but
		// ens:// is historically an alias for the ENH-framed adapter path
		// in ebusd and helianthus-ebusgo. Route both to the ENH dialer.
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

	frame, err := client.parser.Parse(client.reader)
	if err != nil {
		client.parser.Reset()
		// PX55: Also reset bufio.Reader to discard stale buffered bytes.
		// If a timeout fires mid-ENH-byte-pair, the first byte remains in
		// the buffer and would be misinterpreted as a command byte on the
		// next ReadFrame call.
		if isTimeoutError(err) {
			client.reader.Reset(client.conn)
		}
		return frame, err
	}

	// PX16/PX23: Reset parser after arbitration-terminal frames (STARTED,
	// FAILED, RESETTED) per enh.md:128. The parser accumulates pending byte
	// state that must be cleared when the adapter resets its own encoder.
	cmd := southboundenh.ENHCommand(frame.Command)
	switch cmd {
	case southboundenh.ENHResStarted, southboundenh.ENHResFailed, southboundenh.ENHResResetted:
		client.parser.Reset()
	}

	return frame, nil
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

	// PX54: Apply default timeout for ENH upstream if none configured,
	// to prevent indefinite blocking on a hung adapter.
	if readTimeout <= 0 {
		readTimeout = defaultENHIOTimeout
	}
	if writeTimeout <= 0 {
		writeTimeout = defaultENHIOTimeout
	}
	return &upstreamClient{
		conn:         conn,
		reader:       bufio.NewReaderSize(conn, 4096),
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
	}, nil
}

// PX41: Use a ring buffer for pending bytes to prevent unbounded growth
// and avoid backing-array leaks from head-chop slicing.
type udpPlainUpstreamClient struct {
	conn         *net.UDPConn
	readTimeout  time.Duration
	writeTimeout time.Duration

	readMu  sync.Mutex
	writeMu sync.Mutex

	pending    []byte
	pendingOff int // PX41: read offset into pending to avoid head-chop
	buffer     []byte
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
		// PX41: Use offset tracking to avoid O(n) head-chop on every byte.
		if client.pendingOff < len(client.pending) {
			value := client.pending[client.pendingOff]
			client.pendingOff++
			// Reclaim backing array when fully consumed.
			if client.pendingOff == len(client.pending) {
				client.pending = client.pending[:0]
				client.pendingOff = 0
			}
			return downstream.Frame{
				Command: byte(southboundenh.ENHResReceived),
				Payload: []byte{value},
			}, nil
		}

		// Reset pending for next datagram.
		client.pending = client.pending[:0]
		client.pendingOff = 0

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

type tcpPlainUpstreamClient struct {
	conn         net.Conn
	reader       *bufio.Reader
	readTimeout  time.Duration
	writeTimeout time.Duration

	readMu  sync.Mutex
	writeMu sync.Mutex
}

func dialUpstreamTCPPlain(
	ctx context.Context,
	address string,
	timeout time.Duration,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) (*tcpPlainUpstreamClient, error) {
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
	return &tcpPlainUpstreamClient{
		conn:         conn,
		reader:       bufio.NewReaderSize(conn, 4096),
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
	}, nil
}

func (client *tcpPlainUpstreamClient) Close() error {
	if client == nil || client.conn == nil {
		return nil
	}
	return client.conn.Close()
}

func (client *tcpPlainUpstreamClient) ReadFrame() (downstream.Frame, error) {
	if client == nil || client.conn == nil {
		return downstream.Frame{}, io.EOF
	}
	client.readMu.Lock()
	defer client.readMu.Unlock()

	if err := setReadDeadline(client.conn, client.readTimeout); err != nil {
		return downstream.Frame{}, err
	}
	value, err := client.reader.ReadByte()
	if err != nil {
		return downstream.Frame{}, err
	}
	return downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{value},
	}, nil
}

func (client *tcpPlainUpstreamClient) WriteFrame(frame downstream.Frame) error {
	if client == nil || client.conn == nil {
		return io.EOF
	}
	if len(frame.Payload) != 1 {
		return fmt.Errorf("tcp-plain upstream requires exactly one byte payload, got %d", len(frame.Payload))
	}
	command := southboundenh.ENHCommand(frame.Command)
	switch command {
	case southboundenh.ENHReqSend:
	case southboundenh.ENHReqInit:
		return nil
	default:
		return fmt.Errorf("tcp-plain upstream does not support command 0x%02X", frame.Command)
	}

	client.writeMu.Lock()
	defer client.writeMu.Unlock()

	if err := setWriteDeadline(client.conn, client.writeTimeout); err != nil {
		return err
	}
	_, err := client.conn.Write(frame.Payload)
	return err
}

func (client *tcpPlainUpstreamClient) SendInit(features byte) error {
	return nil
}

// PX54/AT-03: Default I/O timeout for ENH upstream connections to prevent
// indefinite blocking on a hung adapter. Plain transports (UDP/TCP) may
// legitimately have long inter-telegram gaps, so they use the caller-provided
// timeout (including zero = no deadline).
const defaultENHIOTimeout = 30 * time.Second

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
