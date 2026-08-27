package adapterproxy

import (
	"os"
	"strings"
	"testing"
)

func TestServerOrchestrationKeepsStartupAcceptAndTeardownOrder(t *testing.T) {
	text := readServerOrchestrationSources(t)

	assertFirstOrder := func(labels ...string) {
		t.Helper()
		last := -1
		for _, label := range labels {
			next := strings.Index(text, label)
			if next < 0 || next < last {
				t.Fatalf("server orchestration order changed at %q: previous=%d current=%d", label, last, next)
			}
			last = next
		}
	}

	assertFirstOrder(
		"dialUpstream(ctx, server.cfg.UpstreamTransport",
		"server.upstream = upstream",
		"server.upstream.SendInit(0x01)",
		"net.Listen(\"tcp\", server.cfg.ListenAddr)",
		"go server.runUpstreamReader(ctx)",
		"listener.Accept()",
	)
	assertTerminalOrder := func(labels ...string) {
		t.Helper()
		last := -1
		for _, label := range labels {
			next := strings.LastIndex(text, label)
			if next < 0 || next < last {
				t.Fatalf("terminal server teardown order changed at %q: previous=%d current=%d", label, last, next)
			}
			last = next
		}
	}
	assertTerminalOrder("_ = listener.Close()", "_ = upstream.Close()", "server.closeSessions()", "server.waitGroup.Wait()", "server.wireLog.Close()")
}

func TestServerOrchestrationKeepsSessionReaderBeforeUnregister(t *testing.T) {
	text := readServerOrchestrationSources(t)

	reader := strings.Index(text, "sessionState.runReader(")
	unregister := strings.Index(text, "server.unregisterSession(sessionState.id)")
	if reader < 0 || unregister < 0 || reader > unregister {
		t.Fatalf("session reader/unregister order changed: reader=%d unregister=%d", reader, unregister)
	}
}

func readServerOrchestrationSources(t *testing.T) string {
	t.Helper()
	var sources []string
	for _, path := range []string{
		"server_lifecycle.go",
		"server_sessions.go",
		"server_start.go",
		"server_upstream_reader.go",
		"server_proxy_routing.go",
		"server_admin.go",
	} {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		sources = append(sources, string(source))
	}
	return strings.Join(sources, "\n")
}
