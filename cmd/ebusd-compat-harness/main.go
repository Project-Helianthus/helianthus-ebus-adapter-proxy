package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/compat/ebusd"
)

func main() {
	timeout := flag.Duration(
		"timeout",
		10*time.Second,
		"maximum duration for the local compatibility harness run",
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	result, err := ebusd.RunConfigOnlyMigrationHarness(ctx, nil)
	if err != nil {
		fmt.Printf("RESULT: FAIL (harness error: %v)\n", err)
		os.Exit(1)
	}

	fmt.Println("Issue #14 ebusd compatibility harness")
	fmt.Println("MODE: sim (local topology with mock adapter)")
	fmt.Println("MIGRATION: config-only endpoint switch (host/port)")
	fmt.Printf("DIRECT_ENDPOINT=%s\n", result.AdapterEndpoint)
	fmt.Printf("PROXY_ENDPOINT=%s\n", result.ProxyEndpoint)

	failed := false
	for _, exchange := range result.Exchanges {
		status := "PASS"
		if !exchange.Match {
			status = "FAIL"
			failed = true
		}

		fmt.Printf(
			"%s %s request=%s direct_response=%s proxy_response=%s\n",
			status,
			exchange.Name,
			formatPair(exchange.Request),
			formatPair(exchange.DirectResponse),
			formatPair(exchange.ProxyResponse),
		)
	}

	if failed || !result.Compatible() {
		fmt.Println("RESULT: FAIL (proxy responses diverge from direct endpoint)")
		os.Exit(1)
	}

	fmt.Println("RESULT: PASS (config-only migration verified; no ebusd patch required)")
}

func formatPair(sequence [2]byte) string {
	return fmt.Sprintf("0x%02X 0x%02X", sequence[0], sequence[1])
}
