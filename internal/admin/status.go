package admin

import (
	"context"
	"sort"
)

type Status struct {
	Sessions  []SessionStatus `json:"sessions"`
	Scheduler SchedulerStatus `json:"scheduler"`
	Addresses AddressesStatus `json:"addresses"`
}

type SessionStatus struct {
	ID     string `json:"id"`
	Client string `json:"client"`
	State  string `json:"state"`
}

type SchedulerStatus struct {
	Running      bool   `json:"running"`
	PollInterval string `json:"poll_interval"`
	NextRun      string `json:"next_run"`
}

type AddressesStatus struct {
	GuardEnabled     bool    `json:"guard_enabled"`
	Allowed          []uint8 `json:"allowed"`
	Blocked          []uint8 `json:"blocked"`
	EmulationEnabled bool    `json:"emulation_enabled"`
	Emulated         []uint8 `json:"emulated"`
}

type StatusProvider interface {
	Status(context.Context) (Status, error)
}

func normalizeStatus(status Status) Status {
	status.Sessions = normalizeSessions(status.Sessions)
	status.Addresses.Allowed = normalizeAddresses(status.Addresses.Allowed)
	status.Addresses.Blocked = normalizeAddresses(status.Addresses.Blocked)
	status.Addresses.Emulated = normalizeAddresses(status.Addresses.Emulated)

	return status
}

func normalizeSessions(sessions []SessionStatus) []SessionStatus {
	normalized := append([]SessionStatus(nil), sessions...)

	sort.SliceStable(normalized, func(i, j int) bool {
		if normalized[i].ID != normalized[j].ID {
			return normalized[i].ID < normalized[j].ID
		}

		if normalized[i].Client != normalized[j].Client {
			return normalized[i].Client < normalized[j].Client
		}

		return normalized[i].State < normalized[j].State
	})

	return normalized
}

func normalizeAddresses(addresses []uint8) []uint8 {
	normalized := append([]uint8(nil), addresses...)

	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i] < normalized[j]
	})

	return normalized
}
