package adapterproxy

import (
	"context"
	"log"
	"net"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func (server *Server) registerSession(connection net.Conn) *session {
	server.mutex.Lock()
	server.nextID++
	id := server.nextID
	sessionState := newSession(id, connection, server.cfg.ReadTimeout, server.cfg.WriteTimeout)
	server.sessions[id] = sessionState
	server.mutex.Unlock()
	return sessionState
}

func (server *Server) unregisterSession(sessionID uint64) {
	server.mutex.Lock()
	delete(server.sessions, sessionID)
	delete(server.learnedBySession, sessionID)
	server.clearLocalResponderAssociationsForSessionLocked(sessionID)
	if server.targetResponderWindow.open && server.targetResponderWindow.responderSessionID == sessionID {
		server.targetResponderWindow = targetResponderWindow{}
	}
	server.mutex.Unlock()

	server.releaseBusIfOwner(sessionID)
	server.releaseLease(sessionID)
	server.clearSessionCollision(sessionID)

	server.pendingStartMu.Lock()
	if server.pendingStart != nil && server.pendingStart.sessionID == sessionID {
		server.nilPendingStartLocked()
	}
	server.pendingStartMu.Unlock()

	server.pendingInfoMu.Lock()
	if server.pendingInfo != nil && server.pendingInfo.sessionID == sessionID {
		server.pendingInfo = nil
	}
	server.pendingInfoMu.Unlock()
}

func (server *Server) closeSessions() {
	server.mutex.Lock()
	sessions := make([]*session, 0, len(server.sessions))
	for _, sess := range server.sessions {
		sessions = append(sessions, sess)
	}
	server.sessions = make(map[uint64]*session)
	server.mutex.Unlock()

	for _, sess := range sessions {
		_ = sess.Close()
	}
}

func (server *Server) handleFrame(ctx context.Context, sessionID uint64, frame downstream.Frame) {
	command := southboundenh.ENHCommand(frame.Command)
	if len(frame.Payload) != 1 {
		log.Printf("session=%d frame_dropped cmd=0x%02X payload_len=%d", sessionID, frame.Command, len(frame.Payload))
		return
	}
	data := frame.Payload[0]

	if sessionID != 2 { // Log non-gateway frames
		log.Printf("session=%d frame cmd=0x%02X data=0x%02X", sessionID, frame.Command, data)
	}

	switch command {
	case southboundenh.ENHReqInit:
		initFeatures := server.initResponseFeatures(data)
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResResetted),
			Payload: []byte{initFeatures},
		})
	case southboundenh.ENHReqInfo:
		server.handleInfo(sessionID, data)
	case southboundenh.ENHReqStart:
		server.handleStart(ctx, sessionID, data)
	case southboundenh.ENHReqSend:
		server.handleSend(sessionID, data)
	default:
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
	}
}

func (server *Server) initResponseFeatures(requested byte) byte {
	upstream := byte(server.upstreamFeatures.Load())
	if upstream != 0 {
		return upstream
	}
	return requested
}
