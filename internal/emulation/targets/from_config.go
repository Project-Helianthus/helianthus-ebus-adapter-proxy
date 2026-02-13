package targets

import (
	"fmt"
	"strings"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/config"
)

func NewRegistryFromConfig(
	emulationConfiguration config.EmulationConfig,
) (*Registry, error) {
	profiles := defaultProfiles()
	profileIndexByName := make(map[string]int, len(profiles))

	for profileIndex, profile := range profiles {
		profileIndexByName[profileNameKey(profile.Name)] = profileIndex
	}

	registryEnabled := emulationConfiguration.Enabled

	for _, targetProfile := range emulationConfiguration.TargetProfiles {
		profileEnabled := targetProfile.Enabled
		if !registryEnabled {
			profileEnabled = false
		}

		profile := Profile{
			Name:          targetProfile.Name,
			TargetAddress: targetProfile.TargetAddress,
			Enabled:       profileEnabled,
		}

		targetProfileNameKey := profileNameKey(profile.Name)
		if existingProfileIndex, found := profileIndexByName[targetProfileNameKey]; found {
			defaultProfile := profiles[existingProfileIndex]
			if defaultProfile.TargetAddress != profile.TargetAddress {
				return nil, fmt.Errorf(
					"%w: %q requires target address 0x%02X (got 0x%02X)",
					ErrTargetAddressConflict,
					strings.TrimSpace(defaultProfile.Name),
					defaultProfile.TargetAddress,
					profile.TargetAddress,
				)
			}

			profile.Name = defaultProfile.Name
			profiles[existingProfileIndex] = profile
			continue
		}

		profiles = append(profiles, profile)
		profileIndexByName[targetProfileNameKey] = len(profiles) - 1
	}

	if !registryEnabled {
		for profileIndex := range profiles {
			profiles[profileIndex].Enabled = false
		}
	}

	return NewRegistry(profiles)
}
