package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestReadOnlyEndpointsReturnStableSchemas(t *testing.T) {
	statusProvider := stubStatusProvider{
		status: Status{
			Sessions: []SessionStatus{
				{ID: "s-2", Client: "client-b", State: "idle"},
				{ID: "s-1", Client: "client-a", State: "active"},
			},
			Scheduler: SchedulerStatus{
				Running:      true,
				PollInterval: "5s",
				NextRun:      "2026-02-13T20:00:00Z",
			},
			Addresses: AddressesStatus{
				GuardEnabled:     true,
				Allowed:          []uint8{0x21, 0x03, 0x30},
				Blocked:          []uint8{0x60, 0x10},
				EmulationEnabled: true,
				Emulated:         []uint8{0x80, 0x20},
			},
		},
	}
	httpHandler := NewHandler(statusProvider)

	testCases := []struct {
		path         string
		expectedKeys []string
		validateBody func(*testing.T, []byte)
	}{
		{
			path:         "/health",
			expectedKeys: []string{"status"},
			validateBody: func(t *testing.T, body []byte) {
				t.Helper()

				var response healthResponse
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("failed to unmarshal health response: %v", err)
				}

				if response.Status != "ok" {
					t.Fatalf("expected health status ok, got %q", response.Status)
				}
			},
		},
		{
			path:         "/sessions",
			expectedKeys: []string{"sessions"},
			validateBody: func(t *testing.T, body []byte) {
				t.Helper()

				var response sessionsResponse
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("failed to unmarshal sessions response: %v", err)
				}

				expectedSessions := []SessionStatus{
					{ID: "s-1", Client: "client-a", State: "active"},
					{ID: "s-2", Client: "client-b", State: "idle"},
				}
				if !reflect.DeepEqual(response.Sessions, expectedSessions) {
					t.Fatalf("expected sorted sessions %#v, got %#v", expectedSessions, response.Sessions)
				}

				firstSessionObject := decodeObject(
					t,
					body,
					"sessions",
					0,
				)
				assertObjectKeys(t, firstSessionObject, []string{"id", "client", "state"})
			},
		},
		{
			path:         "/scheduler",
			expectedKeys: []string{"scheduler"},
			validateBody: func(t *testing.T, body []byte) {
				t.Helper()

				schedulerObject := decodeObject(t, body, "scheduler")
				assertObjectKeys(t, schedulerObject, []string{
					"running",
					"poll_interval",
					"next_run",
				})
			},
		},
		{
			path:         "/addresses",
			expectedKeys: []string{"addresses"},
			validateBody: func(t *testing.T, body []byte) {
				t.Helper()

				var response addressesResponse
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatalf("failed to unmarshal addresses response: %v", err)
				}

				expectedAddresses := AddressesStatus{
					GuardEnabled:     true,
					Allowed:          []uint8{0x03, 0x21, 0x30},
					Blocked:          []uint8{0x10, 0x60},
					EmulationEnabled: true,
					Emulated:         []uint8{0x20, 0x80},
				}
				if !reflect.DeepEqual(response.Addresses, expectedAddresses) {
					t.Fatalf("expected normalized addresses %#v, got %#v", expectedAddresses, response.Addresses)
				}

				addressesObject := decodeObject(t, body, "addresses")
				assertObjectKeys(t, addressesObject, []string{
					"guard_enabled",
					"allowed",
					"blocked",
					"emulation_enabled",
					"emulated",
				})
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
			responseRecorder := httptest.NewRecorder()

			httpHandler.ServeHTTP(responseRecorder, request)

			if responseRecorder.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", responseRecorder.Code)
			}

			contentType := responseRecorder.Header().Get("Content-Type")
			if !strings.Contains(contentType, "application/json") {
				t.Fatalf("expected application/json content type, got %q", contentType)
			}

			rootObject := decodeObject(t, responseRecorder.Body.Bytes())
			assertObjectKeys(t, rootObject, testCase.expectedKeys)
			testCase.validateBody(t, responseRecorder.Body.Bytes())
		})
	}
}

func TestReadOnlyEndpointsRejectMutatingMethods(t *testing.T) {
	httpHandler := NewHandler(stubStatusProvider{
		status: Status{},
	})

	paths := []string{"/health", "/sessions", "/scheduler", "/addresses"}
	methods := []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
	}

	for _, path := range paths {
		for _, method := range methods {
			t.Run(path+"_"+method, func(t *testing.T) {
				request := httptest.NewRequest(method, path, nil)
				responseRecorder := httptest.NewRecorder()

				httpHandler.ServeHTTP(responseRecorder, request)

				if responseRecorder.Code != http.StatusMethodNotAllowed {
					t.Fatalf("expected status 405, got %d", responseRecorder.Code)
				}

				if allow := responseRecorder.Header().Get("Allow"); allow != http.MethodGet {
					t.Fatalf("expected Allow header GET, got %q", allow)
				}

				var response errorResponse
				if err := json.Unmarshal(responseRecorder.Body.Bytes(), &response); err != nil {
					t.Fatalf("failed to unmarshal error response: %v", err)
				}

				if response.Error != "method not allowed" {
					t.Fatalf("expected method not allowed error, got %q", response.Error)
				}

				rootObject := decodeObject(t, responseRecorder.Body.Bytes())
				assertObjectKeys(t, rootObject, []string{"error"})
			})
		}
	}
}

func TestStatusEndpointsReturnProviderErrors(t *testing.T) {
	statusProvider := stubStatusProvider{
		err: errors.New("backend unavailable"),
	}
	httpHandler := NewHandler(statusProvider)

	paths := []string{"/sessions", "/scheduler", "/addresses"}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			responseRecorder := httptest.NewRecorder()

			httpHandler.ServeHTTP(responseRecorder, request)

			if responseRecorder.Code != http.StatusInternalServerError {
				t.Fatalf("expected status 500, got %d", responseRecorder.Code)
			}

			var response errorResponse
			if err := json.Unmarshal(responseRecorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("failed to unmarshal error response: %v", err)
			}

			if response.Error != "backend unavailable" {
				t.Fatalf("expected backend unavailable error, got %q", response.Error)
			}

			rootObject := decodeObject(t, responseRecorder.Body.Bytes())
			assertObjectKeys(t, rootObject, []string{"error"})
		})
	}
}

func TestStatusEndpointsUseStaticProviderWhenNil(t *testing.T) {
	httpHandler := NewHandler(nil)

	paths := []string{"/sessions", "/scheduler", "/addresses"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			responseRecorder := httptest.NewRecorder()

			httpHandler.ServeHTTP(responseRecorder, request)

			if responseRecorder.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", responseRecorder.Code)
			}
		})
	}
}

func TestHealthEndpointDoesNotDependOnProvider(t *testing.T) {
	httpHandler := NewHandler(stubStatusProvider{
		err: errors.New("this should not be used for /health"),
	})

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	responseRecorder := httptest.NewRecorder()

	httpHandler.ServeHTTP(responseRecorder, request)

	if responseRecorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", responseRecorder.Code)
	}
}

func TestNewServerSetsAddressAndHandler(t *testing.T) {
	httpServer := NewServer(":8080", stubStatusProvider{})

	if httpServer.Addr != ":8080" {
		t.Fatalf("expected server address :8080, got %q", httpServer.Addr)
	}

	if httpServer.Handler == nil {
		t.Fatalf("expected server handler to be set")
	}
}

type stubStatusProvider struct {
	status Status
	err    error
}

func (provider stubStatusProvider) Status(context.Context) (Status, error) {
	if provider.err != nil {
		return Status{}, provider.err
	}

	return provider.status, nil
}

func decodeObject(t *testing.T, body []byte, path ...interface{}) map[string]interface{} {
	t.Helper()

	var object interface{}
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("failed to unmarshal json body: %v", err)
	}

	node := object
	for _, segment := range path {
		switch typedSegment := segment.(type) {
		case string:
			objectNode, ok := node.(map[string]interface{})
			if !ok {
				t.Fatalf("expected object node at segment %q", typedSegment)
			}
			nextNode, ok := objectNode[typedSegment]
			if !ok {
				t.Fatalf("expected key %q in object", typedSegment)
			}
			node = nextNode
		case int:
			arrayNode, ok := node.([]interface{})
			if !ok {
				t.Fatalf("expected array node at segment %d", typedSegment)
			}
			if typedSegment < 0 || typedSegment >= len(arrayNode) {
				t.Fatalf("index %d out of range for array of len %d", typedSegment, len(arrayNode))
			}
			node = arrayNode[typedSegment]
		default:
			t.Fatalf("unsupported path segment type %T", segment)
		}
	}

	objectNode, ok := node.(map[string]interface{})
	if !ok {
		t.Fatalf("expected object at path %v", path)
	}

	return objectNode
}

func assertObjectKeys(t *testing.T, object map[string]interface{}, expectedKeys []string) {
	t.Helper()

	actualKeys := make([]string, 0, len(object))
	for key := range object {
		actualKeys = append(actualKeys, key)
	}
	sort.Strings(actualKeys)

	sortedExpectedKeys := append([]string(nil), expectedKeys...)
	sort.Strings(sortedExpectedKeys)

	if !reflect.DeepEqual(actualKeys, sortedExpectedKeys) {
		t.Fatalf("expected keys %v, got %v", sortedExpectedKeys, actualKeys)
	}
}
