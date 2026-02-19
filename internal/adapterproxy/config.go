package adapterproxy

import "time"

type UpstreamTransport string

const (
	UpstreamENH      UpstreamTransport = "enh"
	UpstreamUDPPlain UpstreamTransport = "udp-plain"
)

type Config struct {
	ListenAddr        string
	UpstreamTransport UpstreamTransport
	UpstreamAddr      string
	DialTimeout       time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	WireLogPath       string
	Debug             bool
}
