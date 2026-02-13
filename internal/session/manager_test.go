package session

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

func TestManagerConnectDisconnectReconnectLifecycle(t *testing.T) {
	var (
		eventsMu sync.Mutex
		events   []Event
	)

	manager := NewManager(
		Options{
			InboundCapacity:  2,
			OutboundCapacity: 2,
		},
		Hooks{
			OnConnect: func(event Event) {
				eventsMu.Lock()
				events = append(events, event)
				eventsMu.Unlock()
			},
			OnDisconnect: func(event Event) {
				eventsMu.Lock()
				events = append(events, event)
				eventsMu.Unlock()
			},
			OnReconnect: func(event Event) {
				eventsMu.Lock()
				events = append(events, event)
				eventsMu.Unlock()
			},
		},
	)

	identity := Identity{
		ClientID:   "client-a",
		Protocol:   "enh",
		RemoteAddr: "127.0.0.1:10001",
	}

	connectedSession, err := manager.Register(identity)
	if err != nil {
		t.Fatalf("expected register success, got %v", err)
	}

	if connectedSession.ID == 0 {
		t.Fatalf("expected non-zero session ID")
	}

	if !connectedSession.Connected {
		t.Fatalf("expected connected session")
	}

	disconnectCause := errors.New("connection closed")
	disconnectedSession, err := manager.Unregister(connectedSession.ID, disconnectCause)
	if err != nil {
		t.Fatalf("expected unregister success, got %v", err)
	}

	if disconnectedSession.Connected {
		t.Fatalf("expected disconnected session")
	}

	if disconnectedSession.DisconnectedAt.IsZero() {
		t.Fatalf("expected disconnection timestamp")
	}

	reconnectedSession, err := manager.Reconnect(identity)
	if err != nil {
		t.Fatalf("expected reconnect success, got %v", err)
	}

	if reconnectedSession.ID != connectedSession.ID {
		t.Fatalf("expected stable session ID %d, got %d", connectedSession.ID, reconnectedSession.ID)
	}

	if !reconnectedSession.Connected {
		t.Fatalf("expected connected session after reconnect")
	}

	if reconnectedSession.ReconnectCount != 1 {
		t.Fatalf("expected reconnect count 1, got %d", reconnectedSession.ReconnectCount)
	}

	activeSessions := manager.ActiveSessions()
	if len(activeSessions) != 1 {
		t.Fatalf("expected one active session, got %d", len(activeSessions))
	}

	if activeSessions[0].ID != connectedSession.ID {
		t.Fatalf("expected active session ID %d, got %d", connectedSession.ID, activeSessions[0].ID)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()

	if len(events) != 3 {
		t.Fatalf("expected three lifecycle events, got %d", len(events))
	}

	if events[0].Type != EventTypeConnected {
		t.Fatalf("expected first event connected, got %s", events[0].Type)
	}

	if events[1].Type != EventTypeDisconnected {
		t.Fatalf("expected second event disconnected, got %s", events[1].Type)
	}

	if !errors.Is(events[1].Cause, disconnectCause) {
		t.Fatalf("expected disconnect cause %v, got %v", disconnectCause, events[1].Cause)
	}

	if events[2].Type != EventTypeReconnected {
		t.Fatalf("expected third event reconnected, got %s", events[2].Type)
	}

	for index := range events {
		if events[index].Session.ID != connectedSession.ID {
			t.Fatalf(
				"expected stable ID %d across events, got %d at index %d",
				connectedSession.ID,
				events[index].Session.ID,
				index,
			)
		}
	}
}

func TestManagerPerSessionQueueIsolation(t *testing.T) {
	manager := NewManager(
		Options{
			InboundCapacity:  4,
			OutboundCapacity: 4,
		},
		Hooks{},
	)

	sessionA, err := manager.Register(Identity{
		ClientID:   "client-a",
		Protocol:   "ens",
		RemoteAddr: "127.0.0.1:20001",
	})
	if err != nil {
		t.Fatalf("expected register sessionA success, got %v", err)
	}

	sessionB, err := manager.Register(Identity{
		ClientID:   "client-b",
		Protocol:   "ens",
		RemoteAddr: "127.0.0.1:20002",
	})
	if err != nil {
		t.Fatalf("expected register sessionB success, got %v", err)
	}

	inboundA := downstream.Frame{Command: 0x01, Payload: []byte{0x10}}
	inboundB := downstream.Frame{Command: 0x02, Payload: []byte{0x20}}
	outboundA := downstream.Frame{Command: 0x03, Payload: []byte{0x30}}
	outboundB := downstream.Frame{Command: 0x04, Payload: []byte{0x40}}

	if err := manager.EnqueueInbound(sessionA.ID, inboundA); err != nil {
		t.Fatalf("expected enqueue inbound sessionA success, got %v", err)
	}

	if err := manager.EnqueueInbound(sessionB.ID, inboundB); err != nil {
		t.Fatalf("expected enqueue inbound sessionB success, got %v", err)
	}

	if err := manager.EnqueueOutbound(sessionA.ID, outboundA); err != nil {
		t.Fatalf("expected enqueue outbound sessionA success, got %v", err)
	}

	if err := manager.EnqueueOutbound(sessionB.ID, outboundB); err != nil {
		t.Fatalf("expected enqueue outbound sessionB success, got %v", err)
	}

	readInboundA, ok, err := manager.DequeueInbound(sessionA.ID)
	if err != nil {
		t.Fatalf("expected dequeue inbound sessionA success, got %v", err)
	}
	if !ok {
		t.Fatalf("expected inbound data for sessionA")
	}
	if !reflect.DeepEqual(readInboundA, inboundA) {
		t.Fatalf("expected sessionA inbound %#v, got %#v", inboundA, readInboundA)
	}

	readInboundB, ok, err := manager.DequeueInbound(sessionB.ID)
	if err != nil {
		t.Fatalf("expected dequeue inbound sessionB success, got %v", err)
	}
	if !ok {
		t.Fatalf("expected inbound data for sessionB")
	}
	if !reflect.DeepEqual(readInboundB, inboundB) {
		t.Fatalf("expected sessionB inbound %#v, got %#v", inboundB, readInboundB)
	}

	readOutboundA, ok, err := manager.DequeueOutbound(sessionA.ID)
	if err != nil {
		t.Fatalf("expected dequeue outbound sessionA success, got %v", err)
	}
	if !ok {
		t.Fatalf("expected outbound data for sessionA")
	}
	if !reflect.DeepEqual(readOutboundA, outboundA) {
		t.Fatalf("expected sessionA outbound %#v, got %#v", outboundA, readOutboundA)
	}

	readOutboundB, ok, err := manager.DequeueOutbound(sessionB.ID)
	if err != nil {
		t.Fatalf("expected dequeue outbound sessionB success, got %v", err)
	}
	if !ok {
		t.Fatalf("expected outbound data for sessionB")
	}
	if !reflect.DeepEqual(readOutboundB, outboundB) {
		t.Fatalf("expected sessionB outbound %#v, got %#v", outboundB, readOutboundB)
	}

	snapshotA, err := manager.Snapshot(sessionA.ID)
	if err != nil {
		t.Fatalf("expected snapshot sessionA success, got %v", err)
	}
	if snapshotA.InboundDepth != 0 || snapshotA.OutboundDepth != 0 {
		t.Fatalf("expected empty queues for sessionA, got inbound=%d outbound=%d", snapshotA.InboundDepth, snapshotA.OutboundDepth)
	}

	snapshotB, err := manager.Snapshot(sessionB.ID)
	if err != nil {
		t.Fatalf("expected snapshot sessionB success, got %v", err)
	}
	if snapshotB.InboundDepth != 0 || snapshotB.OutboundDepth != 0 {
		t.Fatalf("expected empty queues for sessionB, got inbound=%d outbound=%d", snapshotB.InboundDepth, snapshotB.OutboundDepth)
	}
}

func TestManagerBoundedQueuesAndReconnectReset(t *testing.T) {
	manager := NewManager(
		Options{
			InboundCapacity:  1,
			OutboundCapacity: 1,
		},
		Hooks{},
	)

	identity := Identity{
		ClientID:   "client-bounded",
		Protocol:   "enh",
		RemoteAddr: "127.0.0.1:30001",
	}

	session, err := manager.Register(identity)
	if err != nil {
		t.Fatalf("expected register success, got %v", err)
	}

	if err := manager.EnqueueInbound(session.ID, downstream.Frame{Payload: []byte{0x01}}); err != nil {
		t.Fatalf("expected enqueue inbound success, got %v", err)
	}

	err = manager.EnqueueInbound(session.ID, downstream.Frame{Payload: []byte{0x02}})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected queue full error, got %v", err)
	}

	if err := manager.EnqueueOutbound(session.ID, downstream.Frame{Payload: []byte{0x03}}); err != nil {
		t.Fatalf("expected enqueue outbound success, got %v", err)
	}

	err = manager.EnqueueOutbound(session.ID, downstream.Frame{Payload: []byte{0x04}})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("expected queue full error, got %v", err)
	}

	_, err = manager.Unregister(session.ID, nil)
	if err != nil {
		t.Fatalf("expected unregister success, got %v", err)
	}

	err = manager.EnqueueInbound(session.ID, downstream.Frame{Payload: []byte{0x05}})
	if !errors.Is(err, ErrSessionNotConnected) {
		t.Fatalf("expected not connected error, got %v", err)
	}

	reconnectedSession, err := manager.Reconnect(identity)
	if err != nil {
		t.Fatalf("expected reconnect success, got %v", err)
	}

	if reconnectedSession.ID != session.ID {
		t.Fatalf("expected stable ID %d, got %d", session.ID, reconnectedSession.ID)
	}

	if reconnectedSession.InboundDepth != 0 || reconnectedSession.OutboundDepth != 0 {
		t.Fatalf("expected empty queues after reconnect, got inbound=%d outbound=%d", reconnectedSession.InboundDepth, reconnectedSession.OutboundDepth)
	}
}
