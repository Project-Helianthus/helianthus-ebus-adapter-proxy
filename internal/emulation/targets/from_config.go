package targets

import "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/config"

func NewRegistryFromConfig(
	emulationConfiguration config.EmulationConfig,
) (*Registry, error) {
	profiles := make([]Profile, 0, len(emulationConfiguration.TargetProfiles))
	registryEnabled := emulationConfiguration.Enabled

	for _, targetProfile := range emulationConfiguration.TargetProfiles {
		profileEnabled := targetProfile.Enabled
		if !registryEnabled {
			profileEnabled = false
		}

		profiles = append(profiles, Profile{
			Name:          targetProfile.Name,
			TargetAddress: targetProfile.TargetAddress,
			Enabled:       profileEnabled,
		})
	}

	return NewRegistry(profiles)
}
