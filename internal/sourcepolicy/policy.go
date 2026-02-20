package sourcepolicy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	ReservationModeSoft     = "soft"
	ReservationModeDisabled = "disabled"

	DefaultSoftReservedAddress uint8 = 0x31
)

var (
	ErrNoSourceAddressAvailable = errors.New("no source address available")
	ErrInvalidReservationMode   = errors.New("invalid source address reservation mode")
	ErrRecentlyActiveAddress    = errors.New("source address recently active")
)

type Config struct {
	AllowedAddresses      []uint8
	BlockedAddresses      []uint8
	SoftReservedAddresses []uint8
	ReservationMode       string
}

type SelectOptions struct {
	InUseAddresses    []uint8
	AllowSoftReserved bool
	ActivityWindow    *ActivityWindow
}

type Policy struct {
	reservationMode string
	allowedSet      map[uint8]struct{}
	blockedSet      map[uint8]struct{}
	softReservedSet map[uint8]struct{}
}

func NewPolicy(configuration Config) (*Policy, error) {
	reservationMode := strings.TrimSpace(configuration.ReservationMode)
	if reservationMode == "" {
		reservationMode = ReservationModeSoft
	}

	switch reservationMode {
	case ReservationModeSoft, ReservationModeDisabled:
	default:
		return nil, fmt.Errorf(
			"%w: %s",
			ErrInvalidReservationMode,
			configuration.ReservationMode,
		)
	}

	softReserved := uniqueSortedAddresses(configuration.SoftReservedAddresses)
	if len(softReserved) == 0 {
		softReserved = []uint8{DefaultSoftReservedAddress}
	}

	return &Policy{
		reservationMode: reservationMode,
		allowedSet:      buildAddressSet(configuration.AllowedAddresses),
		blockedSet:      buildAddressSet(configuration.BlockedAddresses),
		softReservedSet: buildAddressSet(softReserved),
	}, nil
}

// SelectAddress applies deterministic source address assignment:
// 1) candidates are normalized (sorted, unique, invalid-reserved filtered),
// 2) allow/deny, in-use, and recent-activity filters are applied,
// 3) in soft reservation mode, soft-reserved addresses are avoided unless there are no alternatives.
func (policy *Policy) SelectAddress(
	candidates []uint8,
	options SelectOptions,
) (uint8, error) {
	normalizedCandidates := uniqueSortedAddresses(candidates)
	inUseAddressSet := buildAddressSet(options.InUseAddresses)
	filteredCandidates := make([]uint8, 0, len(normalizedCandidates))
	rejectedRecentlyActive := false

	for _, candidate := range normalizedCandidates {
		if len(policy.allowedSet) > 0 {
			if _, allowed := policy.allowedSet[candidate]; !allowed {
				continue
			}
		}

		if _, blocked := policy.blockedSet[candidate]; blocked {
			continue
		}

		if _, alreadyInUse := inUseAddressSet[candidate]; alreadyInUse {
			continue
		}

		if options.ActivityWindow != nil && options.ActivityWindow.IsRecentlyActive(candidate) {
			rejectedRecentlyActive = true
			continue
		}

		filteredCandidates = append(filteredCandidates, candidate)
	}

	if len(filteredCandidates) == 0 {
		if rejectedRecentlyActive {
			return 0, ErrRecentlyActiveAddress
		}

		return 0, ErrNoSourceAddressAvailable
	}

	if policy.reservationMode == ReservationModeDisabled || options.AllowSoftReserved {
		return filteredCandidates[0], nil
	}

	for _, candidate := range filteredCandidates {
		if _, softReserved := policy.softReservedSet[candidate]; softReserved {
			continue
		}

		return candidate, nil
	}

	return filteredCandidates[0], nil
}

func uniqueSortedAddresses(addresses []uint8) []uint8 {
	if len(addresses) == 0 {
		return nil
	}

	addressSet := buildAddressSet(addresses)
	uniqueAddresses := make([]uint8, 0, len(addressSet))

	for address := range addressSet {
		// 0x00 is reserved as "auto initiator selection" in ENH START handling,
		// so it must not be handed out as a concrete leased source address.
		if address == 0x00 {
			continue
		}

		uniqueAddresses = append(uniqueAddresses, address)
	}

	sort.Slice(uniqueAddresses, func(i, j int) bool {
		return uniqueAddresses[i] < uniqueAddresses[j]
	})

	return uniqueAddresses
}

func buildAddressSet(addresses []uint8) map[uint8]struct{} {
	addressSet := make(map[uint8]struct{}, len(addresses))

	for _, address := range addresses {
		addressSet[address] = struct{}{}
	}

	return addressSet
}
