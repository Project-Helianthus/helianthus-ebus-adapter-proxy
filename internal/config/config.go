package config

import "time"

const (
	SouthboundTransportSerial = "serial"
	SouthboundTransportTCP    = "tcp"

	SourceAddressReservationModeSoft     = "soft"
	SourceAddressReservationModeDisabled = "disabled"
)

type Config struct {
	Southbound          SouthboundConfig
	Northbound          NorthboundConfig
	Clients             ClientsConfig
	Scheduler           SchedulerConfig
	AddressGuard        AddressGuardConfig
	Emulation           EmulationConfig
	SourceAddressPolicy SourceAddressPolicyConfig
}

type SouthboundConfig struct {
	Transport string
	Serial    SerialSouthboundConfig
	TCP       TCPSouthboundConfig
}

type SerialSouthboundConfig struct {
	Device   string
	BaudRate int
}

type TCPSouthboundConfig struct {
	Address string
}

type NorthboundConfig struct {
	Endpoint    string
	TopicPrefix string
}

type ClientsConfig struct {
	MaxParallel    int
	RequestTimeout time.Duration
}

type SchedulerConfig struct {
	PollInterval time.Duration
	MaxJitter    time.Duration
}

type AddressGuardConfig struct {
	Enabled          bool
	AllowedAddresses []uint8
	BlockedAddresses []uint8
}

type EmulationConfig struct {
	Enabled                bool
	VirtualSourceAddresses []uint8
	TargetProfiles         []EmulatedTargetProfileConfig
}

type EmulatedTargetProfileConfig struct {
	Name          string
	TargetAddress uint8
	Enabled       bool
}

type SourceAddressPolicyConfig struct {
	AllowedAddresses      []uint8
	BlockedAddresses      []uint8
	SoftReservedAddresses []uint8
	ReservationMode       string
}
