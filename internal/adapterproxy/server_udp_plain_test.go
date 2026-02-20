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

func TestDeliverPendingStartFromArbByteENSStarted(t *testing.T) {
	t.Parallel()

	respCh := make(chan downstream.Frame, 1)
	server := &Server{
		cfg: Config{UpstreamTransport: UpstreamENS},
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeENH,
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

func TestDeliverPendingStartFromArbByteENHFallbackDisabled(t *testing.T) {
	t.Parallel()

	respCh := make(chan downstream.Frame, 1)
	server := &Server{
		cfg: Config{UpstreamTransport: UpstreamENH},
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeENH,
			initiator: 0x31,
		},
	}

	if server.deliverPendingStartFromArbByte(0x31) {
		t.Fatalf("deliverPendingStartFromArbByte returned true; want false")
	}

	select {
	case frame := <-respCh:
		t.Fatalf("unexpected pending response delivered: cmd=0x%02X payload=%x", frame.Command, frame.Payload)
	default:
	}
}

func TestDeliverPendingStart_UDPPlainDoesNotReplyImmediatelyOnFailed(t *testing.T) {
	t.Parallel()

	respCh := make(chan downstream.Frame, 1)
	sessionState := &session{id: 1, sendCh: make(chan downstream.Frame, 1), done: make(chan struct{})}
	server := &Server{
		sessions: map[uint64]*session{1: sessionState},
		pendingStart: &pendingStart{
			sessionID: 1,
			respCh:    respCh,
			mode:      pendingStartModeUDPPlain,
			initiator: 0x31,
		},
	}

	delivered := server.deliverPendingStart(downstream.Frame{
		Command: byte(southboundenh.ENHResFailed),
		Payload: []byte{0x33},
	})
	if !delivered {
		t.Fatalf("deliverPendingStart returned false; want true")
	}

	select {
	case <-respCh:
	default:
		t.Fatalf("pending response channel did not receive frame")
	}

	select {
	case frame := <-sessionState.sendCh:
		t.Fatalf("unexpected immediate downstream reply: cmd=0x%02X payload=%x", frame.Command, frame.Payload)
	default:
	}
}

func TestAcquireLeaseRejectsDuplicateInitiatorAcrossSessions(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	if _, err := server.acquireLease(1, 0x31); err != nil {
		t.Fatalf("acquireLease(session=1) error = %v", err)
	}

	_, err := server.acquireLease(2, 0x31)
	if err == nil {
		t.Fatalf("acquireLease(session=2) error = nil; want conflict")
	}

	var conflict sourcepolicy.LeaseConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("acquireLease(session=2) error = %v; want LeaseConflictError", err)
	}
}

func TestUDPPlainRetryBackoffWithJitter(t *testing.T) {
	t.Parallel()

	base := udpPlainRetryBackoff(2)
	if got := udpPlainRetryBackoffWithJitter(2, 0, func() float64 { return 0.5 }); got != base {
		t.Fatalf("jitter disabled backoff = %s; want %s", got, base)
	}

	low := udpPlainRetryBackoffWithJitter(2, 0.2, func() float64 { return 0.0 })
	high := udpPlainRetryBackoffWithJitter(2, 0.2, func() float64 { return 1.0 })
	if low >= base {
		t.Fatalf("low jitter backoff = %s; want < %s", low, base)
	}
	if high <= base {
		t.Fatalf("high jitter backoff = %s; want > %s", high, base)
	}
	if high > udpPlainBackoffMax {
		t.Fatalf("high jitter backoff = %s; want <= %s", high, udpPlainBackoffMax)
	}
}

func TestSelectAutoInitiatorSkipsObservedAndLeased(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{AutoJoinActivityWindow: time.Minute})
	now := time.Now().UTC()

	server.observedMu.Lock()
	server.observedInitiatorAt[0xFF] = now
	server.observedMu.Unlock()

	server.leasesMu.Lock()
	server.leasedBySess[1] = sourcepolicy.Lease{OwnerID: "session/1", Address: 0xF7}
	server.leasesMu.Unlock()

	selected, err := server.selectAutoInitiator()
	if err != nil {
		t.Fatalf("selectAutoInitiator error = %v", err)
	}
	if selected != 0xF3 {
		t.Fatalf("selectAutoInitiator = 0x%02X; want 0xF3", selected)
	}
}

func TestHandleSendReturnsArbitrationFailedWhenCollisionActive(t *testing.T) {
	t.Parallel()

	sessionState := &session{
		id:     1,
		sendCh: make(chan downstream.Frame, 1),
		done:   make(chan struct{}),
	}
	server := &Server{
		sessions:           map[uint64]*session{1: sessionState},
		collisionBySession: map[uint64]byte{1: 0x33},
	}

	server.handleSend(1, 0xB5)

	select {
	case frame := <-sessionState.sendCh:
		if southboundenh.ENHCommand(frame.Command) != southboundenh.ENHResFailed {
			t.Fatalf("command = 0x%02X; want ENHResFailed", frame.Command)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != 0x33 {
			t.Fatalf("payload = %x; want [33]", frame.Payload)
		}
	default:
		t.Fatalf("expected arbitration failed reply")
	}
}
