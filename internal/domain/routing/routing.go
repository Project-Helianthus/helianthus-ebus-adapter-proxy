package routing

import (
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/upstream"
)

type Router interface {
	Route(downstream.Frame) (upstream.Message, error)
}
