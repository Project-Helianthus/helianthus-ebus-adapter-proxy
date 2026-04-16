package adapterproxy

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type UpstreamTransport string

const (
	UpstreamENH      UpstreamTransport = "enh"
	UpstreamENS      UpstreamTransport = "ens"
	UpstreamUDPPlain UpstreamTransport = "udp-plain"
	UpstreamTCPPlain UpstreamTransport = "tcp-plain"
)

type Config struct {
	ListenAddr                             string
	UDPPlainListenAddr                     string
	UpstreamTransport                      UpstreamTransport
	UpstreamAddr                           string
	DialTimeout                            time.Duration
	ReadTimeout                            time.Duration
	WriteTimeout                           time.Duration
	AutoJoinWarmup                         time.Duration
	AutoJoinActivityWindow                 time.Duration
	UDPPlainRetryJitter                    float64
	UDPPlainStartWait                      time.Duration
	DisableUDPPlainStartFallback           bool
	EnableExperimentalChildTargetResponder bool
	WireLogPath                            string
	WireLogMaxSize                         int64  // PX14/PX48: max wirelog file size in bytes (0 = no limit)
	SourceAddressPolicy                    string // PX57: reservation mode override ("soft"/"disabled")
	MaxConcurrentSessions                  int           // PX47: max northbound sessions (0 = no limit)
	AcceptRateLimit                        time.Duration // PX53: min interval between accepts (0 = unlimited)
	Debug                                  bool
}

// PX49: Validate checks for configuration errors that would cause silent
// misbehavior (e.g. empty ListenAddr binding to OS-assigned random port).
func (cfg Config) Validate() error {
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return errors.New("ListenAddr is required")
	}
	if strings.TrimSpace(cfg.UpstreamAddr) == "" {
		return errors.New("UpstreamAddr is required")
	}
	// AT-07: Validate SourceAddressPolicy if set.
	if p := strings.TrimSpace(cfg.SourceAddressPolicy); p != "" {
		switch p {
		case "soft", "disabled":
		default:
			return fmt.Errorf("SourceAddressPolicy must be \"soft\" or \"disabled\", got %q", p)
		}
	}
	return nil
}
