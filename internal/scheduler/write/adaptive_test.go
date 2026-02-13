package writescheduler

import (
	"reflect"
	"sync"
	"testing"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	sessionmanager "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/session"
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

func TestAdaptiveSchedulerDeterministicControlSameInputsSameSequence(t *testing.T) {
	inputs := [][]Candidate{
		{
			{SessionID: 1, QueueDepth: 3},
			{SessionID: 2, QueueDepth: 2},
			{SessionID: 3, QueueDepth: 2},
		},
		{
			{SessionID: 3, QueueDepth: 1},
			{SessionID: 2, QueueDepth: 4},
		},
		{
			{SessionID: 1, QueueDepth: 0},
			{SessionID: 2, QueueDepth: 1},
			{SessionID: 3, QueueDepth: 3},
			{SessionID: 2, QueueDepth: 3},
		},
		{
			{SessionID: 1, QueueDepth: 5},
			{SessionID: 3, QueueDepth: 5},
		},
	}

	runSequence := func() []uint64 {
		scheduler := NewAdaptiveScheduler(Options{StarvationAfter: 4})

		sequence := make([]uint64, 0, len(inputs)*3)
		for range 3 {
			for _, candidates := range inputs {
				sessionID, ok := scheduler.Select(candidates)
				if !ok {
					t.Fatalf("expected selection to succeed")
				}
				sequence = append(sequence, sessionID)
			}
		}

		return sequence
	}

	baseline := runSequence()
	for iteration := range 12 {
		got := runSequence()
		if !reflect.DeepEqual(got, baseline) {
			t.Fatalf(
				"expected deterministic sequence in iteration %d: %#v, got %#v",
				iteration,
				baseline,
				got,
			)
		}
	}
}

func TestAdaptiveSchedulerConcurrentSelectSafety(t *testing.T) {
	scheduler := NewAdaptiveScheduler(Options{StarvationAfter: 6})

	candidates := []Candidate{
		{SessionID: 11, QueueDepth: 6},
		{SessionID: 22, QueueDepth: 4},
		{SessionID: 33, QueueDepth: 2},
	}

	validSessionIDs := map[uint64]struct{}{
		11: {},
		22: {},
		33: {},
	}

	const goroutines = 48
	const iterationsPerGoroutine = 120

	start := make(chan struct{})
	results := make(chan uint64, goroutines*iterationsPerGoroutine)
	errors := make(chan string, goroutines)

	var waitGroup sync.WaitGroup
	waitGroup.Add(goroutines)

	for range goroutines {
		go func() {
			defer waitGroup.Done()
			<-start
			for range iterationsPerGoroutine {
				sessionID, ok := scheduler.Select(candidates)
				if !ok {
					errors <- "expected selection to succeed"
					return
				}

				if _, valid := validSessionIDs[sessionID]; !valid {
					errors <- "received invalid session ID"
					return
				}

				results <- sessionID
			}
		}()
	}

	close(start)
	waitGroup.Wait()
	close(errors)
	close(results)

	for errMessage := range errors {
		t.Fatal(errMessage)
	}

	counts := make(map[uint64]int)
	totalSelections := 0
	for sessionID := range results {
		counts[sessionID]++
		totalSelections++
	}

	expectedSelections := goroutines * iterationsPerGoroutine
	if totalSelections != expectedSelections {
		t.Fatalf("expected %d selections, got %d", expectedSelections, totalSelections)
	}

	for sessionID := range validSessionIDs {
		if counts[sessionID] == 0 {
			t.Fatalf("expected session %d to be selected at least once", sessionID)
		}
	}
}

func TestAdaptiveSchedulerSessionManagerIntegrationTouchpoint(t *testing.T) {
	manager := sessionmanager.NewManager(
		sessionmanager.Options{
			InboundCapacity:  4,
			OutboundCapacity: 8,
		},
		sessionmanager.Hooks{},
	)

	sessionA, err := manager.Register(sessionmanager.Identity{
		ClientID:   "scheduler-a",
		Protocol:   "enh",
		RemoteAddr: "127.0.0.1:50001",
	})
	if err != nil {
		t.Fatalf("expected register sessionA success, got %v", err)
	}

	sessionB, err := manager.Register(sessionmanager.Identity{
		ClientID:   "scheduler-b",
		Protocol:   "enh",
		RemoteAddr: "127.0.0.1:50002",
	})
	if err != nil {
		t.Fatalf("expected register sessionB success, got %v", err)
	}

	if err := manager.EnqueueOutbound(
		sessionA.ID,
		downstream.Frame{Command: 0x01, Payload: []byte{0x10}},
	); err != nil {
		t.Fatalf("expected enqueue outbound sessionA success, got %v", err)
	}

	for _, payloadValue := range []byte{0x21, 0x22, 0x23} {
		if err := manager.EnqueueOutbound(
			sessionB.ID,
			downstream.Frame{Command: 0x02, Payload: []byte{payloadValue}},
		); err != nil {
			t.Fatalf("expected enqueue outbound sessionB success, got %v", err)
		}
	}

	scheduler := NewAdaptiveScheduler(Options{StarvationAfter: 5})

	snapshots, err := queueSnapshotsFromManager(manager, []uint64{sessionA.ID, sessionB.ID})
	if err != nil {
		t.Fatalf("expected snapshot collection success, got %v", err)
	}

	candidates := CandidatesFromSessionQueueSnapshots(snapshots)
	selectedSessionID, ok := scheduler.Select(candidates)
	if !ok {
		t.Fatalf("expected selection to succeed")
	}

	if selectedSessionID != sessionB.ID {
		t.Fatalf("expected sessionB (%d) selected first, got %d", sessionB.ID, selectedSessionID)
	}

	_, err = manager.Unregister(sessionB.ID, nil)
	if err != nil {
		t.Fatalf("expected unregister sessionB success, got %v", err)
	}

	snapshots, err = queueSnapshotsFromManager(manager, []uint64{sessionA.ID, sessionB.ID})
	if err != nil {
		t.Fatalf("expected snapshot collection success, got %v", err)
	}

	candidates = CandidatesFromSessionQueueSnapshots(snapshots)
	if len(candidates) != 1 {
		t.Fatalf("expected one connected candidate after sessionB disconnect, got %d", len(candidates))
	}
	if candidates[0].SessionID != sessionA.ID {
		t.Fatalf("expected remaining candidate sessionA (%d), got %d", sessionA.ID, candidates[0].SessionID)
	}

	selectedSessionID, ok = scheduler.Select(candidates)
	if !ok {
		t.Fatalf("expected selection to succeed")
	}
	if selectedSessionID != sessionA.ID {
		t.Fatalf("expected sessionA (%d) selected after disconnect, got %d", sessionA.ID, selectedSessionID)
	}
}

func queueSnapshotsFromManager(
	manager *sessionmanager.Manager,
	sessionIDs []uint64,
) ([]SessionQueueSnapshot, error) {
	snapshots := make([]SessionQueueSnapshot, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		snapshot, err := manager.Snapshot(sessionID)
		if err != nil {
			return nil, err
		}

		snapshots = append(snapshots, SessionQueueSnapshot{
			SessionID:     snapshot.ID,
			Connected:     snapshot.Connected,
			OutboundDepth: snapshot.OutboundDepth,
		})
	}

	return snapshots, nil
}
