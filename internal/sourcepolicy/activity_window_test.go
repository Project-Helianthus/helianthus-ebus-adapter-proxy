package sourcepolicy

import (
	"errors"
	"testing"
	"time"
)

func TestActivityWindowRejectsNonPositiveDuration(t *testing.T) {
	_, err := NewActivityWindow(0, nil)
	if !errors.Is(err, ErrInvalidActivityWindow) {
		t.Fatalf("expected invalid activity window error, got %v", err)
	}
}

func TestActivityWindowBoundaryIsExclusiveAtWindowLength(t *testing.T) {
	baseTime := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	now := baseTime
	activityWindow := mustNewActivityWindow(t, 5*time.Second, func() time.Time {
		return now
	})

	activityWindow.ObserveAt(0x40, baseTime)

	now = baseTime.Add(5*time.Second - time.Nanosecond)
	if !activityWindow.IsRecentlyActive(0x40) {
		t.Fatalf("expected address to be recently active one nanosecond before boundary")
	}

	now = baseTime.Add(5 * time.Second)
	if activityWindow.IsRecentlyActive(0x40) {
		t.Fatalf("expected address to expire exactly at boundary")
	}
}

func TestActivityWindowKeepsMostRecentObservationForAddress(t *testing.T) {
	baseTime := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	now := baseTime
	activityWindow := mustNewActivityWindow(t, 5*time.Second, func() time.Time {
		return now
	})

	activityWindow.ObserveAt(0x40, baseTime.Add(4*time.Second))
	activityWindow.ObserveAt(0x40, baseTime.Add(2*time.Second))

	now = baseTime.Add(9*time.Second - time.Nanosecond)
	if !activityWindow.IsRecentlyActive(0x40) {
		t.Fatalf("expected newer observation to remain active before boundary")
	}

	now = baseTime.Add(9 * time.Second)
	if activityWindow.IsRecentlyActive(0x40) {
		t.Fatalf("expected address to expire at boundary from newest observation")
	}
}

func mustNewActivityWindow(
	t *testing.T,
	window time.Duration,
	clock func() time.Time,
) *ActivityWindow {
	t.Helper()

	activityWindow, err := NewActivityWindow(window, clock)
	if err != nil {
		t.Fatalf("expected activity window creation success, got %v", err)
	}

	return activityWindow
}
