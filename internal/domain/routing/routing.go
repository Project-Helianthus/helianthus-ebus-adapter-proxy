package routing

import (
	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/upstream"
)

type Router interface {
	Route(downstream.Frame) (upstream.Message, error)
}
