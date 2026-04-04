package adapterproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/admin"
	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

func TestServerStatusIncludesLearnedInitiatorMappings(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.sessions = map[uint64]*session{
		1: {id: 1, remoteAddr: "client-a", sendCh: make(chan downstream.Frame, 1), done: make(chan struct{})},
		2: {id: 2, remoteAddr: "client-b", sendCh: make(chan downstream.Frame, 1), done: make(chan struct{})},
	}

	server.learnSessionInitiator(1, 0x31, "start")
	server.learnSessionInitiator(2, 0x71, "request")

	status, err := server.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(status.Sessions) != 2 {
		t.Fatalf("sessions len = %d; want 2", len(status.Sessions))
	}

	for _, sessionStatus := range status.Sessions {
		switch sessionStatus.ID {
		case "1":
			if sessionStatus.Initiator == nil || *sessionStatus.Initiator != 0x31 {
				t.Fatalf("session 1 initiator = %v; want 0x31", sessionStatus.Initiator)
			}
			if sessionStatus.InitiatorSource != "start" {
				t.Fatalf("session 1 initiator_source = %q; want start", sessionStatus.InitiatorSource)
			}
			if sessionStatus.InitiatorLearnedAt == "" {
				t.Fatal("session 1 initiator_learned_at empty; want RFC3339 timestamp")
			}
		case "2":
			if sessionStatus.Initiator == nil || *sessionStatus.Initiator != 0x71 {
				t.Fatalf("session 2 initiator = %v; want 0x71", sessionStatus.Initiator)
			}
			if sessionStatus.InitiatorSource != "request" {
				t.Fatalf("session 2 initiator_source = %q; want request", sessionStatus.InitiatorSource)
			}
			if sessionStatus.InitiatorLearnedAt == "" {
				t.Fatal("session 2 initiator_learned_at empty; want RFC3339 timestamp")
			}
		default:
			t.Fatalf("unexpected session ID %q in status", sessionStatus.ID)
		}
	}
}

func TestAdminSessionsEndpointExposesLearnedInitiatorMappings(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{})
	server.sessions = map[uint64]*session{
		1: {id: 1, remoteAddr: "client-a", sendCh: make(chan downstream.Frame, 1), done: make(chan struct{})},
	}
	server.learnSessionInitiator(1, 0x31, "start")

	handler := admin.NewHandler(server)
	request := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	responseRecorder := httptest.NewRecorder()
	handler.ServeHTTP(responseRecorder, request)

	if responseRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d; want 200", responseRecorder.Code)
	}

	var payload map[string][]map[string]interface{}
	if err := json.Unmarshal(responseRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode JSON payload: %v", err)
	}
	sessions := payload["sessions"]
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d; want 1", len(sessions))
	}

	session := sessions[0]
	if got, ok := session["id"].(string); !ok || got != "1" {
		t.Fatalf("sessions[0].id = %#v; want \"1\"", session["id"])
	}
	if got, ok := session["initiator"].(float64); !ok || uint8(got) != 0x31 {
		t.Fatalf("sessions[0].initiator = %#v; want 49", session["initiator"])
	}
	if got, ok := session["initiator_source"].(string); !ok || got != "start" {
		t.Fatalf("sessions[0].initiator_source = %#v; want \"start\"", session["initiator_source"])
	}
	if got, ok := session["initiator_learned_at"].(string); !ok || got == "" {
		t.Fatalf("sessions[0].initiator_learned_at = %#v; want non-empty string", session["initiator_learned_at"])
	}
}
