package sourcepolicy

import (
	"errors"
	"sync"
	"time"
)

var ErrInvalidActivityWindow = errors.New("invalid source address activity window")

// ActivityWindow tracks recently active source addresses so unsafe new leases can be rejected.
type ActivityWindow struct {
	mutex    sync.Mutex
	window   time.Duration
	clock    func() time.Time
	lastSeen map[uint8]time.Time
}

func NewActivityWindow(window time.Duration, clock func() time.Time) (*ActivityWindow, error) {
	if window <= 0 {
		return nil, ErrInvalidActivityWindow
	}

	if clock == nil {
		clock = time.Now
	}

	return &ActivityWindow{
		window:   window,
		clock:    clock,
		lastSeen: make(map[uint8]time.Time),
	}, nil
}

func (activityWindow *ActivityWindow) Observe(address uint8) {
	activityWindow.ObserveAt(address, activityWindow.clock())
}

func (activityWindow *ActivityWindow) ObserveAt(address uint8, observedAt time.Time) {
	if !isTrackableAddress(address) {
		return
	}

	observedAt = observedAt.UTC()
	cutoff := observedAt.Add(-activityWindow.window)

	activityWindow.mutex.Lock()
	activityWindow.pruneLocked(cutoff)
	lastObservedAt, found := activityWindow.lastSeen[address]
	if !found || observedAt.After(lastObservedAt) {
		activityWindow.lastSeen[address] = observedAt
	}
	activityWindow.mutex.Unlock()
}

func (activityWindow *ActivityWindow) IsRecentlyActive(address uint8) bool {
	return activityWindow.IsRecentlyActiveAt(address, activityWindow.clock())
}

func (activityWindow *ActivityWindow) IsRecentlyActiveAt(address uint8, now time.Time) bool {
	if !isTrackableAddress(address) {
		return false
	}

	now = now.UTC()
	cutoff := now.Add(-activityWindow.window)

	activityWindow.mutex.Lock()
	activityWindow.pruneLocked(cutoff)
	lastObservedAt, found := activityWindow.lastSeen[address]
	activityWindow.mutex.Unlock()
	if !found {
		return false
	}

	return lastObservedAt.After(cutoff)
}

func (activityWindow *ActivityWindow) pruneLocked(cutoff time.Time) {
	for address, lastObservedAt := range activityWindow.lastSeen {
		if !lastObservedAt.After(cutoff) {
			delete(activityWindow.lastSeen, address)
		}
	}
}

func isTrackableAddress(address uint8) bool {
	return address != 0x00 && address != 0xFF
}
