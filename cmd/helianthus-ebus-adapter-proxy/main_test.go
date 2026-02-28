package main

import (
	"testing"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/adapterproxy"
)

func TestNormalizeUpstreamEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		wantTransport adapterproxy.UpstreamTransport
		wantAddress   string
		wantErr       bool
	}{
		{
			name:          "enh scheme",
			input:         "enh://127.0.0.1:9999",
			wantTransport: adapterproxy.UpstreamENH,
			wantAddress:   "127.0.0.1:9999",
		},
		{
			name:          "ens scheme",
			input:         "ens://127.0.0.1:9999",
			wantTransport: adapterproxy.UpstreamENS,
			wantAddress:   "127.0.0.1:9999",
		},
		{
			name:          "udp plain scheme",
			input:         "udp-plain://127.0.0.1:9999",
			wantTransport: adapterproxy.UpstreamUDPPlain,
			wantAddress:   "127.0.0.1:9999",
		},
		{
			name:          "tcp plain scheme",
			input:         "tcp-plain://127.0.0.1:9999",
			wantTransport: adapterproxy.UpstreamTCPPlain,
			wantAddress:   "127.0.0.1:9999",
		},
		{
			name:          "hostport defaults to enh",
			input:         "127.0.0.1:9999",
			wantTransport: adapterproxy.UpstreamENH,
			wantAddress:   "127.0.0.1:9999",
		},
		{
			name:    "unsupported scheme",
			input:   "unix:///tmp/socket",
			wantErr: true,
		},
		{
			name:    "missing host",
			input:   "udp-plain://",
			wantErr: true,
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			gotTransport, gotAddress, err := normalizeUpstreamEndpoint(testCase.input)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("normalizeUpstreamEndpoint(%q) error = nil; want non-nil", testCase.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeUpstreamEndpoint(%q) error = %v", testCase.input, err)
			}
			if gotTransport != testCase.wantTransport {
				t.Fatalf("normalizeUpstreamEndpoint(%q) transport = %q; want %q", testCase.input, gotTransport, testCase.wantTransport)
			}
			if gotAddress != testCase.wantAddress {
				t.Fatalf("normalizeUpstreamEndpoint(%q) address = %q; want %q", testCase.input, gotAddress, testCase.wantAddress)
			}
		})
	}
}

func TestNormalizeOptionalListenAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty", input: "", want: ""},
		{name: "valid", input: "127.0.0.1:19011", want: "127.0.0.1:19011"},
		{name: "invalid", input: "127.0.0.1", wantErr: true},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeOptionalListenAddr(testCase.input)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("normalizeOptionalListenAddr(%q) error=nil; want non-nil", testCase.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeOptionalListenAddr(%q) error=%v", testCase.input, err)
			}
			if got != testCase.want {
				t.Fatalf("normalizeOptionalListenAddr(%q) = %q; want %q", testCase.input, got, testCase.want)
			}
		})
	}
}
