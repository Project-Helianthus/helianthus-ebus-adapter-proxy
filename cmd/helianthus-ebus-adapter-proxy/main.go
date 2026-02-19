package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/adapterproxy"
)

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:19001", "listen address for downstream ebusd-compatible enhanced protocol clients")
	listenUDPPlainAddr := flag.String("listen-udp-plain", "", "optional udp listen address for northbound raw-byte clients")
	upstream := flag.String("upstream", "", "upstream adapter endpoint (e.g. enh://host:port, ens://host:port, or udp-plain://host:port)")
	dialTimeout := flag.Duration("dial-timeout", 3*time.Second, "upstream dial timeout")
	readTimeout := flag.Duration("read-timeout", 200*time.Millisecond, "read timeout applied to upstream and downstream sockets")
	writeTimeout := flag.Duration("write-timeout", 2*time.Second, "write timeout applied to upstream and downstream sockets")
	autoJoinWarmup := flag.Duration("auto-join-warmup", 5*time.Second, "passive warmup duration before selecting auto initiator")
	autoJoinActivityWindow := flag.Duration("auto-join-activity-window", 5*time.Second, "activity freshness window for auto initiator selection")
	udpRetryJitter := flag.Float64("udp-retry-jitter", 0.2, "jitter factor [0..1] for udp-plain arbitration retry backoff")
	wireLogPath := flag.String("wire-log", "", "optional file path to write timestamped upstream tx/rx bytes (no addresses)")
	debug := flag.Bool("debug", false, "enable debug logging (no client addresses)")
	flag.Parse()

	normalizedListen, err := normalizeListenAddr(*listenAddr)
	if err != nil {
		log.Fatalf("invalid -listen: %v", err)
	}
	normalizedUDPPlainListen, err := normalizeOptionalListenAddr(*listenUDPPlainAddr)
	if err != nil {
		log.Fatalf("invalid -listen-udp-plain: %v", err)
	}

	upstreamTransport, normalizedUpstream, err := normalizeUpstreamEndpoint(*upstream)
	if err != nil {
		log.Fatalf("invalid -upstream: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("Starting eBUS adapter proxy")
	log.Printf("Listen: %s", normalizedListen)
	log.Printf("Upstream: (configured)")

	server := adapterproxy.NewServer(adapterproxy.Config{
		ListenAddr:             normalizedListen,
		UDPPlainListenAddr:     normalizedUDPPlainListen,
		UpstreamTransport:      upstreamTransport,
		UpstreamAddr:           normalizedUpstream,
		DialTimeout:            *dialTimeout,
		ReadTimeout:            *readTimeout,
		WriteTimeout:           *writeTimeout,
		AutoJoinWarmup:         *autoJoinWarmup,
		AutoJoinActivityWindow: *autoJoinActivityWindow,
		UDPPlainRetryJitter:    *udpRetryJitter,
		WireLogPath:            strings.TrimSpace(*wireLogPath),
		Debug:                  *debug,
	})

	if err := server.Serve(ctx); err != nil {
		log.Fatalf("proxy exited: %v", err)
	}
}

func normalizeListenAddr(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("listen address is required")
	}
	_, _, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", err
	}
	return trimmed, nil
}

func normalizeOptionalListenAddr(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	_, _, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", err
	}
	return trimmed, nil
}

func normalizeUpstreamEndpoint(raw string) (adapterproxy.UpstreamTransport, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", fmt.Errorf("upstream endpoint is required")
	}

	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", "", err
		}
		if parsed.Host == "" {
			return "", "", fmt.Errorf("upstream endpoint missing host: %q", raw)
		}
		scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
		switch scheme {
		case "enh", "ens", "tcp":
			return adapterproxy.UpstreamENH, parsed.Host, nil
		case "udp-plain":
			return adapterproxy.UpstreamUDPPlain, parsed.Host, nil
		default:
			return "", "", fmt.Errorf("upstream endpoint unsupported scheme %q", scheme)
		}
	}

	_, _, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", "", err
	}
	return adapterproxy.UpstreamENH, trimmed, nil
}
