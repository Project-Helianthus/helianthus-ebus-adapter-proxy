package enh

import (
	"context"
	"errors"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

type Parser interface {
	Parse(io.Reader) (downstream.Frame, error)
}

type ParserFactory func() Parser

type FrameHandler func(context.Context, SessionInfo, downstream.Frame) error

type Hooks struct {
	OnConnect    func(SessionInfo)
	OnDisconnect func(SessionInfo, error)
	OnError      func(SessionInfo, error)
}

type Options struct {
	ReadTimeout   time.Duration
	ParserFactory ParserFactory
}

type SessionInfo struct {
	ID          uint64
	RemoteAddr  string
	ConnectedAt time.Time
}

type Metrics struct {
	ActiveSessions   int
	TotalConnections uint64
	TotalDisconnects uint64
	TotalErrors      uint64
	TotalFrames      uint64
}

type Listener struct {
	listener net.Listener
	handler  FrameHandler
	hooks    Hooks
	options  Options

	mutex         sync.Mutex
	sessions      map[uint64]sessionState
	nextSessionID uint64
	metrics       Metrics
	closed        bool

	waitGroup sync.WaitGroup
	closeOnce sync.Once
}

type sessionState struct {
	info SessionInfo
	conn net.Conn
}

func NewListener(
	listener net.Listener,
	handler FrameHandler,
	options Options,
	hooks Hooks,
) (*Listener, error) {
	if listener == nil {
		return nil, errors.New("listener is required")
	}

	if options.ParserFactory == nil {
		options.ParserFactory = func() Parser {
			return &southboundenh.ENHParser{}
		}
	}

	if handler == nil {
		handler = func(context.Context, SessionInfo, downstream.Frame) error {
			return nil
		}
	}

	return &Listener{
		listener: listener,
		handler:  handler,
		hooks:    hooks,
		options:  options,
		sessions: make(map[uint64]sessionState),
	}, nil
}

func (listener *Listener) Serve(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	stopContextWatcher := make(chan struct{})
	defer close(stopContextWatcher)

	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stopContextWatcher:
		}
	}()

	for {
		connection, err := listener.listener.Accept()
		if err != nil {
			if listener.isClosedError(err) || ctx.Err() != nil || listener.isClosed() {
				return nil
			}

			listener.recordError(SessionInfo{}, err)
			continue
		}

		sessionInfo := listener.registerSession(connection)
		parser := listener.options.ParserFactory()

		listener.waitGroup.Add(1)
		go listener.serveSession(ctx, sessionInfo, connection, parser)
	}
}

func (listener *Listener) Close() error {
	var closeErr error

	listener.closeOnce.Do(func() {
		activeConnections := listener.markClosedAndCollectConnections()

		if err := listener.listener.Close(); err != nil && !listener.isClosedError(err) {
			closeErr = err
		}

		for _, connection := range activeConnections {
			_ = connection.Close()
		}

		listener.waitGroup.Wait()
	})

	return closeErr
}

func (listener *Listener) Sessions() []SessionInfo {
	listener.mutex.Lock()
	sessions := make([]SessionInfo, 0, len(listener.sessions))
	for _, state := range listener.sessions {
		sessions = append(sessions, state.info)
	}
	listener.mutex.Unlock()

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	return sessions
}

func (listener *Listener) Metrics() Metrics {
	listener.mutex.Lock()
	metrics := listener.metrics
	listener.mutex.Unlock()

	return metrics
}

func (listener *Listener) serveSession(
	ctx context.Context,
	sessionInfo SessionInfo,
	connection net.Conn,
	parser Parser,
) {
	defer listener.waitGroup.Done()
	defer func() {
		_ = connection.Close()
	}()

	var disconnectCause error = io.EOF
	defer func() {
		listener.unregisterSession(sessionInfo.ID, disconnectCause)
	}()

	for {
		if err := setReadDeadline(connection, listener.options.ReadTimeout); err != nil {
			disconnectCause = err
			listener.recordError(sessionInfo, err)
			return
		}

		frame, err := parser.Parse(connection)
		if err != nil {
			switch {
			case isTimeoutError(err):
				continue
			case errors.Is(err, io.EOF) || listener.isClosedError(err):
				disconnectCause = err
				return
			case errors.Is(err, southboundenh.ErrMalformedFrame):
				listener.recordError(sessionInfo, err)
				continue
			default:
				disconnectCause = err
				listener.recordError(sessionInfo, err)
				return
			}
		}

		listener.recordFrame()
		if err := listener.handler(ctx, sessionInfo, frame); err != nil {
			listener.recordError(sessionInfo, err)
		}
	}
}

func (listener *Listener) registerSession(connection net.Conn) SessionInfo {
	listener.mutex.Lock()
	listener.nextSessionID++
	sessionInfo := SessionInfo{
		ID:          listener.nextSessionID,
		RemoteAddr:  connection.RemoteAddr().String(),
		ConnectedAt: time.Now().UTC(),
	}
	listener.sessions[sessionInfo.ID] = sessionState{
		info: sessionInfo,
		conn: connection,
	}
	listener.metrics.ActiveSessions++
	listener.metrics.TotalConnections++
	connectHook := listener.hooks.OnConnect
	listener.mutex.Unlock()

	if connectHook != nil {
		connectHook(sessionInfo)
	}

	return sessionInfo
}

func (listener *Listener) unregisterSession(sessionID uint64, cause error) {
	listener.mutex.Lock()
	state, found := listener.sessions[sessionID]
	if found {
		delete(listener.sessions, sessionID)
		if listener.metrics.ActiveSessions > 0 {
			listener.metrics.ActiveSessions--
		}
		listener.metrics.TotalDisconnects++
	}
	disconnectHook := listener.hooks.OnDisconnect
	listener.mutex.Unlock()

	if found && disconnectHook != nil {
		disconnectHook(state.info, cause)
	}
}

func (listener *Listener) recordError(sessionInfo SessionInfo, err error) {
	listener.mutex.Lock()
	listener.metrics.TotalErrors++
	errorHook := listener.hooks.OnError
	listener.mutex.Unlock()

	if errorHook != nil {
		errorHook(sessionInfo, err)
	}
}

func (listener *Listener) recordFrame() {
	listener.mutex.Lock()
	listener.metrics.TotalFrames++
	listener.mutex.Unlock()
}

func (listener *Listener) markClosedAndCollectConnections() []net.Conn {
	listener.mutex.Lock()
	listener.closed = true

	activeConnections := make([]net.Conn, 0, len(listener.sessions))
	for _, state := range listener.sessions {
		activeConnections = append(activeConnections, state.conn)
	}

	listener.mutex.Unlock()
	return activeConnections
}

func (listener *Listener) isClosed() bool {
	listener.mutex.Lock()
	closed := listener.closed
	listener.mutex.Unlock()

	return closed
}

func (listener *Listener) isClosedError(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return true
	}

	if err == nil {
		return false
	}

	return strings.Contains(err.Error(), "use of closed network connection")
}

func setReadDeadline(connection net.Conn, timeout time.Duration) error {
	if timeout <= 0 {
		return connection.SetReadDeadline(time.Time{})
	}

	return connection.SetReadDeadline(time.Now().Add(timeout))
}

func isTimeoutError(err error) bool {
	var netError net.Error
	if errors.As(err, &netError) {
		return netError.Timeout()
	}

	return false
}
