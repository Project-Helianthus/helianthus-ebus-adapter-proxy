package sourcepolicy

import (
	"errors"
	"testing"
	"time"
)

func TestPolicySoftReserve31WhenEbusdOnlinePrefersAlternative(t *testing.T) {
	policy := mustNewPolicy(t, Config{})

	selectedAddress, err := policy.SelectAddress(
		[]uint8{0x35, 0x31},
		SelectOptions{
			InUseAddresses: []uint8{0x31},
		},
	)
	if err != nil {
		t.Fatalf("expected address selection success, got %v", err)
	}

	if selectedAddress != 0x35 {
		t.Fatalf("expected selected address 0x35, got 0x%02X", selectedAddress)
	}
}

func TestPolicySoftReserve31WhenEbusdOfflineUses31OnlyWithoutAlternatives(t *testing.T) {
	policy := mustNewPolicy(t, Config{})

	selectedAddress, err := policy.SelectAddress(
		[]uint8{0x31, 0x40},
		SelectOptions{},
	)
	if err != nil {
		t.Fatalf("expected address selection success, got %v", err)
	}

	if selectedAddress != 0x40 {
		t.Fatalf("expected selected address 0x40, got 0x%02X", selectedAddress)
	}

	selectedAddress, err = policy.SelectAddress(
		[]uint8{0x31},
		SelectOptions{},
	)
	if err != nil {
		t.Fatalf("expected address selection success, got %v", err)
	}

	if selectedAddress != 0x31 {
		t.Fatalf("expected selected address 0x31 when no alternatives exist, got 0x%02X", selectedAddress)
	}
}

func TestPolicyExplicitAllowCanUse31WithAlternatives(t *testing.T) {
	policy := mustNewPolicy(t, Config{})

	selectedAddress, err := policy.SelectAddress(
		[]uint8{0x31, 0x40},
		SelectOptions{
			AllowSoftReserved: true,
		},
	)
	if err != nil {
		t.Fatalf("expected address selection success, got %v", err)
	}

	if selectedAddress != 0x31 {
		t.Fatalf("expected selected address 0x31 with explicit soft-reserved allow, got 0x%02X", selectedAddress)
	}
}

func TestPolicyAppliesAllowAndDenyFiltersDeterministically(t *testing.T) {
	policy := mustNewPolicy(t, Config{
		AllowedAddresses: []uint8{
			0x31,
			0x40,
			0x41,
		},
		BlockedAddresses: []uint8{
			0x40,
		},
	})

	selectedAddress, err := policy.SelectAddress(
		[]uint8{0x41, 0x40, 0x31},
		SelectOptions{},
	)
	if err != nil {
		t.Fatalf("expected address selection success, got %v", err)
	}

	if selectedAddress != 0x41 {
		t.Fatalf("expected selected address 0x41, got 0x%02X", selectedAddress)
	}
}

func TestPolicyRejectsInvalidReservationMode(t *testing.T) {
	_, err := NewPolicy(Config{
		ReservationMode: "strict",
	})
	if !errors.Is(err, ErrInvalidReservationMode) {
		t.Fatalf("expected invalid reservation mode error, got %v", err)
	}
}

func TestPolicyReturnsNoAddressAvailableWhenAllCandidatesFiltered(t *testing.T) {
	policy := mustNewPolicy(t, Config{
		AllowedAddresses: []uint8{0x31},
	})

	_, err := policy.SelectAddress(
		[]uint8{0x31},
		SelectOptions{
			InUseAddresses: []uint8{0x31},
		},
	)
	if !errors.Is(err, ErrNoSourceAddressAvailable) {
		t.Fatalf("expected no source address available error, got %v", err)
	}
}

func TestPolicyRejectsRecentlyActiveAddressesForNewLeases(t *testing.T) {
	baseTime := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	now := baseTime
	activityWindow := mustNewActivityWindow(t, 5*time.Second, func() time.Time {
		return now
	})
	activityWindow.ObserveAt(0x40, baseTime.Add(-2*time.Second))

	policy := mustNewPolicy(t, Config{})
	_, err := policy.SelectAddress(
		[]uint8{0x40},
		SelectOptions{
			ActivityWindow: activityWindow,
		},
	)
	if !errors.Is(err, ErrRecentlyActiveAddress) {
		t.Fatalf("expected recently active address error, got %v", err)
	}
}

func TestPolicySkipsRecentlyActiveAddressWhenAlternativeExists(t *testing.T) {
	baseTime := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	now := baseTime
	activityWindow := mustNewActivityWindow(t, 5*time.Second, func() time.Time {
		return now
	})
	activityWindow.ObserveAt(0x40, baseTime.Add(-2*time.Second))

	policy := mustNewPolicy(t, Config{})
	selectedAddress, err := policy.SelectAddress(
		[]uint8{0x40, 0x41},
		SelectOptions{
			ActivityWindow: activityWindow,
		},
	)
	if err != nil {
		t.Fatalf("expected address selection success, got %v", err)
	}

	if selectedAddress != 0x41 {
		t.Fatalf("expected selected address 0x41, got 0x%02X", selectedAddress)
	}
}

func TestPolicyAllowsLeaseAtActivityWindowBoundary(t *testing.T) {
	baseTime := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	now := baseTime
	activityWindow := mustNewActivityWindow(t, 5*time.Second, func() time.Time {
		return now
	})
	activityWindow.ObserveAt(0x40, baseTime)
	now = baseTime.Add(5 * time.Second)

	policy := mustNewPolicy(t, Config{})
	selectedAddress, err := policy.SelectAddress(
		[]uint8{0x40},
		SelectOptions{
			ActivityWindow: activityWindow,
		},
	)
	if err != nil {
		t.Fatalf("expected address selection success at boundary, got %v", err)
	}

	if selectedAddress != 0x40 {
		t.Fatalf("expected selected address 0x40, got 0x%02X", selectedAddress)
	}
}

func mustNewPolicy(t *testing.T, configuration Config) *Policy {
	t.Helper()

	policy, err := NewPolicy(configuration)
	if err != nil {
		t.Fatalf("expected policy creation success, got %v", err)
	}

	return policy
}
