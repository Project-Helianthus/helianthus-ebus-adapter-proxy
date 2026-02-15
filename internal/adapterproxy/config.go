package adapterproxy

import "time"

type Config struct {
	ListenAddr   string
	UpstreamAddr string
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}
