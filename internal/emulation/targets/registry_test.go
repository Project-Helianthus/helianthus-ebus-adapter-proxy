package targets

import (
	"errors"
	"reflect"
	"testing"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/config"
)

func TestRegistryRegisterAndLookup(t *testing.T) {
	registry, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("expected registry initialization success, got %v", err)
	}

	registeredProfile, err := registry.Register(Profile{
		Name:          "  VR90  ",
		TargetAddress: 0x15,
		Enabled:       false,
	})
	if err != nil {
		t.Fatalf("expected profile registration success, got %v", err)
	}

	if registeredProfile.Name != "VR90" {
		t.Fatalf("expected normalized profile name VR90, got %q", registeredProfile.Name)
	}

	byName, found := registry.LookupByName("vr90")
	if !found {
		t.Fatalf("expected lookup by name to succeed")
	}

	if !reflect.DeepEqual(byName, registeredProfile) {
		t.Fatalf("expected lookup by name result %#v, got %#v", registeredProfile, byName)
	}

	byAddress, found := registry.LookupByTargetAddress(0x15)
	if !found {
		t.Fatalf("expected lookup by target address to succeed")
	}

	if !reflect.DeepEqual(byAddress, registeredProfile) {
		t.Fatalf("expected lookup by address result %#v, got %#v", registeredProfile, byAddress)
	}
}

func TestRegistryRegisterRejectsNameAndAddressConflicts(t *testing.T) {
	registry, err := NewRegistry([]Profile{
		{
			Name:          "vr90",
			TargetAddress: 0x15,
			Enabled:       true,
		},
	})
	if err != nil {
		t.Fatalf("expected registry initialization success, got %v", err)
	}

	_, err = registry.Register(Profile{
		Name:          "VR90",
		TargetAddress: 0x16,
		Enabled:       true,
	})
	if !errors.Is(err, ErrTargetProfileNameConflict) {
		t.Fatalf("expected duplicate name conflict error, got %v", err)
	}

	_, err = registry.Register(Profile{
		Name:          "vr71",
		TargetAddress: 0x15,
		Enabled:       true,
	})
	if !errors.Is(err, ErrTargetAddressConflict) {
		t.Fatalf("expected duplicate target address conflict error, got %v", err)
	}
}

func TestRegistrySelectRouteUsesProfileEnableState(t *testing.T) {
	registry, err := NewRegistry([]Profile{
		{
			Name:          "vr90",
			TargetAddress: 0x15,
			Enabled:       false,
		},
	})
	if err != nil {
		t.Fatalf("expected registry initialization success, got %v", err)
	}

	selection := registry.SelectRoute(0x15)
	if selection.Mode != RouteModePassthrough {
		t.Fatalf("expected passthrough route, got %s", selection.Mode)
	}

	if _, err := registry.Enable("VR90"); err != nil {
		t.Fatalf("expected profile enable success, got %v", err)
	}

	selection = registry.SelectRoute(0x15)
	if selection.Mode != RouteModeEmulated {
		t.Fatalf("expected emulated route, got %s", selection.Mode)
	}
	if selection.Profile.Name != "vr90" {
		t.Fatalf("expected vr90 selected profile, got %q", selection.Profile.Name)
	}

	if _, err := registry.Disable("vr90"); err != nil {
		t.Fatalf("expected profile disable success, got %v", err)
	}

	selection = registry.SelectRoute(0x15)
	if selection.Mode != RouteModePassthrough {
		t.Fatalf("expected passthrough route after disable, got %s", selection.Mode)
	}

	selection = registry.SelectRoute(0x77)
	if selection.Mode != RouteModePassthrough {
		t.Fatalf("expected passthrough route for unknown target, got %s", selection.Mode)
	}
}

func TestRegistryProfilesReturnsDeterministicOrder(t *testing.T) {
	registry, err := NewRegistry([]Profile{
		{
			Name:          "VR71",
			TargetAddress: 0x31,
			Enabled:       true,
		},
		{
			Name:          "vr90",
			TargetAddress: 0x15,
			Enabled:       false,
		},
	})
	if err != nil {
		t.Fatalf("expected registry initialization success, got %v", err)
	}

	orderedProfiles := registry.Profiles()
	expected := []Profile{
		{
			Name:          "VR71",
			TargetAddress: 0x31,
			Enabled:       true,
		},
		{
			Name:          "vr90",
			TargetAddress: 0x15,
			Enabled:       false,
		},
	}

	if !reflect.DeepEqual(orderedProfiles, expected) {
		t.Fatalf("expected deterministic profile snapshot %#v, got %#v", expected, orderedProfiles)
	}
}

func TestNewRegistryFromConfigBuildsProfileRegistry(t *testing.T) {
	registry, err := NewRegistryFromConfig(config.EmulationConfig{
		TargetProfiles: []config.EmulatedTargetProfileConfig{
			{
				Name:          "vr90",
				TargetAddress: 0x15,
				Enabled:       true,
			},
			{
				Name:          "vr71",
				TargetAddress: 0x31,
				Enabled:       false,
			},
		},
	})
	if err != nil {
		t.Fatalf("expected profile registry initialization success, got %v", err)
	}

	selection := registry.SelectRoute(0x15)
	if selection.Mode != RouteModeEmulated {
		t.Fatalf("expected emulated route for configured and enabled profile, got %s", selection.Mode)
	}

	selection = registry.SelectRoute(0x31)
	if selection.Mode != RouteModePassthrough {
		t.Fatalf("expected passthrough route for configured but disabled profile, got %s", selection.Mode)
	}
}
