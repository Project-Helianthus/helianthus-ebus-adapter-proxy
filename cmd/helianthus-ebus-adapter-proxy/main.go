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
	upstream := flag.String("upstream", "", "upstream enhanced endpoint (e.g. enh://host:port or ens://host:port)")
	dialTimeout := flag.Duration("dial-timeout", 3*time.Second, "upstream dial timeout")
	readTimeout := flag.Duration("read-timeout", 200*time.Millisecond, "read timeout applied to upstream and downstream sockets")
	writeTimeout := flag.Duration("write-timeout", 2*time.Second, "write timeout applied to upstream and downstream sockets")
	debug := flag.Bool("debug", false, "enable debug logging (no client addresses)")
	flag.Parse()

	normalizedListen, err := normalizeListenAddr(*listenAddr)
	if err != nil {
		log.Fatalf("invalid -listen: %v", err)
	}

	normalizedUpstream, err := normalizeUpstreamEndpoint(*upstream)
	if err != nil {
		log.Fatalf("invalid -upstream: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("Starting eBUS adapter proxy")
	log.Printf("Listen: %s", normalizedListen)
	log.Printf("Upstream: (configured)")

	server := adapterproxy.NewServer(adapterproxy.Config{
		ListenAddr:   normalizedListen,
		UpstreamAddr: normalizedUpstream,
		DialTimeout:  *dialTimeout,
		ReadTimeout:  *readTimeout,
		WriteTimeout: *writeTimeout,
		Debug:        *debug,
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

func normalizeUpstreamEndpoint(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("upstream endpoint is required")
	}

	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", err
		}
		if parsed.Host == "" {
			return "", fmt.Errorf("upstream endpoint missing host: %q", raw)
		}
		return parsed.Host, nil
	}

	_, _, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", err
	}
	return trimmed, nil
}
