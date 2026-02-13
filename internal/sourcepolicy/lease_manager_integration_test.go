package sourcepolicy

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLeaseManagerConcurrentAcquireSingleAddressConflict(t *testing.T) {
	manager := mustNewLeaseManager(
		t,
		mustNewPolicy(t, Config{}),
		LeaseManagerOptions{
			LeaseDuration: 30 * time.Second,
		},
	)

	const contenders = 24

	start := make(chan struct{})
	type acquireResult struct {
		lease Lease
		err   error
	}
	results := make(chan acquireResult, contenders)

	var waitGroup sync.WaitGroup
	waitGroup.Add(contenders)

	for contenderIndex := 0; contenderIndex < contenders; contenderIndex++ {
		ownerID := fmt.Sprintf("owner-%02d", contenderIndex)
		go func(ownerID string) {
			defer waitGroup.Done()
			<-start
			lease, err := manager.Acquire(
				ownerID,
				AcquireOptions{
					Candidates: []uint8{0x40},
				},
			)
			results <- acquireResult{
				lease: lease,
				err:   err,
			}
		}(ownerID)
	}

	close(start)
	waitGroup.Wait()
	close(results)

	successCount := 0
	conflictCount := 0

	for result := range results {
		if result.err == nil {
			successCount++
			if result.lease.Address != 0x40 {
				t.Fatalf("expected successful lease address 0x40, got 0x%02X", result.lease.Address)
			}
			continue
		}

		var conflictError LeaseConflictError
		if !errors.As(result.err, &conflictError) {
			t.Fatalf("expected lease conflict error type, got %v", result.err)
		}

		if conflictError.Code != LeaseConflictCodeAddressInUse {
			t.Fatalf(
				"expected conflict code %q, got %q",
				LeaseConflictCodeAddressInUse,
				conflictError.Code,
			)
		}

		if conflictError.Address != 0x40 {
			t.Fatalf("expected conflict address 0x40, got 0x%02X", conflictError.Address)
		}

		conflictCount++
	}

	if successCount != 1 {
		t.Fatalf("expected one successful acquisition, got %d", successCount)
	}

	if conflictCount != contenders-1 {
		t.Fatalf(
			"expected %d conflict acquisitions, got %d",
			contenders-1,
			conflictCount,
		)
	}
}

func TestLeaseManagerConcurrentAcquireMultiAddressContention(t *testing.T) {
	manager := mustNewLeaseManager(
		t,
		mustNewPolicy(t, Config{}),
		LeaseManagerOptions{
			LeaseDuration: 30 * time.Second,
		},
	)

	const contenders = 32
	availableAddresses := map[uint8]struct{}{
		0x40: {},
		0x41: {},
		0x42: {},
	}

	start := make(chan struct{})
	type acquireResult struct {
		lease Lease
		err   error
	}
	results := make(chan acquireResult, contenders)

	var waitGroup sync.WaitGroup
	waitGroup.Add(contenders)

	for contenderIndex := 0; contenderIndex < contenders; contenderIndex++ {
		ownerID := fmt.Sprintf("owner-%02d", contenderIndex)
		go func(ownerID string) {
			defer waitGroup.Done()
			<-start
			lease, err := manager.Acquire(
				ownerID,
				AcquireOptions{
					Candidates: []uint8{0x40, 0x41, 0x42},
				},
			)
			results <- acquireResult{
				lease: lease,
				err:   err,
			}
		}(ownerID)
	}

	close(start)
	waitGroup.Wait()
	close(results)

	successAddresses := make(map[uint8]int)
	conflictCount := 0

	for result := range results {
		if result.err == nil {
			if _, allowed := availableAddresses[result.lease.Address]; !allowed {
				t.Fatalf("expected successful lease in %#v, got 0x%02X", availableAddresses, result.lease.Address)
			}
			successAddresses[result.lease.Address]++
			continue
		}

		var conflictError LeaseConflictError
		if !errors.As(result.err, &conflictError) {
			t.Fatalf("expected lease conflict error type, got %v", result.err)
		}

		if conflictError.Code != LeaseConflictCodeAddressInUse {
			t.Fatalf(
				"expected conflict code %q, got %q",
				LeaseConflictCodeAddressInUse,
				conflictError.Code,
			)
		}

		conflictCount++
	}

	if len(successAddresses) != len(availableAddresses) {
		t.Fatalf(
			"expected %d successful unique leases, got %d (%#v)",
			len(availableAddresses),
			len(successAddresses),
			successAddresses,
		)
	}

	for address, count := range successAddresses {
		if count != 1 {
			t.Fatalf("expected address 0x%02X acquired once, got %d", address, count)
		}
	}

	if conflictCount != contenders-len(availableAddresses) {
		t.Fatalf(
			"expected %d conflicts, got %d",
			contenders-len(availableAddresses),
			conflictCount,
		)
	}
}
