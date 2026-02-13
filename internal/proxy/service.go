package proxy

import (
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/routing"
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/upstream"
)

type Service struct {
	downstreamClient downstream.Client
	upstreamClient   upstream.Client
	router           routing.Router
}

func NewService(
	downstreamClient downstream.Client,
	upstreamClient upstream.Client,
	router routing.Router,
) *Service {
	return &Service{
		downstreamClient: downstreamClient,
		upstreamClient:   upstreamClient,
		router:           router,
	}
}

func (service *Service) HandleFrame(frame downstream.Frame) error {
	message, err := service.router.Route(frame)
	if err != nil {
		return err
	}

	return service.upstreamClient.Publish(message)
}
