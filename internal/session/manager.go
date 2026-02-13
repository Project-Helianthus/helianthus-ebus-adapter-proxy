package session

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

var (
	ErrSessionNotFound         = errors.New("session not found")
	ErrSessionAlreadyConnected = errors.New("session already connected")
	ErrSessionNotConnected     = errors.New("session not connected")
	ErrQueueFull               = errors.New("session queue is full")
)

type EventType string

const (
	EventTypeConnected    EventType = "connected"
	EventTypeDisconnected EventType = "disconnected"
	EventTypeReconnected  EventType = "reconnected"
)

type Identity struct {
	ClientID   string
	Protocol   string
	RemoteAddr string
}

type Session struct {
	ID             uint64
	Identity       Identity
	Connected      bool
	ConnectedAt    time.Time
	DisconnectedAt time.Time
	ReconnectCount uint64
	InboundDepth   int
	OutboundDepth  int
}

type Event struct {
	Type    EventType
	Session Session
	Cause   error
}

type Hooks struct {
	OnConnect    func(Event)
	OnDisconnect func(Event)
	OnReconnect  func(Event)
}

type Options struct {
	InboundCapacity  int
	OutboundCapacity int
}

type Manager struct {
	mutex        sync.RWMutex
	options      Options
	hooks        Hooks
	identityToID map[string]uint64
	sessions     map[uint64]*sessionState
	nextID       uint64
}

type sessionState struct {
	id             uint64
	identity       Identity
	connected      bool
	connectedAt    time.Time
	disconnectedAt time.Time
	reconnectCount uint64
	inboundQueue   *frameQueue
	outboundQueue  *frameQueue
}

type frameQueue struct {
	mutex    sync.Mutex
	capacity int
	items    []downstream.Frame
}

func NewManager(options Options, hooks Hooks) *Manager {
	if options.InboundCapacity <= 0 {
		options.InboundCapacity = 32
	}

	if options.OutboundCapacity <= 0 {
		options.OutboundCapacity = 32
	}

	return &Manager{
		options:      options,
		hooks:        hooks,
		identityToID: make(map[string]uint64),
		sessions:     make(map[uint64]*sessionState),
	}
}

func (manager *Manager) Register(identity Identity) (Session, error) {
	identityKey := keyFromIdentity(identity)

	manager.mutex.Lock()
	if existingSessionID, found := manager.identityToID[identityKey]; found {
		existingState := manager.sessions[existingSessionID]
		if existingState != nil && existingState.connected {
			manager.mutex.Unlock()
			return Session{}, ErrSessionAlreadyConnected
		}

		manager.mutex.Unlock()
		return Session{}, ErrSessionNotConnected
	}

	manager.nextID++
	now := time.Now().UTC()
	state := &sessionState{
		id:          manager.nextID,
		identity:    identity,
		connected:   true,
		connectedAt: now,
		inboundQueue: newFrameQueue(
			manager.options.InboundCapacity,
		),
		outboundQueue: newFrameQueue(
			manager.options.OutboundCapacity,
		),
	}

	manager.identityToID[identityKey] = state.id
	manager.sessions[state.id] = state
	event := Event{
		Type:    EventTypeConnected,
		Session: state.snapshot(),
	}
	connectHook := manager.hooks.OnConnect
	manager.mutex.Unlock()

	if connectHook != nil {
		connectHook(event)
	}

	return event.Session, nil
}

func (manager *Manager) Unregister(sessionID uint64, cause error) (Session, error) {
	manager.mutex.Lock()
	state, found := manager.sessions[sessionID]
	if !found {
		manager.mutex.Unlock()
		return Session{}, ErrSessionNotFound
	}

	if !state.connected {
		manager.mutex.Unlock()
		return Session{}, ErrSessionNotConnected
	}

	state.connected = false
	state.disconnectedAt = time.Now().UTC()
	state.inboundQueue.clear()
	state.outboundQueue.clear()

	event := Event{
		Type:    EventTypeDisconnected,
		Session: state.snapshot(),
		Cause:   cause,
	}
	disconnectHook := manager.hooks.OnDisconnect
	manager.mutex.Unlock()

	if disconnectHook != nil {
		disconnectHook(event)
	}

	return event.Session, nil
}

func (manager *Manager) Reconnect(identity Identity) (Session, error) {
	identityKey := keyFromIdentity(identity)

	manager.mutex.Lock()
	sessionID, found := manager.identityToID[identityKey]
	if !found {
		manager.mutex.Unlock()
		return Session{}, ErrSessionNotFound
	}

	state := manager.sessions[sessionID]
	if state == nil {
		manager.mutex.Unlock()
		return Session{}, ErrSessionNotFound
	}

	if state.connected {
		manager.mutex.Unlock()
		return Session{}, ErrSessionAlreadyConnected
	}

	now := time.Now().UTC()
	state.connected = true
	state.connectedAt = now
	state.disconnectedAt = time.Time{}
	state.reconnectCount++
	state.identity = identity
	state.inboundQueue = newFrameQueue(manager.options.InboundCapacity)
	state.outboundQueue = newFrameQueue(manager.options.OutboundCapacity)

	event := Event{
		Type:    EventTypeReconnected,
		Session: state.snapshot(),
	}
	reconnectHook := manager.hooks.OnReconnect
	manager.mutex.Unlock()

	if reconnectHook != nil {
		reconnectHook(event)
	}

	return event.Session, nil
}

func (manager *Manager) Snapshot(sessionID uint64) (Session, error) {
	manager.mutex.RLock()
	state, found := manager.sessions[sessionID]
	if !found {
		manager.mutex.RUnlock()
		return Session{}, ErrSessionNotFound
	}

	session := state.snapshot()
	manager.mutex.RUnlock()

	return session, nil
}

func (manager *Manager) ActiveSessions() []Session {
	manager.mutex.RLock()
	sessions := make([]Session, 0, len(manager.sessions))
	for _, state := range manager.sessions {
		if !state.connected {
			continue
		}

		sessions = append(sessions, state.snapshot())
	}
	manager.mutex.RUnlock()

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	return sessions
}

func (manager *Manager) EnqueueInbound(sessionID uint64, frame downstream.Frame) error {
	manager.mutex.RLock()
	state, found := manager.sessions[sessionID]
	if !found {
		manager.mutex.RUnlock()
		return ErrSessionNotFound
	}

	if !state.connected {
		manager.mutex.RUnlock()
		return ErrSessionNotConnected
	}

	err := state.inboundQueue.enqueue(frame)
	manager.mutex.RUnlock()

	return err
}

func (manager *Manager) EnqueueOutbound(sessionID uint64, frame downstream.Frame) error {
	manager.mutex.RLock()
	state, found := manager.sessions[sessionID]
	if !found {
		manager.mutex.RUnlock()
		return ErrSessionNotFound
	}

	if !state.connected {
		manager.mutex.RUnlock()
		return ErrSessionNotConnected
	}

	err := state.outboundQueue.enqueue(frame)
	manager.mutex.RUnlock()

	return err
}

func (manager *Manager) DequeueInbound(sessionID uint64) (downstream.Frame, bool, error) {
	manager.mutex.RLock()
	state, found := manager.sessions[sessionID]
	if !found {
		manager.mutex.RUnlock()
		return downstream.Frame{}, false, ErrSessionNotFound
	}

	if !state.connected {
		manager.mutex.RUnlock()
		return downstream.Frame{}, false, ErrSessionNotConnected
	}

	frame, ok := state.inboundQueue.dequeue()
	manager.mutex.RUnlock()

	return frame, ok, nil
}

func (manager *Manager) DequeueOutbound(sessionID uint64) (downstream.Frame, bool, error) {
	manager.mutex.RLock()
	state, found := manager.sessions[sessionID]
	if !found {
		manager.mutex.RUnlock()
		return downstream.Frame{}, false, ErrSessionNotFound
	}

	if !state.connected {
		manager.mutex.RUnlock()
		return downstream.Frame{}, false, ErrSessionNotConnected
	}

	frame, ok := state.outboundQueue.dequeue()
	manager.mutex.RUnlock()

	return frame, ok, nil
}

func (state *sessionState) snapshot() Session {
	return Session{
		ID:             state.id,
		Identity:       state.identity,
		Connected:      state.connected,
		ConnectedAt:    state.connectedAt,
		DisconnectedAt: state.disconnectedAt,
		ReconnectCount: state.reconnectCount,
		InboundDepth:   state.inboundQueue.depth(),
		OutboundDepth:  state.outboundQueue.depth(),
	}
}

func newFrameQueue(capacity int) *frameQueue {
	return &frameQueue{
		capacity: capacity,
		items:    make([]downstream.Frame, 0, capacity),
	}
}

func (queue *frameQueue) enqueue(frame downstream.Frame) error {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()

	if len(queue.items) >= queue.capacity {
		return ErrQueueFull
	}

	queue.items = append(queue.items, cloneFrame(frame))
	return nil
}

func (queue *frameQueue) dequeue() (downstream.Frame, bool) {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()

	if len(queue.items) == 0 {
		return downstream.Frame{}, false
	}

	frame := queue.items[0]
	queue.items = queue.items[1:]
	return frame, true
}

func (queue *frameQueue) depth() int {
	queue.mutex.Lock()
	depth := len(queue.items)
	queue.mutex.Unlock()

	return depth
}

func (queue *frameQueue) clear() {
	queue.mutex.Lock()
	queue.items = queue.items[:0]
	queue.mutex.Unlock()
}

func cloneFrame(frame downstream.Frame) downstream.Frame {
	clonedPayload := append([]byte(nil), frame.Payload...)
	return downstream.Frame{
		Address: frame.Address,
		Command: frame.Command,
		Payload: clonedPayload,
	}
}

func keyFromIdentity(identity Identity) string {
	return fmt.Sprintf("%s|%s|%s", identity.ClientID, identity.Protocol, identity.RemoteAddr)
}
