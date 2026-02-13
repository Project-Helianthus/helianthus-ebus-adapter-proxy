package targets

const (
	BuiltInProfileVR90Name          = "VR90"
	BuiltInProfileVR90TargetAddress = uint8(0x15)
)

func defaultProfiles() []Profile {
	return []Profile{
		{
			Name:          BuiltInProfileVR90Name,
			TargetAddress: BuiltInProfileVR90TargetAddress,
			Enabled:       false,
		},
	}
}
