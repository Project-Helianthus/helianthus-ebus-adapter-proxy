package writescheduler

import (
	"errors"
	"reflect"
	"testing"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	sessionmanager "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/session"
)

func TestSharedArbitrationPathRejectsInvalidWrites(t *testing.T) {
	path := NewSharedArbitrationPath(NewAdaptiveScheduler(Options{StarvationAfter: 4}))

	err := path.EnqueuePassThrough(0, downstream.Frame{Command: 0x01})
	if !errors.Is(err, ErrArbitrationIDRequired) {
		t.Fatalf("expected arbitration id required error, got %v", err)
	}

	err = path.enqueue(WriteEnvelope{
		ArbitrationID: 1,
		Mode:          WriteMode("unknown"),
		Frame:         downstream.Frame{Command: 0x02},
	})
	if !errors.Is(err, ErrWriteModeUnsupported) {
		t.Fatalf("expected unsupported write mode error, got %v", err)
	}
}

func TestSharedArbitrationPathIntegrationOrderingGuaranteeMixedModes(t *testing.T) {
	path := NewSharedArbitrationPath(NewAdaptiveScheduler(Options{StarvationAfter: 5}))

	if err := path.EnqueuePassThrough(
		10,
		downstream.Frame{Command: 0xA0, Payload: []byte{0x10}},
	); err != nil {
		t.Fatalf("expected pass-through enqueue success, got %v", err)
	}
	if err := path.EnqueueEmulated(
		10,
		downstream.Frame{Command: 0xA1, Payload: []byte{0x11}},
	); err != nil {
		t.Fatalf("expected emulated enqueue success, got %v", err)
	}
	if err := path.EnqueueEmulated(
		20,
		downstream.Frame{Command: 0xB0, Payload: []byte{0x20}},
	); err != nil {
		t.Fatalf("expected emulated enqueue success, got %v", err)
	}
	if err := path.EnqueuePassThrough(
		20,
		downstream.Frame{Command: 0xB1, Payload: []byte{0x21}},
	); err != nil {
		t.Fatalf("expected pass-through enqueue success, got %v", err)
	}

	want := []WriteEnvelope{
		{ArbitrationID: 10, Mode: WriteModePassThrough, Frame: downstream.Frame{Command: 0xA0, Payload: []byte{0x10}}},
		{ArbitrationID: 20, Mode: WriteModeEmulated, Frame: downstream.Frame{Command: 0xB0, Payload: []byte{0x20}}},
		{ArbitrationID: 10, Mode: WriteModeEmulated, Frame: downstream.Frame{Command: 0xA1, Payload: []byte{0x11}}},
		{ArbitrationID: 20, Mode: WriteModePassThrough, Frame: downstream.Frame{Command: 0xB1, Payload: []byte{0x21}}},
	}

	for index, expectedWrite := range want {
		write, ok := path.NextWrite()
		if !ok {
			t.Fatalf("expected write #%d to be available", index)
		}

		if !reflect.DeepEqual(write, expectedWrite) {
			t.Fatalf("write #%d mismatch:\nexpected: %#v\nactual: %#v", index, expectedWrite, write)
		}
	}

	if _, ok := path.NextWrite(); ok {
		t.Fatalf("expected no writes after queue drain")
	}
}

func TestSharedArbitrationPathIntegrationDeterministicControlSequence(t *testing.T) {
	wantSequence := []WriteEnvelope{
		{ArbitrationID: 1, Mode: WriteModePassThrough, Frame: downstream.Frame{Command: 0x11, Payload: []byte{0x01}}},
		{ArbitrationID: 2, Mode: WriteModeEmulated, Frame: downstream.Frame{Command: 0x21, Payload: []byte{0x02}}},
		{ArbitrationID: 1, Mode: WriteModeEmulated, Frame: downstream.Frame{Command: 0x12, Payload: []byte{0x03}}},
		{ArbitrationID: 2, Mode: WriteModePassThrough, Frame: downstream.Frame{Command: 0x22, Payload: []byte{0x04}}},
	}

	for iteration := 0; iteration < 50; iteration++ {
		path := NewSharedArbitrationPath(NewAdaptiveScheduler(Options{StarvationAfter: 4}))

		enqueues := []WriteEnvelope{
			{ArbitrationID: 1, Mode: WriteModePassThrough, Frame: downstream.Frame{Command: 0x11, Payload: []byte{0x01}}},
			{ArbitrationID: 1, Mode: WriteModeEmulated, Frame: downstream.Frame{Command: 0x12, Payload: []byte{0x03}}},
			{ArbitrationID: 2, Mode: WriteModeEmulated, Frame: downstream.Frame{Command: 0x21, Payload: []byte{0x02}}},
			{ArbitrationID: 2, Mode: WriteModePassThrough, Frame: downstream.Frame{Command: 0x22, Payload: []byte{0x04}}},
		}

		for enqueueIndex, write := range enqueues {
			if err := path.enqueue(write); err != nil {
				t.Fatalf(
					"expected enqueue #%d success in iteration %d, got %v",
					enqueueIndex,
					iteration,
					err,
				)
			}
		}

		sequence := make([]WriteEnvelope, 0, len(wantSequence))
		for range wantSequence {
			write, ok := path.NextWrite()
			if !ok {
				t.Fatalf("expected write availability in iteration %d", iteration)
			}
			sequence = append(sequence, write)
		}

		if !reflect.DeepEqual(sequence, wantSequence) {
			t.Fatalf(
				"expected deterministic sequence in iteration %d:\nexpected: %#v\nactual: %#v",
				iteration,
				wantSequence,
				sequence,
			)
		}
	}
}

func TestSharedArbitrationPathSessionManagerIntegrationOrdering(t *testing.T) {
	manager := sessionmanager.NewManager(
		sessionmanager.Options{
			InboundCapacity:  4,
			OutboundCapacity: 8,
		},
		sessionmanager.Hooks{},
	)

	sessionA, err := manager.Register(sessionmanager.Identity{
		ClientID:   "arb-a",
		Protocol:   "enh",
		RemoteAddr: "127.0.0.1:60001",
	})
	if err != nil {
		t.Fatalf("expected register sessionA success, got %v", err)
	}

	sessionB, err := manager.Register(sessionmanager.Identity{
		ClientID:   "arb-b",
		Protocol:   "enh",
		RemoteAddr: "127.0.0.1:60002",
	})
	if err != nil {
		t.Fatalf("expected register sessionB success, got %v", err)
	}

	path := NewSharedArbitrationPath(NewAdaptiveScheduler(Options{StarvationAfter: 5}))

	if err := path.EnqueuePassThrough(
		sessionA.ID,
		downstream.Frame{Command: 0x31, Payload: []byte{0xA1}},
	); err != nil {
		t.Fatalf("expected pass-through enqueue success, got %v", err)
	}
	if err := path.EnqueueEmulated(
		sessionA.ID,
		downstream.Frame{Command: 0x32, Payload: []byte{0xA2}},
	); err != nil {
		t.Fatalf("expected emulated enqueue success, got %v", err)
	}
	if err := path.EnqueueEmulated(
		sessionB.ID,
		downstream.Frame{Command: 0x41, Payload: []byte{0xB1}},
	); err != nil {
		t.Fatalf("expected emulated enqueue success, got %v", err)
	}
	if err := path.EnqueuePassThrough(
		sessionB.ID,
		downstream.Frame{Command: 0x42, Payload: []byte{0xB2}},
	); err != nil {
		t.Fatalf("expected pass-through enqueue success, got %v", err)
	}

	wantOrder := []uint64{sessionA.ID, sessionB.ID, sessionA.ID, sessionB.ID}
	for index, expectedSessionID := range wantOrder {
		write, ok := path.NextWrite()
		if !ok {
			t.Fatalf("expected write availability at index %d", index)
		}

		if write.ArbitrationID != expectedSessionID {
			t.Fatalf(
				"expected arbitration id %d at index %d, got %d",
				expectedSessionID,
				index,
				write.ArbitrationID,
			)
		}
	}

	if path.Pending(sessionA.ID) != 0 || path.Pending(sessionB.ID) != 0 {
		t.Fatalf("expected no pending writes after drain (a=%d b=%d)", path.Pending(sessionA.ID), path.Pending(sessionB.ID))
	}
}
