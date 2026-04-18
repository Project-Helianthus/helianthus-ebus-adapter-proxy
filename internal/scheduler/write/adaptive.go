package writescheduler

import (
	"sort"
	"sync"
)

type Candidate struct {
	SessionID  uint64
	QueueDepth int
}

type SessionQueueSnapshot struct {
	SessionID     uint64
	Connected     bool
	OutboundDepth int
}

type Options struct {
	StarvationAfter int
}

type AdaptiveScheduler struct {
	mutex   sync.Mutex
	options Options
	round   uint64
	states  map[uint64]sessionState
}

type sessionState struct {
	seen            bool
	lastServedRound uint64
}

type rankedCandidate struct {
	candidate   Candidate
	staleRounds uint64
}

func NewAdaptiveScheduler(options Options) *AdaptiveScheduler {
	if options.StarvationAfter <= 0 {
		options.StarvationAfter = 8
	}

	return &AdaptiveScheduler{
		options: options,
		states:  make(map[uint64]sessionState),
	}
}

func (scheduler *AdaptiveScheduler) Select(candidates []Candidate) (uint64, bool) {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()

	active := normalizeCandidates(candidates)
	if len(active) == 0 {
		return 0, false
	}

	ranked := make([]rankedCandidate, 0, len(active))
	for _, candidate := range active {
		state := scheduler.states[candidate.SessionID]
		ranked = append(ranked, rankedCandidate{
			candidate:   candidate,
			staleRounds: scheduler.staleRounds(state),
		})
	}

	selected := scheduler.selectCandidate(ranked)

	state := scheduler.states[selected.SessionID]
	state.seen = true
	state.lastServedRound = scheduler.round
	scheduler.states[selected.SessionID] = state
	scheduler.round++

	return selected.SessionID, true
}

func (scheduler *AdaptiveScheduler) selectCandidate(
	ranked []rankedCandidate,
) Candidate {
	starvationCandidates := make([]rankedCandidate, 0, len(ranked))
	for _, candidate := range ranked {
		if int(candidate.staleRounds) >= scheduler.options.StarvationAfter {
			starvationCandidates = append(starvationCandidates, candidate)
		}
	}

	if len(starvationCandidates) > 0 {
		sort.Slice(starvationCandidates, func(i, j int) bool {
			if starvationCandidates[i].staleRounds != starvationCandidates[j].staleRounds {
				return starvationCandidates[i].staleRounds > starvationCandidates[j].staleRounds
			}

			if starvationCandidates[i].candidate.QueueDepth != starvationCandidates[j].candidate.QueueDepth {
				return starvationCandidates[i].candidate.QueueDepth > starvationCandidates[j].candidate.QueueDepth
			}

			return starvationCandidates[i].candidate.SessionID < starvationCandidates[j].candidate.SessionID
		})

		return starvationCandidates[0].candidate
	}

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].candidate.QueueDepth != ranked[j].candidate.QueueDepth {
			return ranked[i].candidate.QueueDepth > ranked[j].candidate.QueueDepth
		}

		if ranked[i].staleRounds != ranked[j].staleRounds {
			return ranked[i].staleRounds > ranked[j].staleRounds
		}

		return ranked[i].candidate.SessionID < ranked[j].candidate.SessionID
	})

	return ranked[0].candidate
}

func (scheduler *AdaptiveScheduler) staleRounds(state sessionState) uint64 {
	if !state.seen {
		// PX40: Clamp to prevent overflow when round wraps.
		if scheduler.round == ^uint64(0) {
			return ^uint64(0)
		}
		return scheduler.round + 1
	}

	return scheduler.round - state.lastServedRound
}

func normalizeCandidates(candidates []Candidate) []Candidate {
	if len(candidates) == 0 {
		return nil
	}

	merged := make(map[uint64]int)
	for _, candidate := range candidates {
		if candidate.QueueDepth <= 0 {
			continue
		}

		existingDepth, found := merged[candidate.SessionID]
		if !found || candidate.QueueDepth > existingDepth {
			merged[candidate.SessionID] = candidate.QueueDepth
		}
	}

	if len(merged) == 0 {
		return nil
	}

	normalized := make([]Candidate, 0, len(merged))
	for sessionID, queueDepth := range merged {
		normalized = append(normalized, Candidate{
			SessionID:  sessionID,
			QueueDepth: queueDepth,
		})
	}

	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].SessionID < normalized[j].SessionID
	})

	return normalized
}

func CandidatesFromSessionQueueSnapshots(
	snapshots []SessionQueueSnapshot,
) []Candidate {
	if len(snapshots) == 0 {
		return nil
	}

	candidates := make([]Candidate, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if !snapshot.Connected {
			continue
		}

		candidates = append(candidates, Candidate{
			SessionID:  snapshot.SessionID,
			QueueDepth: snapshot.OutboundDepth,
		})
	}

	return normalizeCandidates(candidates)
}
