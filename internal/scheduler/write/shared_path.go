package writescheduler

import (
	"errors"
	"sync"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

var (
	ErrArbitrationIDRequired = errors.New("arbitration id is required")
	ErrWriteModeUnsupported  = errors.New("write mode is unsupported")
)

type WriteMode string

const (
	WriteModePassThrough WriteMode = "pass_through"
	WriteModeEmulated    WriteMode = "emulated"
)

type WriteEnvelope struct {
	ArbitrationID uint64
	Mode          WriteMode
	Frame         downstream.Frame
}

type SharedArbitrationPath struct {
	mutex     sync.Mutex
	scheduler *AdaptiveScheduler
	queues    map[uint64][]WriteEnvelope
}

func NewSharedArbitrationPath(scheduler *AdaptiveScheduler) *SharedArbitrationPath {
	if scheduler == nil {
		scheduler = NewAdaptiveScheduler(Options{})
	}

	return &SharedArbitrationPath{
		scheduler: scheduler,
		queues:    make(map[uint64][]WriteEnvelope),
	}
}

func (path *SharedArbitrationPath) EnqueuePassThrough(
	arbitrationID uint64,
	frame downstream.Frame,
) error {
	return path.enqueue(WriteEnvelope{
		ArbitrationID: arbitrationID,
		Mode:          WriteModePassThrough,
		Frame:         frame,
	})
}

func (path *SharedArbitrationPath) EnqueueEmulated(
	arbitrationID uint64,
	frame downstream.Frame,
) error {
	return path.enqueue(WriteEnvelope{
		ArbitrationID: arbitrationID,
		Mode:          WriteModeEmulated,
		Frame:         frame,
	})
}

func (path *SharedArbitrationPath) NextWrite() (WriteEnvelope, bool) {
	path.mutex.Lock()
	defer path.mutex.Unlock()

	candidates := make([]Candidate, 0, len(path.queues))
	for arbitrationID, queue := range path.queues {
		if len(queue) == 0 {
			continue
		}

		candidates = append(candidates, Candidate{
			SessionID:  arbitrationID,
			QueueDepth: len(queue),
		})
	}

	selectedArbitrationID, ok := path.scheduler.Select(candidates)
	if !ok {
		return WriteEnvelope{}, false
	}

	queue := path.queues[selectedArbitrationID]
	nextWrite := queue[0]

	if len(queue) == 1 {
		delete(path.queues, selectedArbitrationID)
	} else {
		path.queues[selectedArbitrationID] = queue[1:]
	}

	return cloneWriteEnvelope(nextWrite), true
}

func (path *SharedArbitrationPath) Pending(arbitrationID uint64) int {
	path.mutex.Lock()
	pending := len(path.queues[arbitrationID])
	path.mutex.Unlock()

	return pending
}

func (path *SharedArbitrationPath) enqueue(write WriteEnvelope) error {
	if write.ArbitrationID == 0 {
		return ErrArbitrationIDRequired
	}

	if write.Mode != WriteModePassThrough && write.Mode != WriteModeEmulated {
		return ErrWriteModeUnsupported
	}

	path.mutex.Lock()
	path.queues[write.ArbitrationID] = append(
		path.queues[write.ArbitrationID],
		cloneWriteEnvelope(write),
	)
	path.mutex.Unlock()

	return nil
}

func cloneWriteEnvelope(write WriteEnvelope) WriteEnvelope {
	return WriteEnvelope{
		ArbitrationID: write.ArbitrationID,
		Mode:          write.Mode,
		Frame: downstream.Frame{
			Address: write.Frame.Address,
			Command: write.Frame.Command,
			Payload: append([]byte(nil), write.Frame.Payload...),
		},
	}
}
