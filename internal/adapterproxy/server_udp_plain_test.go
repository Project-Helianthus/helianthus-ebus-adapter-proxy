package adapterproxy

import (
	"errors"
	"testing"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/sourcepolicy"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func TestUDPPlainRetryBackoffCapped(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: -1, want: udpPlainBackoffBase},
		{attempt: 0, want: udpPlainBackoffBase},
		{attempt: 1, want: 50 * time.Millisecond},
		{attempt: 2, want: 100 * time.Millisecond},
		{attempt: 3, want: 200 * time.Millisecond},
		{attempt: 4, want: udpPlainBackoffMax},
		{attempt: 8, want: udpPlainBackoffMax},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(time.Duration(testCase.attempt).String(), func(t *testing.T) {
			t.Parallel()
			got := udpPlainRetryBackoff(testCase.attempt)
			if got != testCase.want {
				t.Fatalf("udpPlainRetryBackoff(%d) = %s; want %s", testCase.attempt, got, testCase.want)
			}
		})
	}
}

func TestDeliverPendingStartFromArbByteStarted(t *testing.T) {
	t.Parallel()

	respCh := make(chan downstream.Frame, 1)
	server := &Server{
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeUDPPlain,
			initiator: 0x31,
		},
	}

	if !server.deliverPendingStartFromArbByte(0x31) {
		t.Fatalf("deliverPendingStartFromArbByte returned false; want true")
	}

	select {
	case frame := <-respCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResStarted {
			t.Fatalf("command = 0x%02X; want ENHResStarted", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x31 {
			t.Fatalf("payload = %x; want [31]", frame.Payload)
		}
	default:
		t.Fatalf("no pending response delivered")
	}
}

func TestDeliverPendingStartFromArbByteFailed(t *testing.T) {
	t.Parallel()

	respCh := make(chan downstream.Frame, 1)
	server := &Server{
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeUDPPlain,
			initiator: 0x31,
		},
	}

	if !server.deliverPendingStartFromArbByte(0x33) {
		t.Fatalf("deliverPendingStartFromArbByte returned false; want true")
	}

	select {
	case frame := <-respCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResFailed {
			t.Fatalf("command = 0x%02X; want ENHResFailed", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x33 {
			t.Fatalf("payload = %x; want [33]", frame.Payload)
		}
	default:
		t.Fatalf("no pending response delivered")
	}
}

func TestAcquireLeaseRejectsDuplicateInitiatorAcrossSessions(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	if err := server.acquireLease(1, 0x31); err != nil {
		t.Fatalf("acquireLease(session=1) error = %v", err)
	}

	err := server.acquireLease(2, 0x31)
	if err == nil {
		t.Fatalf("acquireLease(session=2) error = nil; want conflict")
	}

	var conflict sourcepolicy.LeaseConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("acquireLease(session=2) error = %v; want LeaseConflictError", err)
	}
}
