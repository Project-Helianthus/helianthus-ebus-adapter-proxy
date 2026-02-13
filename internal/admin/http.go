package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type statusHandler func(http.ResponseWriter, *http.Request) error

type handler struct {
	statusProvider StatusProvider
}

func NewServer(address string, statusProvider StatusProvider) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           NewHandler(statusProvider),
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func NewHandler(statusProvider StatusProvider) http.Handler {
	if statusProvider == nil {
		statusProvider = staticStatusProvider{}
	}

	serverHandler := &handler{
		statusProvider: statusProvider,
	}
	mux := http.NewServeMux()

	mux.Handle("/health", readOnly(serverHandler.health))
	mux.Handle("/sessions", readOnly(serverHandler.sessions))
	mux.Handle("/scheduler", readOnly(serverHandler.scheduler))
	mux.Handle("/addresses", readOnly(serverHandler.addresses))

	return mux
}

func readOnly(handler statusHandler) http.Handler {
	return http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		responseWriter.Header().Set("Content-Type", "application/json")

		if request.Method != http.MethodGet {
			responseWriter.Header().Set("Allow", http.MethodGet)
			writeJSON(
				responseWriter,
				http.StatusMethodNotAllowed,
				errorResponse{Error: "method not allowed"},
			)
			return
		}

		if err := handler(responseWriter, request); err != nil {
			writeJSON(
				responseWriter,
				http.StatusInternalServerError,
				errorResponse{Error: err.Error()},
			)
		}
	})
}

func (serverHandler *handler) health(responseWriter http.ResponseWriter, _ *http.Request) error {
	writeJSON(responseWriter, http.StatusOK, healthResponse{Status: "ok"})
	return nil
}

func (serverHandler *handler) sessions(responseWriter http.ResponseWriter, request *http.Request) error {
	status, err := serverHandler.resolveStatus(request)
	if err != nil {
		return err
	}

	writeJSON(
		responseWriter,
		http.StatusOK,
		sessionsResponse{Sessions: status.Sessions},
	)

	return nil
}

func (serverHandler *handler) scheduler(responseWriter http.ResponseWriter, request *http.Request) error {
	status, err := serverHandler.resolveStatus(request)
	if err != nil {
		return err
	}

	writeJSON(
		responseWriter,
		http.StatusOK,
		schedulerResponse{Scheduler: status.Scheduler},
	)

	return nil
}

func (serverHandler *handler) addresses(responseWriter http.ResponseWriter, request *http.Request) error {
	status, err := serverHandler.resolveStatus(request)
	if err != nil {
		return err
	}

	writeJSON(
		responseWriter,
		http.StatusOK,
		addressesResponse{Addresses: status.Addresses},
	)

	return nil
}

func (serverHandler *handler) resolveStatus(request *http.Request) (Status, error) {
	status, err := serverHandler.statusProvider.Status(request.Context())
	if err != nil {
		return Status{}, err
	}

	return normalizeStatus(status), nil
}

func writeJSON(responseWriter http.ResponseWriter, statusCode int, payload interface{}) {
	responseWriter.WriteHeader(statusCode)

	encoder := json.NewEncoder(responseWriter)
	encoder.SetEscapeHTML(false)

	_ = encoder.Encode(payload)
}

type healthResponse struct {
	Status string `json:"status"`
}

type sessionsResponse struct {
	Sessions []SessionStatus `json:"sessions"`
}

type schedulerResponse struct {
	Scheduler SchedulerStatus `json:"scheduler"`
}

type addressesResponse struct {
	Addresses AddressesStatus `json:"addresses"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type staticStatusProvider struct{}

func (staticStatusProvider) Status(_ context.Context) (Status, error) {
	return Status{}, nil
}
