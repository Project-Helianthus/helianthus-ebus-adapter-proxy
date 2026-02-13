package targets

import "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/config"

func NewRegistryFromConfig(
	emulationConfiguration config.EmulationConfig,
) (*Registry, error) {
	profiles := make([]Profile, 0, len(emulationConfiguration.TargetProfiles))

	for _, targetProfile := range emulationConfiguration.TargetProfiles {
		profiles = append(profiles, Profile{
			Name:          targetProfile.Name,
			TargetAddress: targetProfile.TargetAddress,
			Enabled:       targetProfile.Enabled,
		})
	}

	return NewRegistry(profiles)
}
