package adapterproxy

import (
	"bufio"
	"io"
	"net"
	"sync"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

type session struct {
	id         uint64
	remoteAddr string

	conn         net.Conn
	reader       *bufio.Reader
	readTimeout  time.Duration
	writeTimeout time.Duration

	parser  requestParser
	encoder southboundenh.ENHEncoder

	sendCh chan downstream.Frame

	closeOnce sync.Once
	done      chan struct{}
}

const (
	defaultSessionSendBuffer = 8192
)

func newSession(
	id uint64,
	conn net.Conn,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) *session {
	enableLowLatencySocketOptions(conn)

	return &session{
		id:           id,
		remoteAddr:   conn.RemoteAddr().String(),
		conn:         conn,
		reader:       bufio.NewReaderSize(conn, 4096),
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
		sendCh:       make(chan downstream.Frame, defaultSessionSendBuffer),
		done:         make(chan struct{}),
	}
}

func (s *session) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.done)
		closeErr = s.conn.Close()
	})
	return closeErr
}

func (s *session) enqueue(frame downstream.Frame) bool {
	select {
	case s.sendCh <- cloneFrame(frame):
		return true
	default:
		return false
	}
}

func (s *session) runWriter(onError func(error)) {
	for {
		select {
		case <-s.done:
			return
		case frame := <-s.sendCh:
			payload, err := encodeDownstreamFrame(s.encoder, frame)
			if err != nil {
				if onError != nil {
					onError(err)
				}
				continue
			}

			_ = setWriteDeadline(s.conn, s.writeTimeout)
			if err := writeAll(s.conn, payload); err != nil {
				if onError != nil {
					onError(err)
				}
				_ = s.Close()
				return
			}
		}
	}
}

func (s *session) runReader(onFrame func(downstream.Frame), onError func(error)) {
	for {
		select {
		case <-s.done:
			return
		default:
		}

		_ = setReadDeadline(s.conn, s.readTimeout)
		frame, err := s.parser.Parse(s.reader)
		if err != nil {
			if isTimeoutError(err) {
				s.parser.Reset()
				continue
			}
			if err == io.EOF {
				_ = s.Close()
				return
			}
			if onError != nil {
				onError(err)
			}
			_ = s.Close()
			return
		}

		if onFrame != nil {
			onFrame(frame)
		}
	}
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
}

func cloneFrame(frame downstream.Frame) downstream.Frame {
	return downstream.Frame{
		Address: frame.Address,
		Command: frame.Command,
		Payload: append([]byte(nil), frame.Payload...),
	}
}

func enableLowLatencySocketOptions(connection net.Conn) {
	tcpConn, ok := connection.(*net.TCPConn)
	if !ok {
		return
	}

	_ = tcpConn.SetNoDelay(true)
	_ = tcpConn.SetKeepAlive(true)
	_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
}
