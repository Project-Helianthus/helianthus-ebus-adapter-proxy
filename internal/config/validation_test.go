package config

import (
	"reflect"
	"testing"
	"time"
)

func TestValidateAcceptsValidSerialConfiguration(t *testing.T) {
	configuration := validConfiguration()

	err := Validate(configuration)
	if err != nil {
		t.Fatalf("expected no validation errors, got %v", err)
	}
}

func TestValidateAcceptsValidTCPConfiguration(t *testing.T) {
	configuration := validConfiguration()
	configuration.Southbound.Transport = SouthboundTransportTCP
	configuration.Southbound.Serial = SerialSouthboundConfig{}
	configuration.Southbound.TCP = TCPSouthboundConfig{
		Address: "127.0.0.1:9000",
	}

	err := Validate(configuration)
	if err != nil {
		t.Fatalf("expected no validation errors, got %v", err)
	}
}

func TestValidateReturnsDeterministicErrors(t *testing.T) {
	configuration := Config{
		Southbound: SouthboundConfig{
			Transport: SouthboundTransportSerial,
			Serial: SerialSouthboundConfig{
				Device:   "",
				BaudRate: 0,
			},
			TCP: TCPSouthboundConfig{
				Address: "127.0.0.1:3333",
			},
		},
		Northbound: NorthboundConfig{
			Endpoint: "",
		},
		Clients: ClientsConfig{
			MaxParallel:    0,
			RequestTimeout: 0,
		},
		Scheduler: SchedulerConfig{
			PollInterval: 0,
			MaxJitter:    -time.Second,
		},
		AddressGuard: AddressGuardConfig{
			Enabled:          true,
			AllowedAddresses: []uint8{0x00, 0x21, 0x21, 0x30},
			BlockedAddresses: []uint8{0x30, 0xFF},
		},
		Emulation: EmulationConfig{
			Enabled:                true,
			VirtualSourceAddresses: []uint8{0x40, 0x21, 0x30, 0x30},
		},
	}

	expectedErrors := ValidationErrors{
		{
			Code:    "southbound.serial.device.required",
			Field:   "southbound.serial.device",
			Message: "serial device is required",
		},
		{
			Code:    "southbound.serial.baud_rate.invalid",
			Field:   "southbound.serial.baud_rate",
			Message: "baud rate must be greater than zero",
		},
		{
			Code:    "southbound.tcp.address.conflict",
			Field:   "southbound.tcp.address",
			Message: "tcp address must be empty when transport is serial",
		},
		{
			Code:    "northbound.endpoint.required",
			Field:   "northbound.endpoint",
			Message: "endpoint is required",
		},
		{
			Code:    "clients.max_parallel.invalid",
			Field:   "clients.max_parallel",
			Message: "max parallel must be greater than zero",
		},
		{
			Code:    "clients.request_timeout.invalid",
			Field:   "clients.request_timeout",
			Message: "request timeout must be greater than zero",
		},
		{
			Code:    "scheduler.poll_interval.invalid",
			Field:   "scheduler.poll_interval",
			Message: "poll interval must be greater than zero",
		},
		{
			Code:    "scheduler.max_jitter.invalid",
			Field:   "scheduler.max_jitter",
			Message: "max jitter cannot be negative",
		},
		{
			Code:    "address.invalid_reserved",
			Field:   "address_guard.allowed_addresses[0]",
			Message: "address 0x00 is reserved",
		},
		{
			Code:    "address.duplicate",
			Field:   "address_guard.allowed_addresses[2]",
			Message: "address 0x21 is duplicated",
		},
		{
			Code:    "address.invalid_reserved",
			Field:   "address_guard.blocked_addresses[1]",
			Message: "address 0xFF is reserved",
		},
		{
			Code:    "address_guard.address.overlap",
			Field:   "address_guard",
			Message: "address 0x30 cannot be both allowed and blocked",
		},
		{
			Code:    "address.duplicate",
			Field:   "emulation.virtual_source_addresses[3]",
			Message: "address 0x30 is duplicated",
		},
		{
			Code:    "emulation.virtual_source_addresses.blocked",
			Field:   "emulation.virtual_source_addresses",
			Message: "virtual source address 0x30 is blocked by address guard",
		},
		{
			Code:    "emulation.virtual_source_addresses.not_allowed",
			Field:   "emulation.virtual_source_addresses",
			Message: "virtual source address 0x40 is not allowed by address guard",
		},
	}

	actualErrors, ok := Validate(configuration).(ValidationErrors)
	if !ok {
		t.Fatalf("expected ValidationErrors type")
	}

	if !reflect.DeepEqual(actualErrors, expectedErrors) {
		t.Fatalf("unexpected validation errors:\nexpected: %#v\nactual: %#v", expectedErrors, actualErrors)
	}

	repeatedErrors, ok := Validate(configuration).(ValidationErrors)
	if !ok {
		t.Fatalf("expected ValidationErrors type")
	}

	if !reflect.DeepEqual(actualErrors, repeatedErrors) {
		t.Fatalf("expected deterministic validation errors")
	}
}

func TestValidateRejectsDisabledPoliciesWithData(t *testing.T) {
	configuration := validConfiguration()
	configuration.AddressGuard.Enabled = false
	configuration.AddressGuard.AllowedAddresses = []uint8{0x21}
	configuration.Emulation.Enabled = false
	configuration.Emulation.VirtualSourceAddresses = []uint8{0x21}

	expectedErrors := ValidationErrors{
		{
			Code:    "address_guard.allowed_addresses.forbidden",
			Field:   "address_guard.allowed_addresses",
			Message: "allowed addresses must be empty when address guard is disabled",
		},
		{
			Code:    "emulation.virtual_source_addresses.forbidden",
			Field:   "emulation.virtual_source_addresses",
			Message: "virtual source addresses must be empty when emulation is disabled",
		},
	}

	actualErrors, ok := Validate(configuration).(ValidationErrors)
	if !ok {
		t.Fatalf("expected ValidationErrors type")
	}

	if !reflect.DeepEqual(actualErrors, expectedErrors) {
		t.Fatalf("unexpected validation errors:\nexpected: %#v\nactual: %#v", expectedErrors, actualErrors)
	}
}

func validConfiguration() Config {
	return Config{
		Southbound: SouthboundConfig{
			Transport: SouthboundTransportSerial,
			Serial: SerialSouthboundConfig{
				Device:   "/dev/ttyUSB0",
				BaudRate: 9600,
			},
		},
		Northbound: NorthboundConfig{
			Endpoint:    "127.0.0.1:9001",
			TopicPrefix: "helianthus",
		},
		Clients: ClientsConfig{
			MaxParallel:    4,
			RequestTimeout: 3 * time.Second,
		},
		Scheduler: SchedulerConfig{
			PollInterval: 5 * time.Second,
			MaxJitter:    1 * time.Second,
		},
		AddressGuard: AddressGuardConfig{
			Enabled:          true,
			AllowedAddresses: []uint8{0x21, 0x30},
		},
		Emulation: EmulationConfig{
			Enabled:                true,
			VirtualSourceAddresses: []uint8{0x21},
		},
	}
}
