package proxy

import (
	"errors"
	"testing"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/upstream"
)

func TestServiceHandleFramePublishesRoutedMessage(t *testing.T) {
	expectedMessage := upstream.Message{
		Topic:   "ebus.frame",
		Payload: []byte{0x01, 0x02},
	}

	router := &stubRouter{
		message: expectedMessage,
	}
	upstreamClient := &stubUpstreamClient{}
	service := NewService(nil, upstreamClient, router)

	err := service.HandleFrame(downstream.Frame{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(upstreamClient.messages) != 1 {
		t.Fatalf("expected one published message, got %d", len(upstreamClient.messages))
	}

	if upstreamClient.messages[0].Topic != expectedMessage.Topic {
		t.Fatalf("expected topic %q, got %q", expectedMessage.Topic, upstreamClient.messages[0].Topic)
	}
}

func TestServiceHandleFrameReturnsRoutingError(t *testing.T) {
	expectedErr := errors.New("route error")
	router := &stubRouter{
		err: expectedErr,
	}
	service := NewService(nil, &stubUpstreamClient{}, router)

	err := service.HandleFrame(downstream.Frame{})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected route error, got %v", err)
	}
}

type stubRouter struct {
	message upstream.Message
	err     error
}

func (router *stubRouter) Route(downstream.Frame) (upstream.Message, error) {
	if router.err != nil {
		return upstream.Message{}, router.err
	}

	return router.message, nil
}

type stubUpstreamClient struct {
	messages []upstream.Message
}

func (client *stubUpstreamClient) Publish(message upstream.Message) error {
	client.messages = append(client.messages, message)

	return nil
}
