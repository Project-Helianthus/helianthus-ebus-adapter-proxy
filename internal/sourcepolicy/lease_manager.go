package sourcepolicy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrLeaseConflict        = errors.New("source address lease conflict")
	ErrLeasePolicyRequired  = errors.New("source address lease policy is required")
	ErrLeaseOwnerRequired   = errors.New("source address lease owner is required")
	ErrInvalidLeaseDuration = errors.New("invalid source address lease duration")
)

type LeaseConflictCode string

const (
	// Lease conflict codes are stable contract values for callers and tests.
	LeaseConflictCodeAddressInUse LeaseConflictCode = "source_address_lease.address_in_use"
	// Lease conflict codes are stable contract values for callers and tests.
	LeaseConflictCodeOwnerAlreadyHasLease LeaseConflictCode = "source_address_lease.owner_already_has_lease"
	// Lease conflict codes are stable contract values for callers and tests.
	LeaseConflictCodeOwnerLeaseMissing LeaseConflictCode = "source_address_lease.owner_lease_missing"
	// Lease conflict codes are stable contract values for callers and tests.
	LeaseConflictCodeOwnerLeaseExpired LeaseConflictCode = "source_address_lease.owner_lease_expired"
)

type LeaseConflictError struct {
	Code           LeaseConflictCode
	OwnerID        string
	Address        uint8
	CurrentOwnerID string
}

func (errorValue LeaseConflictError) Error() string {
	switch errorValue.Code {
	case LeaseConflictCodeAddressInUse:
		return fmt.Sprintf(
			"%s: owner %q cannot acquire address 0x%02X held by owner %q",
			errorValue.Code,
			errorValue.OwnerID,
			errorValue.Address,
			errorValue.CurrentOwnerID,
		)
	case LeaseConflictCodeOwnerAlreadyHasLease:
		return fmt.Sprintf(
			"%s: owner %q already holds address 0x%02X",
			errorValue.Code,
			errorValue.OwnerID,
			errorValue.Address,
		)
	case LeaseConflictCodeOwnerLeaseExpired:
		return fmt.Sprintf(
			"%s: owner %q lease for address 0x%02X has expired",
			errorValue.Code,
			errorValue.OwnerID,
			errorValue.Address,
		)
	default:
		return fmt.Sprintf(
			"%s: owner %q has no active lease",
			errorValue.Code,
			errorValue.OwnerID,
		)
	}
}

func (errorValue LeaseConflictError) Is(target error) bool {
	return target == ErrLeaseConflict
}

type Lease struct {
	OwnerID    string
	Address    uint8
	AcquiredAt time.Time
	RenewedAt  time.Time
	ExpiresAt  time.Time
}

type LeaseManagerOptions struct {
	LeaseDuration  time.Duration
	Clock          func() time.Time
	ActivityWindow *ActivityWindow
}

type AcquireOptions struct {
	Candidates        []uint8
	AllowSoftReserved bool
}

type LeaseManager struct {
	mutex          sync.Mutex
	policy         *Policy
	leaseDuration  time.Duration
	clock          func() time.Time
	activityWindow *ActivityWindow
	leasesByOwner  map[string]Lease
	ownerByAddress map[uint8]string
}

func NewLeaseManager(
	policy *Policy,
	options LeaseManagerOptions,
) (*LeaseManager, error) {
	if policy == nil {
		return nil, ErrLeasePolicyRequired
	}

	if options.LeaseDuration <= 0 {
		return nil, ErrInvalidLeaseDuration
	}

	if options.Clock == nil {
		options.Clock = time.Now
	}

	return &LeaseManager{
		policy:         policy,
		leaseDuration:  options.LeaseDuration,
		clock:          options.Clock,
		activityWindow: options.ActivityWindow,
		leasesByOwner:  make(map[string]Lease),
		ownerByAddress: make(map[uint8]string),
	}, nil
}

func (manager *LeaseManager) Acquire(
	ownerID string,
	options AcquireOptions,
) (Lease, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return Lease{}, ErrLeaseOwnerRequired
	}

	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	now := manager.clock().UTC()
	manager.expireLocked(now)

	if existingLease, found := manager.leasesByOwner[ownerID]; found {
		return Lease{}, LeaseConflictError{
			Code:           LeaseConflictCodeOwnerAlreadyHasLease,
			OwnerID:        ownerID,
			Address:        existingLease.Address,
			CurrentOwnerID: ownerID,
		}
	}

	normalizedCandidates := uniqueSortedAddresses(options.Candidates)
	selectedAddress, err := manager.policy.SelectAddress(
		normalizedCandidates,
		SelectOptions{
			InUseAddresses:    manager.inUseAddressesLocked(),
			AllowSoftReserved: options.AllowSoftReserved,
			ActivityWindow:    manager.activityWindow,
		},
	)
	if err != nil {
		if errors.Is(err, ErrNoSourceAddressAvailable) {
			conflictAddress, currentOwnerID, found := manager.firstAddressConflictLocked(
				normalizedCandidates,
			)
			if found {
				return Lease{}, LeaseConflictError{
					Code:           LeaseConflictCodeAddressInUse,
					OwnerID:        ownerID,
					Address:        conflictAddress,
					CurrentOwnerID: currentOwnerID,
				}
			}
		}

		return Lease{}, err
	}

	lease := Lease{
		OwnerID:    ownerID,
		Address:    selectedAddress,
		AcquiredAt: now,
		RenewedAt:  now,
		ExpiresAt:  now.Add(manager.leaseDuration),
	}
	manager.leasesByOwner[ownerID] = lease
	manager.ownerByAddress[lease.Address] = ownerID

	return lease, nil
}

func (manager *LeaseManager) Renew(ownerID string) (Lease, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return Lease{}, ErrLeaseOwnerRequired
	}

	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	now := manager.clock().UTC()
	if expiredLease, expired := manager.expireOwnerIfNeededLocked(ownerID, now); expired {
		manager.expireLocked(now)
		return Lease{}, LeaseConflictError{
			Code:           LeaseConflictCodeOwnerLeaseExpired,
			OwnerID:        ownerID,
			Address:        expiredLease.Address,
			CurrentOwnerID: ownerID,
		}
	}

	manager.expireLocked(now)
	lease, found := manager.leasesByOwner[ownerID]
	if !found {
		return Lease{}, LeaseConflictError{
			Code:    LeaseConflictCodeOwnerLeaseMissing,
			OwnerID: ownerID,
		}
	}

	lease.RenewedAt = now
	lease.ExpiresAt = now.Add(manager.leaseDuration)
	manager.leasesByOwner[ownerID] = lease

	return lease, nil
}

func (manager *LeaseManager) Release(ownerID string) (Lease, error) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return Lease{}, ErrLeaseOwnerRequired
	}

	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	now := manager.clock().UTC()
	if expiredLease, expired := manager.expireOwnerIfNeededLocked(ownerID, now); expired {
		manager.expireLocked(now)
		return Lease{}, LeaseConflictError{
			Code:           LeaseConflictCodeOwnerLeaseExpired,
			OwnerID:        ownerID,
			Address:        expiredLease.Address,
			CurrentOwnerID: ownerID,
		}
	}

	manager.expireLocked(now)
	lease, found := manager.leasesByOwner[ownerID]
	if !found {
		return Lease{}, LeaseConflictError{
			Code:    LeaseConflictCodeOwnerLeaseMissing,
			OwnerID: ownerID,
		}
	}

	delete(manager.leasesByOwner, ownerID)
	delete(manager.ownerByAddress, lease.Address)

	return lease, nil
}

func (manager *LeaseManager) Expire() []Lease {
	manager.mutex.Lock()
	now := manager.clock().UTC()
	expiredLeases := manager.expireLocked(now)
	manager.mutex.Unlock()

	return expiredLeases
}

func (manager *LeaseManager) expireOwnerIfNeededLocked(
	ownerID string,
	now time.Time,
) (Lease, bool) {
	lease, found := manager.leasesByOwner[ownerID]
	if !found || lease.ExpiresAt.After(now) {
		return Lease{}, false
	}

	delete(manager.leasesByOwner, ownerID)
	delete(manager.ownerByAddress, lease.Address)
	return lease, true
}

func (manager *LeaseManager) expireLocked(now time.Time) []Lease {
	expiredLeases := make([]Lease, 0)
	for ownerID, lease := range manager.leasesByOwner {
		if lease.ExpiresAt.After(now) {
			continue
		}

		delete(manager.leasesByOwner, ownerID)
		delete(manager.ownerByAddress, lease.Address)
		expiredLeases = append(expiredLeases, lease)
	}

	sort.Slice(expiredLeases, func(i, j int) bool {
		if expiredLeases[i].Address != expiredLeases[j].Address {
			return expiredLeases[i].Address < expiredLeases[j].Address
		}

		return expiredLeases[i].OwnerID < expiredLeases[j].OwnerID
	})

	return expiredLeases
}

func (manager *LeaseManager) inUseAddressesLocked() []uint8 {
	addresses := make([]uint8, 0, len(manager.ownerByAddress))
	for address := range manager.ownerByAddress {
		addresses = append(addresses, address)
	}

	sort.Slice(addresses, func(i, j int) bool {
		return addresses[i] < addresses[j]
	})

	return addresses
}

func (manager *LeaseManager) firstAddressConflictLocked(
	candidates []uint8,
) (uint8, string, bool) {
	for _, candidate := range candidates {
		currentOwnerID, found := manager.ownerByAddress[candidate]
		if found {
			return candidate, currentOwnerID, true
		}
	}

	return 0, "", false
}
