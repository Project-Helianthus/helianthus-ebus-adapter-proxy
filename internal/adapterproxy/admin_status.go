package adapterproxy

import (
	"context"
	"fmt"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/admin"
)

// Status implements admin.StatusProvider and exposes runtime session metadata
// for the admin/status HTTP surface.
func (server *Server) Status(_ context.Context) (admin.Status, error) {
	server.mutex.Lock()
	sessions := make([]admin.SessionStatus, 0, len(server.sessions))
	for _, sess := range server.sessions {
		sessionStatus := admin.SessionStatus{
			ID:     fmt.Sprintf("%d", sess.id),
			Client: sess.remoteAddr,
			State:  sessionState(sess),
		}
		if learned, ok := server.learnedBySession[sess.id]; ok {
			initiator := learned.Initiator
			sessionStatus.Initiator = &initiator
			sessionStatus.InitiatorSource = learned.Source
			if !learned.LearnedAt.IsZero() {
				sessionStatus.InitiatorLearnedAt = learned.LearnedAt.UTC().Format(time.RFC3339Nano)
			}
		}
		sessions = append(sessions, sessionStatus)
	}
	server.mutex.Unlock()

	return admin.Status{
		Sessions: sessions,
	}, nil
}

func sessionState(sess *session) string {
	select {
	case <-sess.done:
		return "closed"
	default:
		return "connected"
	}
}
