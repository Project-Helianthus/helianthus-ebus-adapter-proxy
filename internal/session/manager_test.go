package session

import (
	"errors"
	"fmt"
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

func TestManagerConcurrentOperationsAcrossSessions(t *testing.T) {
	const (
		sessionCount  = 6
		queueCapacity = 3
		rounds        = 8
	)

	manager := NewManager(
		Options{
			InboundCapacity:  queueCapacity,
			OutboundCapacity: queueCapacity,
		},
		Hooks{},
	)

	identities := make([]Identity, sessionCount)
	for index := 0; index < sessionCount; index++ {
		identities[index] = Identity{
			ClientID:   fmt.Sprintf("client-%d", index),
			Protocol:   "enh",
			RemoteAddr: fmt.Sprintf("127.0.0.1:%d", 40000+index),
		}
	}

	registeredSessions := make([]Session, sessionCount)

	startRegister := make(chan struct{})
	var registerGroup sync.WaitGroup
	registerErrors := make(chan error, sessionCount)

	for index := 0; index < sessionCount; index++ {
		registerGroup.Add(1)
		go func(index int) {
			defer registerGroup.Done()
			<-startRegister

			session, err := manager.Register(identities[index])
			if err != nil {
				registerErrors <- fmt.Errorf("register index %d failed: %w", index, err)
				return
			}

			registeredSessions[index] = session
		}(index)
	}

	close(startRegister)
	registerGroup.Wait()
	close(registerErrors)

	for err := range registerErrors {
		t.Fatalf("concurrent register error: %v", err)
	}

	seenIDs := make(map[uint64]struct{}, sessionCount)
	for index := 0; index < sessionCount; index++ {
		session := registeredSessions[index]
		if session.ID == 0 {
			t.Fatalf("expected non-zero session ID at index %d", index)
		}

		if _, duplicate := seenIDs[session.ID]; duplicate {
			t.Fatalf("duplicate session ID detected: %d", session.ID)
		}

		seenIDs[session.ID] = struct{}{}
	}

	if len(manager.ActiveSessions()) != sessionCount {
		t.Fatalf("expected %d active sessions after register, got %d", sessionCount, len(manager.ActiveSessions()))
	}

	startOps := make(chan struct{})
	var opsGroup sync.WaitGroup
	opsErrors := make(chan error, sessionCount)

	for index := 0; index < sessionCount; index++ {
		opsGroup.Add(1)
		go func(index int) {
			defer opsGroup.Done()
			<-startOps

			sessionID := registeredSessions[index].ID
			identity := identities[index]

			for round := 0; round < rounds; round++ {
				inboundFrame := downstream.Frame{
					Command: byte(0x10 + index),
					Payload: []byte{byte(index), byte(round)},
				}
				outboundFrame := downstream.Frame{
					Command: byte(0x20 + index),
					Payload: []byte{byte(round), byte(index)},
				}

				if err := enqueueWithBoundedRecovery(
					func() error {
						return manager.EnqueueInbound(sessionID, inboundFrame)
					},
					func() (downstream.Frame, bool, error) {
						return manager.DequeueInbound(sessionID)
					},
				); err != nil {
					opsErrors <- fmt.Errorf("session %d inbound enqueue failed: %w", sessionID, err)
					return
				}

				if err := enqueueWithBoundedRecovery(
					func() error {
						return manager.EnqueueOutbound(sessionID, outboundFrame)
					},
					func() (downstream.Frame, bool, error) {
						return manager.DequeueOutbound(sessionID)
					},
				); err != nil {
					opsErrors <- fmt.Errorf("session %d outbound enqueue failed: %w", sessionID, err)
					return
				}

				if _, _, err := manager.DequeueInbound(sessionID); err != nil {
					opsErrors <- fmt.Errorf("session %d inbound dequeue failed: %w", sessionID, err)
					return
				}

				if _, _, err := manager.DequeueOutbound(sessionID); err != nil {
					opsErrors <- fmt.Errorf("session %d outbound dequeue failed: %w", sessionID, err)
					return
				}

				if round%2 == 1 {
					if _, err := manager.Unregister(sessionID, nil); err != nil {
						opsErrors <- fmt.Errorf("session %d unregister failed: %w", sessionID, err)
						return
					}

					_, _, err := manager.DequeueInbound(sessionID)
					if !errors.Is(err, ErrSessionNotConnected) {
						opsErrors <- fmt.Errorf("session %d expected not connected error, got %v", sessionID, err)
						return
					}

					reconnectedSession, err := manager.Reconnect(identity)
					if err != nil {
						opsErrors <- fmt.Errorf("session %d reconnect failed: %w", sessionID, err)
						return
					}

					if reconnectedSession.ID != sessionID {
						opsErrors <- fmt.Errorf("session %d reconnect returned different id %d", sessionID, reconnectedSession.ID)
						return
					}
				}
			}

			finalSnapshot, err := manager.Snapshot(sessionID)
			if err != nil {
				opsErrors <- fmt.Errorf("session %d snapshot failed: %w", sessionID, err)
				return
			}

			if !finalSnapshot.Connected {
				opsErrors <- fmt.Errorf("session %d expected connected state", sessionID)
				return
			}

			if finalSnapshot.InboundDepth > queueCapacity || finalSnapshot.OutboundDepth > queueCapacity {
				opsErrors <- fmt.Errorf(
					"session %d queue depth out of bounds: inbound=%d outbound=%d",
					sessionID,
					finalSnapshot.InboundDepth,
					finalSnapshot.OutboundDepth,
				)
				return
			}
		}(index)
	}

	close(startOps)
	opsGroup.Wait()
	close(opsErrors)

	for err := range opsErrors {
		t.Fatalf("concurrent operation error: %v", err)
	}

	if len(manager.ActiveSessions()) != sessionCount {
		t.Fatalf("expected %d active sessions after concurrent operations, got %d", sessionCount, len(manager.ActiveSessions()))
	}

	expectedReconnectCount := uint64(rounds / 2)
	for index := 0; index < sessionCount; index++ {
		sessionID := registeredSessions[index].ID
		snapshot, err := manager.Snapshot(sessionID)
		if err != nil {
			t.Fatalf("expected snapshot for session %d, got %v", sessionID, err)
		}

		if snapshot.ReconnectCount != expectedReconnectCount {
			t.Fatalf(
				"expected reconnect count %d for session %d, got %d",
				expectedReconnectCount,
				sessionID,
				snapshot.ReconnectCount,
			)
		}
	}
}

func enqueueWithBoundedRecovery(
	enqueue func() error,
	dequeue func() (downstream.Frame, bool, error),
) error {
	err := enqueue()
	if err == nil {
		return nil
	}

	if !errors.Is(err, ErrQueueFull) {
		return err
	}

	_, ok, dequeueErr := dequeue()
	if dequeueErr != nil {
		return dequeueErr
	}
	if !ok {
		return errors.New("expected queue item when recovering from full queue")
	}

	return enqueue()
}
