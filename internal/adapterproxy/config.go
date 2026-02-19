package adapterproxy

import "time"

type UpstreamTransport string

const (
	UpstreamENH      UpstreamTransport = "enh"
	UpstreamUDPPlain UpstreamTransport = "udp-plain"
)

type Config struct {
	ListenAddr             string
	UDPPlainListenAddr     string
	UpstreamTransport      UpstreamTransport
	UpstreamAddr           string
	DialTimeout            time.Duration
	ReadTimeout            time.Duration
	WriteTimeout           time.Duration
	AutoJoinWarmup         time.Duration
	AutoJoinActivityWindow time.Duration
	UDPPlainRetryJitter    float64
	WireLogPath            string
	Debug                  bool
}
