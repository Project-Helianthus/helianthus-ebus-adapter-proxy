package ens

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

func TestDriverReadWriteLifecycle(t *testing.T) {
	parser := &ENSParser{}
	encoder := ENSEncoder{}
	connection := &stubConnection{
		readSteps: []readStep{
			{payload: []byte{ENSByteEscape, ensEscEscape}},
		},
	}
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

	frame, err := driver.Read()
	if err != nil {
		t.Fatalf("expected read success, got %v", err)
	}

	expectedReadFrame := downstream.Frame{
		Command: byte(ENSCommandData),
		Payload: []byte{ENSByteEscape},
	}
	if !reflect.DeepEqual(frame, expectedReadFrame) {
		t.Fatalf("expected frame %#v, got %#v", expectedReadFrame, frame)
	}

	err = driver.Write(downstream.Frame{
		Command: byte(ENSCommandData),
		Payload: []byte{ENSByteSync},
	})
	if err != nil {
		t.Fatalf("expected write success, got %v", err)
	}

	if dialer.calls != 1 {
		t.Fatalf("expected one dial call, got %d", dialer.calls)
	}

	if connectCalls != 1 {
		t.Fatalf("expected one connect hook call, got %d", connectCalls)
	}

	if len(connection.writes) != 1 {
		t.Fatalf("expected one write call, got %d", len(connection.writes))
	}

	expectedWrite := []byte{ENSByteEscape, ensEscSync}
	if !reflect.DeepEqual(connection.writes[0], expectedWrite) {
		t.Fatalf("expected write %#v, got %#v", expectedWrite, connection.writes[0])
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
		timeoutErr := timeoutNetworkError{message: "read timeout"}
		driver, err := NewDriver(
			&stubDialer{
				connections: []Connection{
					&stubConnection{
						readSteps: []readStep{
							{err: timeoutErr},
						},
					},
				},
			},
			&ENSParser{},
			ENSEncoder{},
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
		timeoutErr := timeoutNetworkError{message: "write timeout"}
		connection := &stubConnection{
			writeErr: timeoutErr,
		}
		dialer := &stubDialer{
			connections: []Connection{connection},
		}
		reconnectCalls := 0
		driver, err := NewDriver(
			dialer,
			&ENSParser{},
			ENSEncoder{},
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

		err = driver.Write(downstream.Frame{
			Command: byte(ENSCommandData),
			Payload: []byte{0x20},
		})
		if !errors.Is(err, ErrWriteTimeout) {
			t.Fatalf("expected write timeout error, got %v", err)
		}

		if reconnectCalls != 0 {
			t.Fatalf("expected no reconnect attempts, got %d", reconnectCalls)
		}
	})

	t.Run("dial_timeout", func(t *testing.T) {
		timeoutErr := timeoutNetworkError{message: "dial timeout"}
		driver, err := NewDriver(
			&stubDialer{
				errors: []error{timeoutErr},
			},
			&ENSParser{},
			ENSEncoder{},
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

func TestDriverReconnectsAfterReadEOF(t *testing.T) {
	firstConnection := &stubConnection{
		readSteps: []readStep{
			{err: io.EOF},
		},
	}
	secondConnection := &stubConnection{
		readSteps: []readStep{
			{payload: []byte{0x44}},
		},
	}
	dialer := &stubDialer{
		connections: []Connection{
			firstConnection,
			secondConnection,
		},
	}

	connectCalls := 0
	disconnectCauses := make([]error, 0)
	reconnectAttempts := make([]int, 0)

	driver, err := NewDriver(
		dialer,
		&ENSParser{},
		ENSEncoder{},
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
					t.Fatalf("expected io.EOF reconnect cause, got %v", cause)
				}
			},
		},
	)
	if err != nil {
		t.Fatalf("expected driver creation success, got %v", err)
	}

	frame, err := driver.Read()
	if err != nil {
		t.Fatalf("expected read success after reconnect, got %v", err)
	}

	expectedFrame := downstream.Frame{
		Command: byte(ENSCommandData),
		Payload: []byte{0x44},
	}
	if !reflect.DeepEqual(frame, expectedFrame) {
		t.Fatalf("expected frame %#v, got %#v", expectedFrame, frame)
	}

	if dialer.calls != 2 {
		t.Fatalf("expected two dial calls, got %d", dialer.calls)
	}

	if firstConnection.closeCalls != 1 {
		t.Fatalf("expected first connection close call, got %d", firstConnection.closeCalls)
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

func TestDriverMalformedDataDoesNotReconnect(t *testing.T) {
	connection := &stubConnection{
		readSteps: []readStep{
			{payload: []byte{ENSByteEscape, 0x02}},
		},
	}
	dialer := &stubDialer{
		connections: []Connection{
			connection,
			&stubConnection{},
		},
	}
	reconnectCalls := 0

	driver, err := NewDriver(
		dialer,
		&ENSParser{},
		ENSEncoder{},
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
	_, err := NewDriver(nil, &ENSParser{}, ENSEncoder{}, Options{}, Hooks{})
	if err == nil {
		t.Fatalf("expected dialer required error")
	}

	_, err = NewDriver(&stubDialer{}, nil, ENSEncoder{}, Options{}, Hooks{})
	if err == nil {
		t.Fatalf("expected parser required error")
	}

	_, err = NewDriver(&stubDialer{}, &ENSParser{}, nil, Options{}, Hooks{})
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

type readStep struct {
	payload []byte
	err     error
}

type stubConnection struct {
	readSteps []readStep
	readIndex int
	readQueue []byte

	writeErr error
	writes   [][]byte

	closeCalls         int
	readDeadlineCalls  int
	writeDeadlineCalls int
	lastReadDeadline   time.Time
	lastWriteDeadline  time.Time
}

func (connection *stubConnection) Read(buffer []byte) (int, error) {
	for {
		if len(connection.readQueue) > 0 {
			written := copy(buffer, connection.readQueue)
			connection.readQueue = connection.readQueue[written:]
			return written, nil
		}

		if connection.readIndex >= len(connection.readSteps) {
			return 0, io.EOF
		}

		step := connection.readSteps[connection.readIndex]
		connection.readIndex++

		if len(step.payload) > 0 {
			connection.readQueue = append(connection.readQueue, step.payload...)
			continue
		}

		if step.err != nil {
			return 0, step.err
		}
	}
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
