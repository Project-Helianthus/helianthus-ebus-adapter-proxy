package enh

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

func TestDriverReadAndWriteSuccessfulLifecycle(t *testing.T) {
	readFrame := downstream.Frame{
		Address: 0x21,
		Command: 0x02,
		Payload: []byte{0x30},
	}
	writeFrame := downstream.Frame{
		Address: 0x21,
		Command: 0x03,
		Payload: []byte{0x10, 0x11},
	}
	encodedPayload := []byte{0x04, 0x21, 0x03, 0x10, 0x11}

	parser := &stubParser{
		frames: []downstream.Frame{readFrame},
	}
	encoder := &stubEncoder{
		payload: encodedPayload,
	}
	connection := &stubConnection{}
	dialer := &stubDialer{
		connections: []Connection{connection},
	}

	connectCalls := 0
	driver, err := NewDriver(
		dialer,
		parser,
		encoder,
		Options{
			ReadTimeout:  time.Second,
			WriteTimeout: time.Second,
		},
		Hooks{
			OnConnect: func() {
				connectCalls++
			},
		},
	)
	if err != nil {
		t.Fatalf("expected driver creation success, got %v", err)
	}

	actualFrame, err := driver.Read()
	if err != nil {
		t.Fatalf("expected read success, got %v", err)
	}

	if !reflect.DeepEqual(actualFrame, readFrame) {
		t.Fatalf("expected frame %#v, got %#v", readFrame, actualFrame)
	}

	if err := driver.Write(writeFrame); err != nil {
		t.Fatalf("expected write success, got %v", err)
	}

	if dialer.calls != 1 {
		t.Fatalf("expected one dial call, got %d", dialer.calls)
	}

	if connectCalls != 1 {
		t.Fatalf("expected one connect hook call, got %d", connectCalls)
	}

	if parser.calls != 1 {
		t.Fatalf("expected one parser call, got %d", parser.calls)
	}

	if encoder.calls != 1 {
		t.Fatalf("expected one encoder call, got %d", encoder.calls)
	}

	if len(connection.writes) != 1 {
		t.Fatalf("expected one write call, got %d", len(connection.writes))
	}

	if !reflect.DeepEqual(connection.writes[0], encodedPayload) {
		t.Fatalf("expected payload %#v, got %#v", encodedPayload, connection.writes[0])
	}

	if connection.readDeadlineCalls != 1 {
		t.Fatalf("expected one read deadline call, got %d", connection.readDeadlineCalls)
	}

	if connection.writeDeadlineCalls != 1 {
		t.Fatalf("expected one write deadline call, got %d", connection.writeDeadlineCalls)
	}
}

func TestDriverMapsTimeouts(t *testing.T) {
	t.Run("read_timeout", func(t *testing.T) {
		timeoutErr := timeoutNetworkError{message: "read timed out"}
		driver, err := NewDriver(
			&stubDialer{connections: []Connection{&stubConnection{}}},
			&stubParser{
				errors: []error{timeoutErr},
			},
			&stubEncoder{},
			Options{ReadTimeout: time.Second},
			Hooks{},
		)
		if err != nil {
			t.Fatalf("expected driver creation success, got %v", err)
		}

		_, err = driver.Read()
		if !errors.Is(err, ErrReadTimeout) {
			t.Fatalf("expected read timeout error, got %v", err)
		}
	})

	t.Run("write_timeout", func(t *testing.T) {
		timeoutErr := timeoutNetworkError{message: "write timed out"}
		connection := &stubConnection{
			writeErr: timeoutErr,
		}
		dialer := &stubDialer{
			connections: []Connection{connection},
		}
		reconnectCalls := 0
		driver, err := NewDriver(
			dialer,
			&stubParser{},
			&stubEncoder{
				payload: []byte{0x02, 0x21, 0x09},
			},
			Options{WriteTimeout: time.Second},
			Hooks{
				OnReconnect: func(int, error) {
					reconnectCalls++
				},
			},
		)
		if err != nil {
			t.Fatalf("expected driver creation success, got %v", err)
		}

		err = driver.Write(downstream.Frame{})
		if !errors.Is(err, ErrWriteTimeout) {
			t.Fatalf("expected write timeout error, got %v", err)
		}

		if reconnectCalls != 0 {
			t.Fatalf("expected no reconnect attempts, got %d", reconnectCalls)
		}
	})

	t.Run("dial_timeout", func(t *testing.T) {
		driver, err := NewDriver(
			&stubDialer{
				errors: []error{timeoutNetworkError{message: "dial timed out"}},
			},
			&stubParser{},
			&stubEncoder{},
			Options{DialTimeout: time.Second},
			Hooks{},
		)
		if err != nil {
			t.Fatalf("expected driver creation success, got %v", err)
		}

		_, err = driver.Read()
		if !errors.Is(err, ErrDialTimeout) {
			t.Fatalf("expected dial timeout error, got %v", err)
		}
	})
}

func TestDriverReconnectsAndRetriesWrite(t *testing.T) {
	firstConnection := &stubConnection{
		writeErr: io.EOF,
	}
	secondConnection := &stubConnection{}
	dialer := &stubDialer{
		connections: []Connection{
			firstConnection,
			secondConnection,
		},
	}
	encoder := &stubEncoder{
		payload: []byte{0x02, 0x21, 0x01},
	}

	connectCalls := 0
	disconnectCauses := make([]error, 0)
	reconnectAttempts := make([]int, 0)

	driver, err := NewDriver(
		dialer,
		&stubParser{},
		encoder,
		Options{},
		Hooks{
			OnConnect: func() {
				connectCalls++
			},
			OnDisconnect: func(err error) {
				disconnectCauses = append(disconnectCauses, err)
			},
			OnReconnect: func(attempt int, cause error) {
				reconnectAttempts = append(reconnectAttempts, attempt)
				if !errors.Is(cause, io.EOF) {
					t.Fatalf("expected reconnect cause io.EOF, got %v", cause)
				}
			},
		},
	)
	if err != nil {
		t.Fatalf("expected driver creation success, got %v", err)
	}

	err = driver.Write(downstream.Frame{})
	if err != nil {
		t.Fatalf("expected write success after reconnect, got %v", err)
	}

	if dialer.calls != 2 {
		t.Fatalf("expected two dial calls, got %d", dialer.calls)
	}

	if firstConnection.closeCalls != 1 {
		t.Fatalf("expected first connection close call, got %d", firstConnection.closeCalls)
	}

	if len(secondConnection.writes) != 1 {
		t.Fatalf("expected one write on second connection, got %d", len(secondConnection.writes))
	}

	if connectCalls != 2 {
		t.Fatalf("expected two connect hook calls, got %d", connectCalls)
	}

	if len(disconnectCauses) != 1 {
		t.Fatalf("expected one disconnect hook call, got %d", len(disconnectCauses))
	}

	if !errors.Is(disconnectCauses[0], io.EOF) {
		t.Fatalf("expected disconnect cause io.EOF, got %v", disconnectCauses[0])
	}

	if !reflect.DeepEqual(reconnectAttempts, []int{1}) {
		t.Fatalf("expected reconnect attempts [1], got %v", reconnectAttempts)
	}
}

func TestDriverReturnsMalformedFrameWithoutReconnect(t *testing.T) {
	connection := &stubConnection{}
	dialer := &stubDialer{
		connections: []Connection{
			connection,
			&stubConnection{},
		},
	}
	parser := &stubParser{
		errors: []error{wrapError(ErrMalformedFrame, errors.New("invalid frame"))},
	}
	reconnectCalls := 0

	driver, err := NewDriver(
		dialer,
		parser,
		&stubEncoder{},
		Options{},
		Hooks{
			OnReconnect: func(int, error) {
				reconnectCalls++
			},
		},
	)
	if err != nil {
		t.Fatalf("expected driver creation success, got %v", err)
	}

	_, err = driver.Read()
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("expected malformed frame error, got %v", err)
	}

	if dialer.calls != 1 {
		t.Fatalf("expected one dial call, got %d", dialer.calls)
	}

	if reconnectCalls != 0 {
		t.Fatalf("expected no reconnect hook calls, got %d", reconnectCalls)
	}

	if connection.closeCalls != 0 {
		t.Fatalf("expected no connection close calls, got %d", connection.closeCalls)
	}
}

func TestNewDriverRequiresDependencies(t *testing.T) {
	_, err := NewDriver(nil, &stubParser{}, &stubEncoder{}, Options{}, Hooks{})
	if err == nil {
		t.Fatalf("expected dialer required error")
	}

	_, err = NewDriver(&stubDialer{}, nil, &stubEncoder{}, Options{}, Hooks{})
	if err == nil {
		t.Fatalf("expected parser required error")
	}

	_, err = NewDriver(&stubDialer{}, &stubParser{}, nil, Options{}, Hooks{})
	if err == nil {
		t.Fatalf("expected encoder required error")
	}
}

type stubDialer struct {
	connections []Connection
	errors      []error
	calls       int
}

func (dialer *stubDialer) DialContext(context.Context) (Connection, error) {
	callIndex := dialer.calls
	dialer.calls++

	if callIndex < len(dialer.errors) && dialer.errors[callIndex] != nil {
		return nil, dialer.errors[callIndex]
	}

	if callIndex >= len(dialer.connections) {
		return nil, errors.New("no connection available")
	}

	return dialer.connections[callIndex], nil
}

type stubParser struct {
	frames []downstream.Frame
	errors []error
	calls  int
}

func (parser *stubParser) Parse(io.Reader) (downstream.Frame, error) {
	callIndex := parser.calls
	parser.calls++

	if callIndex < len(parser.errors) && parser.errors[callIndex] != nil {
		return downstream.Frame{}, parser.errors[callIndex]
	}

	if callIndex < len(parser.frames) {
		return parser.frames[callIndex], nil
	}

	return downstream.Frame{}, io.EOF
}

type stubEncoder struct {
	payload []byte
	err     error
	calls   int
}

func (encoder *stubEncoder) Encode(downstream.Frame) ([]byte, error) {
	encoder.calls++

	if encoder.err != nil {
		return nil, encoder.err
	}

	if encoder.payload == nil {
		return []byte{0x02, 0x20, 0x01}, nil
	}

	return append([]byte(nil), encoder.payload...), nil
}

type stubConnection struct {
	writeErr           error
	writes             [][]byte
	closeCalls         int
	readDeadlineCalls  int
	writeDeadlineCalls int
	lastReadDeadline   time.Time
	lastWriteDeadline  time.Time
}

func (connection *stubConnection) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (connection *stubConnection) Write(payload []byte) (int, error) {
	if connection.writeErr != nil {
		return 0, connection.writeErr
	}

	connection.writes = append(connection.writes, append([]byte(nil), payload...))
	return len(payload), nil
}

func (connection *stubConnection) Close() error {
	connection.closeCalls++
	return nil
}

func (connection *stubConnection) SetReadDeadline(deadline time.Time) error {
	connection.readDeadlineCalls++
	connection.lastReadDeadline = deadline
	return nil
}

func (connection *stubConnection) SetWriteDeadline(deadline time.Time) error {
	connection.writeDeadlineCalls++
	connection.lastWriteDeadline = deadline
	return nil
}

type timeoutNetworkError struct {
	message string
}

func (err timeoutNetworkError) Error() string {
	return err.message
}

func (timeoutNetworkError) Timeout() bool {
	return true
}

func (timeoutNetworkError) Temporary() bool {
	return true
}

var _ net.Error = timeoutNetworkError{}
