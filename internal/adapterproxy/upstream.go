package adapterproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

type upstreamClient struct {
	conn         net.Conn
	readTimeout  time.Duration
	writeTimeout time.Duration

	parser  southboundenh.ENHParser
	encoder southboundenh.ENHEncoder

	writeMu sync.Mutex
}

func dialUpstream(
	ctx context.Context,
	address string,
	timeout time.Duration,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) (*upstreamClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}

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
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
	}, nil
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

	return client.parser.Parse(client.conn)
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
