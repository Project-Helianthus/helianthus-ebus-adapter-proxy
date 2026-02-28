package enh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

var (
	ErrDialTimeout  = errors.New("enh dial timeout")
	ErrReadTimeout  = errors.New("enh read timeout")
	ErrWriteTimeout = errors.New("enh write timeout")
)

type Connection interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

type Dialer interface {
	DialContext(context.Context) (Connection, error)
}

type Parser interface {
	Parse(io.Reader) (downstream.Frame, error)
}

type Encoder interface {
	Encode(downstream.Frame) ([]byte, error)
}

type Hooks struct {
	OnConnect    func()
	OnDisconnect func(error)
	OnReconnect  func(attempt int, cause error)
}

type Options struct {
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

type Driver struct {
	mutex             sync.Mutex
	dialer            Dialer
	parser            Parser
	encoder           Encoder
	options           Options
	hooks             Hooks
	connection        Connection
	reconnectAttempts int
}

type NetDialer struct {
	Network string
	Address string
	Dialer  *net.Dialer
}

func (dialer NetDialer) DialContext(ctx context.Context) (Connection, error) {
	network := dialer.Network
	if network == "" {
		network = "tcp"
	}

	baseDialer := dialer.Dialer
	if baseDialer == nil {
		baseDialer = &net.Dialer{}
	}

	connection, err := baseDialer.DialContext(ctx, network, dialer.Address)
	if err != nil {
		return nil, err
	}

	return connection, nil
}

func NewDriver(
	dialer Dialer,
	parser Parser,
	encoder Encoder,
	options Options,
	hooks Hooks,
) (*Driver, error) {
	if dialer == nil {
		return nil, errors.New("dialer is required")
	}

	if parser == nil {
		return nil, errors.New("parser is required")
	}

	if encoder == nil {
		return nil, errors.New("encoder is required")
	}

	return &Driver{
		dialer:  dialer,
		parser:  parser,
		encoder: encoder,
		options: options,
		hooks:   hooks,
	}, nil
}

func (driver *Driver) Read() (downstream.Frame, error) {
	driver.mutex.Lock()
	defer driver.mutex.Unlock()

	frame, err := driver.readLocked()
	if err == nil {
		return frame, nil
	}

	if !driver.shouldReconnect(err) {
		return downstream.Frame{}, err
	}

	if reconnectErr := driver.reconnectLocked(err); reconnectErr != nil {
		return downstream.Frame{}, reconnectErr
	}

	return driver.readLocked()
}

func (driver *Driver) Write(frame downstream.Frame) error {
	driver.mutex.Lock()
	defer driver.mutex.Unlock()

	err := driver.writeLocked(frame)
	if err == nil {
		return nil
	}

	if !driver.shouldReconnect(err) {
		return err
	}

	if reconnectErr := driver.reconnectLocked(err); reconnectErr != nil {
		return reconnectErr
	}

	return driver.writeLocked(frame)
}

func (driver *Driver) Close() error {
	driver.mutex.Lock()
	defer driver.mutex.Unlock()

	return driver.closeLocked(nil)
}

func (driver *Driver) readLocked() (downstream.Frame, error) {
	connection, err := driver.ensureConnectedLocked()
	if err != nil {
		return downstream.Frame{}, err
	}

	if err := setReadDeadline(connection, driver.options.ReadTimeout); err != nil {
		return downstream.Frame{}, err
	}

	frame, err := driver.parser.Parse(connection)
	if err != nil {
		if isTimeoutError(err) {
			resetIfSupported(driver.parser)
			return downstream.Frame{}, wrapError(ErrReadTimeout, err)
		}

		return downstream.Frame{}, err
	}

	return frame, nil
}

func (driver *Driver) writeLocked(frame downstream.Frame) error {
	connection, err := driver.ensureConnectedLocked()
	if err != nil {
		return err
	}

	payload, err := driver.encoder.Encode(frame)
	if err != nil {
		return err
	}

	if err := setWriteDeadline(connection, driver.options.WriteTimeout); err != nil {
		return err
	}

	if err := writeAll(connection, payload); err != nil {
		if isTimeoutError(err) {
			return wrapError(ErrWriteTimeout, err)
		}

		return err
	}

	return nil
}

func (driver *Driver) ensureConnectedLocked() (Connection, error) {
	if driver.connection != nil {
		return driver.connection, nil
	}

	ctx := context.Background()
	cancel := func() {}

	if driver.options.DialTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, driver.options.DialTimeout)
	}
	defer cancel()

	connection, err := driver.dialer.DialContext(ctx)
	if err != nil {
		if isTimeoutError(err) || errors.Is(err, context.DeadlineExceeded) {
			return nil, wrapError(ErrDialTimeout, err)
		}

		return nil, err
	}

	driver.connection = connection
	callHook(driver.hooks.OnConnect)

	return connection, nil
}

func (driver *Driver) reconnectLocked(cause error) error {
	if err := driver.closeLocked(cause); err != nil {
		return err
	}

	driver.reconnectAttempts++
	if driver.hooks.OnReconnect != nil {
		driver.hooks.OnReconnect(driver.reconnectAttempts, cause)
	}

	_, err := driver.ensureConnectedLocked()
	return err
}

func (driver *Driver) closeLocked(cause error) error {
	if driver.connection == nil {
		return nil
	}

	connection := driver.connection
	driver.connection = nil

	callHookWithError(driver.hooks.OnDisconnect, cause)

	if err := connection.Close(); err != nil {
		return err
	}

	return nil
}

func (driver *Driver) shouldReconnect(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrReadTimeout):
		return false
	case errors.Is(err, ErrWriteTimeout):
		return false
	case errors.Is(err, ErrDialTimeout):
		return false
	case errors.Is(err, ErrMalformedFrame):
		return false
	case errors.Is(err, io.EOF):
		return true
	case errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.Is(err, net.ErrClosed):
		return true
	}

	var netError net.Error
	if errors.As(err, &netError) {
		return !netError.Timeout()
	}

	return false
}

func setReadDeadline(connection Connection, timeout time.Duration) error {
	if timeout <= 0 {
		return connection.SetReadDeadline(time.Time{})
	}

	return connection.SetReadDeadline(time.Now().Add(timeout))
}

func setWriteDeadline(connection Connection, timeout time.Duration) error {
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

		remaining = remaining[written:]
	}

	return nil
}

func wrapError(base error, err error) error {
	return fmt.Errorf("%w: %v", base, err)
}

func isTimeoutError(err error) bool {
	var netError net.Error
	if errors.As(err, &netError) {
		return netError.Timeout()
	}

	return false
}

func callHook(hook func()) {
	if hook != nil {
		hook()
	}
}

func callHookWithError(hook func(error), err error) {
	if hook != nil {
		hook(err)
	}
}

func resetIfSupported(parser any) {
	resetter, ok := parser.(interface{ Reset() })
	if ok {
		resetter.Reset()
	}
}
