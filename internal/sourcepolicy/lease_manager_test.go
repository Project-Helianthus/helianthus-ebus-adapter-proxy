package sourcepolicy

import (
	"errors"
	"testing"
	"time"
)

func TestLeaseManagerAcquireRenewReleaseLifecycle(t *testing.T) {
	baseTime := time.Date(2026, 2, 2, 9, 30, 0, 0, time.UTC)
	now := baseTime
	manager := mustNewLeaseManager(
		t,
		mustNewPolicy(t, Config{}),
		LeaseManagerOptions{
			LeaseDuration: 5 * time.Second,
			Clock: func() time.Time {
				return now
			},
		},
	)

	acquiredLease, err := manager.Acquire(
		"owner-a",
		AcquireOptions{
			Candidates: []uint8{0x40},
		},
	)
	if err != nil {
		t.Fatalf("expected acquire success, got %v", err)
	}

	if acquiredLease.Address != 0x40 {
		t.Fatalf("expected acquired address 0x40, got 0x%02X", acquiredLease.Address)
	}

	if !acquiredLease.AcquiredAt.Equal(baseTime) {
		t.Fatalf("expected acquired at %v, got %v", baseTime, acquiredLease.AcquiredAt)
	}

	if !acquiredLease.ExpiresAt.Equal(baseTime.Add(5 * time.Second)) {
		t.Fatalf(
			"expected expires at %v, got %v",
			baseTime.Add(5*time.Second),
			acquiredLease.ExpiresAt,
		)
	}

	now = baseTime.Add(2 * time.Second)
	renewedLease, err := manager.Renew("owner-a")
	if err != nil {
		t.Fatalf("expected renew success, got %v", err)
	}

	if !renewedLease.RenewedAt.Equal(now) {
		t.Fatalf("expected renewed at %v, got %v", now, renewedLease.RenewedAt)
	}

	if !renewedLease.ExpiresAt.Equal(now.Add(5 * time.Second)) {
		t.Fatalf(
			"expected renewed expiry at %v, got %v",
			now.Add(5*time.Second),
			renewedLease.ExpiresAt,
		)
	}

	releasedLease, err := manager.Release("owner-a")
	if err != nil {
		t.Fatalf("expected release success, got %v", err)
	}

	if releasedLease.Address != 0x40 {
		t.Fatalf("expected released address 0x40, got 0x%02X", releasedLease.Address)
	}

	reacquiredLease, err := manager.Acquire(
		"owner-a",
		AcquireOptions{
			Candidates: []uint8{0x40},
		},
	)
	if err != nil {
		t.Fatalf("expected reacquire success, got %v", err)
	}

	if reacquiredLease.Address != 0x40 {
		t.Fatalf("expected reacquired address 0x40, got 0x%02X", reacquiredLease.Address)
	}
}

func TestLeaseManagerAcquireAddressConflictCode(t *testing.T) {
	manager := mustNewLeaseManager(
		t,
		mustNewPolicy(t, Config{}),
		LeaseManagerOptions{
			LeaseDuration: 30 * time.Second,
		},
	)

	_, err := manager.Acquire(
		"owner-a",
		AcquireOptions{
			Candidates: []uint8{0x40},
		},
	)
	if err != nil {
		t.Fatalf("expected first acquire success, got %v", err)
	}

	_, err = manager.Acquire(
		"owner-b",
		AcquireOptions{
			Candidates: []uint8{0x40},
		},
	)
	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("expected lease conflict error, got %v", err)
	}

	var conflictError LeaseConflictError
	if !errors.As(err, &conflictError) {
		t.Fatalf("expected lease conflict error type, got %T", err)
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
}

func TestLeaseManagerOwnerConflictCodes(t *testing.T) {
	baseTime := time.Date(2026, 2, 2, 10, 0, 0, 0, time.UTC)
	now := baseTime
	manager := mustNewLeaseManager(
		t,
		mustNewPolicy(t, Config{}),
		LeaseManagerOptions{
			LeaseDuration: 3 * time.Second,
			Clock: func() time.Time {
				return now
			},
		},
	)

	_, err := manager.Acquire(
		"owner-a",
		AcquireOptions{
			Candidates: []uint8{0x40},
		},
	)
	if err != nil {
		t.Fatalf("expected acquire success, got %v", err)
	}

	_, err = manager.Acquire(
		"owner-a",
		AcquireOptions{
			Candidates: []uint8{0x41},
		},
	)
	assertLeaseConflictCode(t, err, LeaseConflictCodeOwnerAlreadyHasLease)

	_, err = manager.Renew("owner-missing")
	assertLeaseConflictCode(t, err, LeaseConflictCodeOwnerLeaseMissing)

	_, err = manager.Release("owner-missing")
	assertLeaseConflictCode(t, err, LeaseConflictCodeOwnerLeaseMissing)

	now = now.Add(4 * time.Second)

	_, err = manager.Renew("owner-a")
	assertLeaseConflictCode(t, err, LeaseConflictCodeOwnerLeaseExpired)

	_, err = manager.Release("owner-a")
	assertLeaseConflictCode(t, err, LeaseConflictCodeOwnerLeaseMissing)
}

func TestLeaseManagerExpireRemovesExpiredLeases(t *testing.T) {
	baseTime := time.Date(2026, 2, 2, 11, 0, 0, 0, time.UTC)
	now := baseTime
	manager := mustNewLeaseManager(
		t,
		mustNewPolicy(t, Config{}),
		LeaseManagerOptions{
			LeaseDuration: 5 * time.Second,
			Clock: func() time.Time {
				return now
			},
		},
	)

	_, err := manager.Acquire(
		"owner-a",
		AcquireOptions{
			Candidates: []uint8{0x40},
		},
	)
	if err != nil {
		t.Fatalf("expected acquire owner-a success, got %v", err)
	}

	now = now.Add(2 * time.Second)
	_, err = manager.Acquire(
		"owner-b",
		AcquireOptions{
			Candidates: []uint8{0x41},
		},
	)
	if err != nil {
		t.Fatalf("expected acquire owner-b success, got %v", err)
	}

	now = baseTime.Add(5 * time.Second)
	expiredLeases := manager.Expire()
	if len(expiredLeases) != 1 {
		t.Fatalf("expected one expired lease at first boundary, got %d", len(expiredLeases))
	}

	if expiredLeases[0].OwnerID != "owner-a" {
		t.Fatalf("expected owner-a to expire first, got %q", expiredLeases[0].OwnerID)
	}

	now = baseTime.Add(7 * time.Second)
	expiredLeases = manager.Expire()
	if len(expiredLeases) != 1 {
		t.Fatalf("expected one expired lease at second boundary, got %d", len(expiredLeases))
	}

	if expiredLeases[0].OwnerID != "owner-b" {
		t.Fatalf("expected owner-b to expire second, got %q", expiredLeases[0].OwnerID)
	}

	_, err = manager.Acquire(
		"owner-c",
		AcquireOptions{
			Candidates: []uint8{0x40},
		},
	)
	if err != nil {
		t.Fatalf("expected acquire after expiration success, got %v", err)
	}
}

func TestLeaseConflictCodesRemainStable(t *testing.T) {
	expectedCodes := map[LeaseConflictCode]string{
		LeaseConflictCodeAddressInUse:         "source_address_lease.address_in_use",
		LeaseConflictCodeOwnerAlreadyHasLease: "source_address_lease.owner_already_has_lease",
		LeaseConflictCodeOwnerLeaseMissing:    "source_address_lease.owner_lease_missing",
		LeaseConflictCodeOwnerLeaseExpired:    "source_address_lease.owner_lease_expired",
	}

	for code, expected := range expectedCodes {
		if string(code) != expected {
			t.Fatalf("expected stable code %q, got %q", expected, code)
		}
	}
}

func TestLeaseManagerRejectsInvalidInputs(t *testing.T) {
	_, err := NewLeaseManager(
		nil,
		LeaseManagerOptions{
			LeaseDuration: 5 * time.Second,
		},
	)
	if !errors.Is(err, ErrLeasePolicyRequired) {
		t.Fatalf("expected lease policy required error, got %v", err)
	}

	_, err = NewLeaseManager(
		mustNewPolicy(t, Config{}),
		LeaseManagerOptions{
			LeaseDuration: 0,
		},
	)
	if !errors.Is(err, ErrInvalidLeaseDuration) {
		t.Fatalf("expected invalid lease duration error, got %v", err)
	}

	manager := mustNewLeaseManager(
		t,
		mustNewPolicy(t, Config{}),
		LeaseManagerOptions{
			LeaseDuration: 5 * time.Second,
		},
	)

	_, err = manager.Acquire(
		" ",
		AcquireOptions{
			Candidates: []uint8{0x40},
		},
	)
	if !errors.Is(err, ErrLeaseOwnerRequired) {
		t.Fatalf("expected lease owner required error, got %v", err)
	}

	_, err = manager.Renew(" ")
	if !errors.Is(err, ErrLeaseOwnerRequired) {
		t.Fatalf("expected lease owner required error, got %v", err)
	}

	_, err = manager.Release("")
	if !errors.Is(err, ErrLeaseOwnerRequired) {
		t.Fatalf("expected lease owner required error, got %v", err)
	}
}

func mustNewLeaseManager(
	t *testing.T,
	policy *Policy,
	options LeaseManagerOptions,
) *LeaseManager {
	t.Helper()

	manager, err := NewLeaseManager(policy, options)
	if err != nil {
		t.Fatalf("expected lease manager creation success, got %v", err)
	}

	return manager
}

func assertLeaseConflictCode(
	t *testing.T,
	err error,
	expectedCode LeaseConflictCode,
) {
	t.Helper()

	if !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("expected lease conflict error, got %v", err)
	}

	var conflictError LeaseConflictError
	if !errors.As(err, &conflictError) {
		t.Fatalf("expected lease conflict error type, got %T", err)
	}

	if conflictError.Code != expectedCode {
		t.Fatalf("expected conflict code %q, got %q", expectedCode, conflictError.Code)
	}
}
