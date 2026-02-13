package writescheduler

import (
	"reflect"
	"testing"
)

func TestAdaptiveSchedulerBalancedLoad(t *testing.T) {
	scheduler := NewAdaptiveScheduler(Options{StarvationAfter: 6})

	candidates := []Candidate{
		{SessionID: 1, QueueDepth: 1},
		{SessionID: 2, QueueDepth: 1},
	}

	selected := make([]uint64, 0, 6)
	for range 6 {
		sessionID, ok := scheduler.Select(candidates)
		if !ok {
			t.Fatalf("expected selection to succeed")
		}
		selected = append(selected, sessionID)
	}

	expected := []uint64{1, 2, 1, 2, 1, 2}
	if !reflect.DeepEqual(selected, expected) {
		t.Fatalf("expected selection order %#v, got %#v", expected, selected)
	}
}

func TestAdaptiveSchedulerHeavyAndSparseLoad(t *testing.T) {
	scheduler := NewAdaptiveScheduler(Options{StarvationAfter: 4})

	candidates := []Candidate{
		{SessionID: 10, QueueDepth: 10},
		{SessionID: 20, QueueDepth: 1},
	}

	selected := make([]uint64, 0, 8)
	for range 8 {
		sessionID, ok := scheduler.Select(candidates)
		if !ok {
			t.Fatalf("expected selection to succeed")
		}
		selected = append(selected, sessionID)
	}

	expected := []uint64{10, 10, 10, 20, 10, 10, 10, 20}
	if !reflect.DeepEqual(selected, expected) {
		t.Fatalf("expected selection order %#v, got %#v", expected, selected)
	}
}

func TestAdaptiveSchedulerStarvationGuardUnderSustainedLoad(t *testing.T) {
	const starvationAfter = 4

	scheduler := NewAdaptiveScheduler(Options{StarvationAfter: starvationAfter})

	candidates := []Candidate{
		{SessionID: 100, QueueDepth: 10},
		{SessionID: 200, QueueDepth: 1},
		{SessionID: 300, QueueDepth: 1},
	}

	const rounds = 24

	selections := map[uint64][]int{
		100: {},
		200: {},
		300: {},
	}

	for round := range rounds {
		sessionID, ok := scheduler.Select(candidates)
		if !ok {
			t.Fatalf("expected selection to succeed")
		}
		selections[sessionID] = append(selections[sessionID], round)
	}

	maxAllowedGap := starvationAfter + len(candidates) - 1

	for sessionID, roundsForSession := range selections {
		if len(roundsForSession) == 0 {
			t.Fatalf("expected session %d to be selected at least once", sessionID)
		}

		previousRound := -1
		maxGap := 0
		for _, round := range roundsForSession {
			gap := round - previousRound
			if gap > maxGap {
				maxGap = gap
			}
			previousRound = round
		}

		if maxGap > maxAllowedGap {
			t.Fatalf(
				"expected max gap for session %d <= %d, got %d (rounds: %#v)",
				sessionID,
				maxAllowedGap,
				maxGap,
				roundsForSession,
			)
		}
	}
}
