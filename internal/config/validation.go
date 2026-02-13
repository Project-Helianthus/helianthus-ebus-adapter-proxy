package config

import (
	"fmt"
	"strings"
)

type ValidationError struct {
	Code    string
	Field   string
	Message string
}

type ValidationErrors []ValidationError

func (errors ValidationErrors) Error() string {
	formatted := make([]string, 0, len(errors))

	for _, validationError := range errors {
		formatted = append(
			formatted,
			fmt.Sprintf(
				"%s (%s): %s",
				validationError.Code,
				validationError.Field,
				validationError.Message,
			),
		)
	}

	return "config validation failed: " + strings.Join(formatted, "; ")
}

func Validate(configuration Config) error {
	return configuration.Validate()
}

func (configuration Config) Validate() error {
	validationErrors := make(ValidationErrors, 0)

	validationErrors = append(
		validationErrors,
		validateSouthbound(configuration.Southbound)...,
	)
	validationErrors = append(
		validationErrors,
		validateNorthbound(configuration.Northbound)...,
	)
	validationErrors = append(
		validationErrors,
		validateClients(configuration.Clients)...,
	)
	validationErrors = append(
		validationErrors,
		validateScheduler(configuration.Scheduler)...,
	)
	validationErrors = append(
		validationErrors,
		validateAddressGuard(configuration.AddressGuard)...,
	)
	validationErrors = append(
		validationErrors,
		validateEmulation(configuration.Emulation)...,
	)
	validationErrors = append(
		validationErrors,
		validateSourceAddressPolicy(configuration.SourceAddressPolicy)...,
	)
	validationErrors = append(
		validationErrors,
		validateEmulationAddressGuardCompatibility(
			configuration.Emulation,
			configuration.AddressGuard,
		)...,
	)

	if len(validationErrors) == 0 {
		return nil
	}

	return validationErrors
}

func validateSouthbound(configuration SouthboundConfig) ValidationErrors {
	validationErrors := make(ValidationErrors, 0)

	switch configuration.Transport {
	case SouthboundTransportSerial:
		if strings.TrimSpace(configuration.Serial.Device) == "" {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "southbound.serial.device.required",
				Field:   "southbound.serial.device",
				Message: "serial device is required",
			})
		}

		if configuration.Serial.BaudRate <= 0 {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "southbound.serial.baud_rate.invalid",
				Field:   "southbound.serial.baud_rate",
				Message: "baud rate must be greater than zero",
			})
		}

		if strings.TrimSpace(configuration.TCP.Address) != "" {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "southbound.tcp.address.conflict",
				Field:   "southbound.tcp.address",
				Message: "tcp address must be empty when transport is serial",
			})
		}
	case SouthboundTransportTCP:
		if strings.TrimSpace(configuration.TCP.Address) == "" {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "southbound.tcp.address.required",
				Field:   "southbound.tcp.address",
				Message: "tcp address is required",
			})
		}

		if strings.TrimSpace(configuration.Serial.Device) != "" {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "southbound.serial.device.conflict",
				Field:   "southbound.serial.device",
				Message: "serial device must be empty when transport is tcp",
			})
		}

		if configuration.Serial.BaudRate != 0 {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "southbound.serial.baud_rate.conflict",
				Field:   "southbound.serial.baud_rate",
				Message: "baud rate must be zero when transport is tcp",
			})
		}
	default:
		validationErrors = append(validationErrors, ValidationError{
			Code:    "southbound.transport.invalid",
			Field:   "southbound.transport",
			Message: "transport must be either serial or tcp",
		})
	}

	return validationErrors
}

func validateNorthbound(configuration NorthboundConfig) ValidationErrors {
	validationErrors := make(ValidationErrors, 0)

	if strings.TrimSpace(configuration.Endpoint) == "" {
		validationErrors = append(validationErrors, ValidationError{
			Code:    "northbound.endpoint.required",
			Field:   "northbound.endpoint",
			Message: "endpoint is required",
		})
	}

	return validationErrors
}

func validateClients(configuration ClientsConfig) ValidationErrors {
	validationErrors := make(ValidationErrors, 0)

	if configuration.MaxParallel <= 0 {
		validationErrors = append(validationErrors, ValidationError{
			Code:    "clients.max_parallel.invalid",
			Field:   "clients.max_parallel",
			Message: "max parallel must be greater than zero",
		})
	}

	if configuration.RequestTimeout <= 0 {
		validationErrors = append(validationErrors, ValidationError{
			Code:    "clients.request_timeout.invalid",
			Field:   "clients.request_timeout",
			Message: "request timeout must be greater than zero",
		})
	}

	return validationErrors
}

func validateScheduler(configuration SchedulerConfig) ValidationErrors {
	validationErrors := make(ValidationErrors, 0)

	if configuration.PollInterval <= 0 {
		validationErrors = append(validationErrors, ValidationError{
			Code:    "scheduler.poll_interval.invalid",
			Field:   "scheduler.poll_interval",
			Message: "poll interval must be greater than zero",
		})
	}

	if configuration.MaxJitter < 0 {
		validationErrors = append(validationErrors, ValidationError{
			Code:    "scheduler.max_jitter.invalid",
			Field:   "scheduler.max_jitter",
			Message: "max jitter cannot be negative",
		})
	}

	if configuration.PollInterval > 0 && configuration.MaxJitter >= configuration.PollInterval {
		validationErrors = append(validationErrors, ValidationError{
			Code:    "scheduler.max_jitter.out_of_range",
			Field:   "scheduler.max_jitter",
			Message: "max jitter must be lower than poll interval",
		})
	}

	return validationErrors
}

func validateAddressGuard(configuration AddressGuardConfig) ValidationErrors {
	validationErrors := make(ValidationErrors, 0)

	if !configuration.Enabled {
		if len(configuration.AllowedAddresses) > 0 {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "address_guard.allowed_addresses.forbidden",
				Field:   "address_guard.allowed_addresses",
				Message: "allowed addresses must be empty when address guard is disabled",
			})
		}

		if len(configuration.BlockedAddresses) > 0 {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "address_guard.blocked_addresses.forbidden",
				Field:   "address_guard.blocked_addresses",
				Message: "blocked addresses must be empty when address guard is disabled",
			})
		}

		return validationErrors
	}

	if len(configuration.AllowedAddresses) == 0 && len(configuration.BlockedAddresses) == 0 {
		validationErrors = append(validationErrors, ValidationError{
			Code:    "address_guard.policy.required",
			Field:   "address_guard",
			Message: "at least one address policy must be configured when address guard is enabled",
		})
	}

	validationErrors = append(
		validationErrors,
		validateAddressList("address_guard.allowed_addresses", configuration.AllowedAddresses)...,
	)
	validationErrors = append(
		validationErrors,
		validateAddressList("address_guard.blocked_addresses", configuration.BlockedAddresses)...,
	)

	blockedAddressSet := buildAddressSet(configuration.BlockedAddresses)
	overlapAddressSet := make(map[uint8]struct{})

	for _, allowedAddress := range configuration.AllowedAddresses {
		if _, blocked := blockedAddressSet[allowedAddress]; !blocked {
			continue
		}

		if _, alreadyReported := overlapAddressSet[allowedAddress]; alreadyReported {
			continue
		}

		validationErrors = append(validationErrors, ValidationError{
			Code:    "address_guard.address.overlap",
			Field:   "address_guard",
			Message: fmt.Sprintf("address 0x%02X cannot be both allowed and blocked", allowedAddress),
		})
		overlapAddressSet[allowedAddress] = struct{}{}
	}

	return validationErrors
}

func validateEmulation(configuration EmulationConfig) ValidationErrors {
	validationErrors := make(ValidationErrors, 0)

	if !configuration.Enabled {
		if len(configuration.VirtualSourceAddresses) > 0 {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "emulation.virtual_source_addresses.forbidden",
				Field:   "emulation.virtual_source_addresses",
				Message: "virtual source addresses must be empty when emulation is disabled",
			})
		}

		return validationErrors
	}

	if len(configuration.VirtualSourceAddresses) == 0 {
		validationErrors = append(validationErrors, ValidationError{
			Code:    "emulation.virtual_source_addresses.required",
			Field:   "emulation.virtual_source_addresses",
			Message: "at least one virtual source address is required when emulation is enabled",
		})
	}

	validationErrors = append(
		validationErrors,
		validateAddressList("emulation.virtual_source_addresses", configuration.VirtualSourceAddresses)...,
	)

	return validationErrors
}

func validateSourceAddressPolicy(configuration SourceAddressPolicyConfig) ValidationErrors {
	validationErrors := make(ValidationErrors, 0)

	reservationMode := strings.TrimSpace(configuration.ReservationMode)
	if reservationMode == "" {
		reservationMode = SourceAddressReservationModeSoft
	}

	switch reservationMode {
	case SourceAddressReservationModeSoft, SourceAddressReservationModeDisabled:
	default:
		validationErrors = append(validationErrors, ValidationError{
			Code:    "source_address_policy.reservation_mode.invalid",
			Field:   "source_address_policy.reservation_mode",
			Message: "reservation mode must be either soft or disabled",
		})
	}

	validationErrors = append(
		validationErrors,
		validateAddressList(
			"source_address_policy.allowed_addresses",
			configuration.AllowedAddresses,
		)...,
	)
	validationErrors = append(
		validationErrors,
		validateAddressList(
			"source_address_policy.blocked_addresses",
			configuration.BlockedAddresses,
		)...,
	)
	validationErrors = append(
		validationErrors,
		validateAddressList(
			"source_address_policy.soft_reserved_addresses",
			configuration.SoftReservedAddresses,
		)...,
	)

	blockedAddressSet := buildAddressSet(configuration.BlockedAddresses)
	overlapAddressSet := make(map[uint8]struct{})

	for _, allowedAddress := range configuration.AllowedAddresses {
		if _, blocked := blockedAddressSet[allowedAddress]; !blocked {
			continue
		}

		if _, alreadyReported := overlapAddressSet[allowedAddress]; alreadyReported {
			continue
		}

		validationErrors = append(validationErrors, ValidationError{
			Code:    "source_address_policy.address.overlap",
			Field:   "source_address_policy",
			Message: fmt.Sprintf("address 0x%02X cannot be both allowed and blocked", allowedAddress),
		})
		overlapAddressSet[allowedAddress] = struct{}{}
	}

	return validationErrors
}

func validateEmulationAddressGuardCompatibility(
	emulationConfiguration EmulationConfig,
	addressGuardConfiguration AddressGuardConfig,
) ValidationErrors {
	validationErrors := make(ValidationErrors, 0)

	if !emulationConfiguration.Enabled || !addressGuardConfiguration.Enabled {
		return validationErrors
	}

	uniqueVirtualAddresses := uniqueAddressesInOrder(
		emulationConfiguration.VirtualSourceAddresses,
	)
	blockedAddressSet := buildAddressSet(addressGuardConfiguration.BlockedAddresses)

	for _, virtualAddress := range uniqueVirtualAddresses {
		if _, blocked := blockedAddressSet[virtualAddress]; !blocked {
			continue
		}

		validationErrors = append(validationErrors, ValidationError{
			Code:    "emulation.virtual_source_addresses.blocked",
			Field:   "emulation.virtual_source_addresses",
			Message: fmt.Sprintf("virtual source address 0x%02X is blocked by address guard", virtualAddress),
		})
	}

	if len(addressGuardConfiguration.AllowedAddresses) == 0 {
		return validationErrors
	}

	allowedAddressSet := buildAddressSet(addressGuardConfiguration.AllowedAddresses)

	for _, virtualAddress := range uniqueVirtualAddresses {
		if _, allowed := allowedAddressSet[virtualAddress]; allowed {
			continue
		}

		validationErrors = append(validationErrors, ValidationError{
			Code:    "emulation.virtual_source_addresses.not_allowed",
			Field:   "emulation.virtual_source_addresses",
			Message: fmt.Sprintf("virtual source address 0x%02X is not allowed by address guard", virtualAddress),
		})
	}

	return validationErrors
}

func validateAddressList(field string, addresses []uint8) ValidationErrors {
	validationErrors := make(ValidationErrors, 0)
	seenAddresses := make(map[uint8]struct{})

	for index, address := range addresses {
		if address == 0x00 || address == 0xFF {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "address.invalid_reserved",
				Field:   fmt.Sprintf("%s[%d]", field, index),
				Message: fmt.Sprintf("address 0x%02X is reserved", address),
			})
		}

		if _, seen := seenAddresses[address]; seen {
			validationErrors = append(validationErrors, ValidationError{
				Code:    "address.duplicate",
				Field:   fmt.Sprintf("%s[%d]", field, index),
				Message: fmt.Sprintf("address 0x%02X is duplicated", address),
			})
			continue
		}

		seenAddresses[address] = struct{}{}
	}

	return validationErrors
}

func uniqueAddressesInOrder(addresses []uint8) []uint8 {
	uniqueAddresses := make([]uint8, 0, len(addresses))
	seenAddresses := make(map[uint8]struct{})

	for _, address := range addresses {
		if _, seen := seenAddresses[address]; seen {
			continue
		}

		seenAddresses[address] = struct{}{}
		uniqueAddresses = append(uniqueAddresses, address)
	}

	return uniqueAddresses
}

func buildAddressSet(addresses []uint8) map[uint8]struct{} {
	addressSet := make(map[uint8]struct{}, len(addresses))

	for _, address := range addresses {
		addressSet[address] = struct{}{}
	}

	return addressSet
}
