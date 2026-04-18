package targets

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	ErrTargetProfileNameRequired = errors.New("target profile name is required")
	ErrTargetProfileNameConflict = errors.New("target profile name conflict")
	ErrTargetAddressReserved     = errors.New("target address is reserved")
	ErrTargetAddressConflict     = errors.New("target address conflict")
	ErrTargetProfileNotFound     = errors.New("target profile not found")
)

const (
	RouteModePassthrough RouteMode = "passthrough"
	RouteModeEmulated    RouteMode = "emulated"
)

type Profile struct {
	Name          string
	TargetAddress uint8
	Enabled       bool
}

type RouteMode string

type RouteSelection struct {
	Mode    RouteMode
	Profile Profile
}

type Registry struct {
	mutex          sync.RWMutex
	profilesByName map[string]Profile
	nameByAddress  map[uint8]string
}

func NewRegistry(initialProfiles []Profile) (*Registry, error) {
	registry := &Registry{
		profilesByName: make(map[string]Profile, len(initialProfiles)),
		nameByAddress:  make(map[uint8]string, len(initialProfiles)),
	}

	for _, profile := range initialProfiles {
		if _, err := registry.registerUnlocked(profile); err != nil {
			return nil, err
		}
	}

	return registry, nil
}

func (registry *Registry) Register(profile Profile) (Profile, error) {
	registry.mutex.Lock()
	registeredProfile, err := registry.registerUnlocked(profile)
	registry.mutex.Unlock()

	if err != nil {
		return Profile{}, err
	}

	return registeredProfile, nil
}

func (registry *Registry) SetEnabled(name string, enabled bool) (Profile, error) {
	nameKey := profileNameKey(name)
	if nameKey == "" {
		return Profile{}, ErrTargetProfileNameRequired
	}

	registry.mutex.Lock()
	profile, found := registry.profilesByName[nameKey]
	if !found {
		registry.mutex.Unlock()
		return Profile{}, fmt.Errorf("%w: %q", ErrTargetProfileNotFound, strings.TrimSpace(name))
	}

	profile.Enabled = enabled
	registry.profilesByName[nameKey] = profile
	registry.mutex.Unlock()

	return profile, nil
}

func (registry *Registry) Enable(name string) (Profile, error) {
	return registry.SetEnabled(name, true)
}

func (registry *Registry) Disable(name string) (Profile, error) {
	return registry.SetEnabled(name, false)
}

func (registry *Registry) LookupByName(name string) (Profile, bool) {
	nameKey := profileNameKey(name)
	if nameKey == "" {
		return Profile{}, false
	}

	registry.mutex.RLock()
	profile, found := registry.profilesByName[nameKey]
	registry.mutex.RUnlock()

	if !found {
		return Profile{}, false
	}

	return profile, true
}

func (registry *Registry) LookupByTargetAddress(targetAddress uint8) (Profile, bool) {
	registry.mutex.RLock()
	nameKey, found := registry.nameByAddress[targetAddress]
	if !found {
		registry.mutex.RUnlock()
		return Profile{}, false
	}

	profile := registry.profilesByName[nameKey]
	registry.mutex.RUnlock()

	return profile, true
}

func (registry *Registry) SelectRoute(targetAddress uint8) RouteSelection {
	profile, found := registry.LookupByTargetAddress(targetAddress)
	if !found || !profile.Enabled {
		return RouteSelection{
			Mode: RouteModePassthrough,
		}
	}

	return RouteSelection{
		Mode:    RouteModeEmulated,
		Profile: profile,
	}
}

func (registry *Registry) Profiles() []Profile {
	registry.mutex.RLock()
	profiles := make([]Profile, 0, len(registry.profilesByName))

	for _, profile := range registry.profilesByName {
		profiles = append(profiles, profile)
	}
	registry.mutex.RUnlock()

	sort.Slice(profiles, func(i, j int) bool {
		return profileNameKey(profiles[i].Name) < profileNameKey(profiles[j].Name)
	})

	return profiles
}

func (registry *Registry) registerUnlocked(profile Profile) (Profile, error) {
	trimmedName := strings.TrimSpace(profile.Name)
	if trimmedName == "" {
		return Profile{}, ErrTargetProfileNameRequired
	}

	// PX32: Reject protocol control symbols as target addresses.
	// 0x00=ACK, 0xFF=NACK, 0xA9=ESC, 0xAA=SYN are all reserved.
	switch profile.TargetAddress {
	case 0x00, 0xFF, 0xA9, 0xAA:
		return Profile{}, fmt.Errorf("%w: 0x%02X", ErrTargetAddressReserved, profile.TargetAddress)
	}

	nameKey := profileNameKey(trimmedName)
	if _, found := registry.profilesByName[nameKey]; found {
		return Profile{}, fmt.Errorf("%w: %q", ErrTargetProfileNameConflict, trimmedName)
	}

	if existingNameKey, found := registry.nameByAddress[profile.TargetAddress]; found {
		existingProfile := registry.profilesByName[existingNameKey]
		return Profile{}, fmt.Errorf(
			"%w: 0x%02X already used by %q",
			ErrTargetAddressConflict,
			profile.TargetAddress,
			existingProfile.Name,
		)
	}

	normalizedProfile := profile
	normalizedProfile.Name = trimmedName

	registry.profilesByName[nameKey] = normalizedProfile
	registry.nameByAddress[normalizedProfile.TargetAddress] = nameKey

	return normalizedProfile, nil
}

func profileNameKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
