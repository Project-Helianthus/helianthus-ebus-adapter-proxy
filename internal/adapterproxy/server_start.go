package adapterproxy

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/sourcepolicy"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func (server *Server) handleStart(ctx context.Context, sessionID uint64, initiator byte) {
	server.mutex.Lock()
	sess := server.sessions[sessionID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}

	log.Printf("session=%d handle_start initiator=0x%02X", sessionID, initiator)

	if initiator == ebusSyn {
		server.handleStartCancel(sessionID)
		if !server.isWirePlainUpstream() {
			// Forward best-effort cancellation upstream. The enhanced protocol does not
			// mandate a response for START+SYN, so we must not block waiting for one.
			_ = server.writeUpstreamSerialized(downstream.Frame{
				Command: byte(southboundenh.ENHReqStart),
				Payload: []byte{initiator},
			})
		}
		return
	}

	if initiator == 0x00 {
		if learnedAddress, ok := server.sessionLearnedInitiator(sessionID); ok {
			initiator = learnedAddress
			if server.cfg.Debug {
				log.Printf("session=%d auto_join_reuse_learned=0x%02X", sessionID, learnedAddress)
			}
		} else if leasedAddress, ok := server.sessionLeaseAddress(sessionID); ok {
			initiator = leasedAddress
			if server.cfg.Debug {
				log.Printf("session=%d auto_join_reuse_lease=0x%02X", sessionID, leasedAddress)
			}
		} else {
			selected, err := server.selectAutoInitiator()
			if err != nil {
				server.reply(sessionID, downstream.Frame{
					Command: byte(southboundenh.ENHResErrorHost),
					Payload: []byte{0x00},
				})
				return
			}
			initiator = selected
		}
		if server.cfg.Debug {
			log.Printf("session=%d auto_join_initiator=0x%02X", sessionID, initiator)
		}
	}

	// GH-P1/PX25: Validate initiator address before lease acquisition.
	// Rejects invalid nibble patterns (e.g. 0x22) that would cause
	// unpredictable adapter behavior.
	if !isInitiatorAddress(initiator) {
		log.Printf("session=%d invalid_initiator=0x%02X", sessionID, initiator)
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	// PX24/CR4-P1a/CR7-P2: Reject explicit initiator if recently observed on
	// the bus from an external device AND this session does not already own a
	// non-expired lease for the same address.
	leaseAddr, hasLease := server.sessionLeaseAddress(sessionID)
	hasActiveLease := hasLease && leaseAddr == initiator
	if hasActiveLease {
		// CR7-P2: Verify the lease hasn't expired — expired entries can
		// linger in leasedBySess until periodic expiry runs.
		server.leasesMu.Lock()
		if existing, ok := server.leasedBySess[sessionID]; ok && existing.ExpiresAt.Before(time.Now()) {
			hasActiveLease = false
		}
		server.leasesMu.Unlock()
	}
	if !hasActiveLease {
		if server.isObservedExternalInitiator(initiator) {
			log.Printf("session=%d explicit_initiator_rejected_observed initiator=0x%02X", sessionID, initiator)
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResFailed),
				Payload: []byte{initiator},
			})
			return
		}
	}

	selectedInitiator, err := server.acquireLease(sessionID, initiator)
	if err != nil {
		log.Printf(
			"session=%d lease_rejected initiator=0x%02X reason=%v",
			sessionID,
			initiator,
			err,
		)
		var conflict sourcepolicy.LeaseConflictError
		if errors.As(err, &conflict) && conflict.Code == sourcepolicy.LeaseConflictCodeAddressInUse {
			winner := conflict.Address
			if winner == 0x00 {
				winner = initiator
			}
			server.markSessionCollision(sessionID, winner)
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResFailed),
				Payload: []byte{winner},
			})
			return
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}
	initiator = selectedInitiator

	if server.isWirePlainUpstream() {
		server.handleStartUDPPlain(ctx, sessionID, initiator)
		return
	}

	ownedBySession := func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		if server.busOwner != sessionID {
			return false
		}
		// Prevent indefinite ownership chaining: force token re-acquisition
		// after maxOwnershipDuration so other sessions get a fair chance.
		if !server.busOwned.IsZero() && time.Since(server.busOwned) > maxOwnershipDuration {
			return false
		}
		return true
	}()

	if !ownedBySession {
		// If we were the owner but exceeded maxOwnershipDuration, release first.
		server.releaseBusIfOwner(sessionID)

		waitStart := time.Now()
		if !server.waitForStartArbitration(ctx, sess, sessionID, initiator) {
			return
		}

		if server.cfg.Debug {
			log.Printf("session=%d start_wait=%s", sessionID, time.Since(waitStart))
		}
	} else {
		server.mutex.Lock()
		if server.busOwner == sessionID {
			server.busDirty = true
		}
		server.mutex.Unlock()
		if server.cfg.Debug {
			log.Printf("session=%d start_reuse_owner=true", sessionID)
		}
	}

	// If we acquired the bus token above, it is now held until SYN (end-of-message)
	// or an error/disconnect. If we are reusing an existing ownership, do not touch
	// the token here.

	// Register pendingStart immediately after busToken acquire so the RESETTED
	// handler can always see and abort it. Without this, a RESETTED arriving in
	// the gap between busToken acquire and pendingStart set would go unnoticed,
	// causing a hang bounded only by the 5s respCh timeout.
	respCh := make(chan downstream.Frame, 1)
	server.pendingStartMu.Lock()
	server.pendingStart = &pendingStart{
		sessionID: sessionID,

		respCh:    respCh,
		mode:      pendingStartModeENH,
		initiator: initiator,
	}
	server.pendingStartMu.Unlock()

	select {
	case <-ctx.Done():
		server.clearPendingStart(sessionID)
		select {
		case <-respCh:
		default:
		}
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		if !ownedBySession {
			server.releaseBusToken()
		}
		return
	case <-sess.done:
		server.clearPendingStart(sessionID)
		select {
		case <-respCh:
		default:
		}
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		if !ownedBySession {
			server.releaseBusToken()
		}
		return
	default:
	}

	startFrame := downstream.Frame{
		Command: byte(southboundenh.ENHReqStart),
		Payload: []byte{initiator},
	}
	if err := server.writeUpstreamSerialized(startFrame); err != nil {
		server.clearPendingStart(sessionID)
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		if !ownedBySession {
			server.releaseBusToken()
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	select {
	case response := <-respCh:
		server.clearPendingStart(sessionID)
		if server.cfg.Debug {
			log.Printf(
				"session=%d start_resp cmd=0x%02X data=0x%02X",
				sessionID,
				response.Command,
				response.Payload[0],
			)
		}
		switch southboundenh.ENHCommand(response.Command) {
		case southboundenh.ENHResStarted:
			server.setBusOwner(sessionID, initiator)
			server.clearSessionCollision(sessionID)
			return
		default:
			if southboundenh.ENHCommand(response.Command) == southboundenh.ENHResFailed {
				if len(response.Payload) == 1 {
					server.markSessionCollision(sessionID, response.Payload[0])
				} else {
					server.markSessionCollision(sessionID, 0x00)
				}
			}
			if !ownedBySession {
				server.releaseLease(sessionID)
			}
			if southboundenh.ENHCommand(response.Command) == southboundenh.ENHResFailed ||
				southboundenh.ENHCommand(response.Command) == southboundenh.ENHResErrorEBUS ||
				southboundenh.ENHCommand(response.Command) == southboundenh.ENHResErrorHost {
				server.releaseBusIfOwner(sessionID)
			}
			// 50ms collision backoff for PIC16F firmware race: hold the
			// bus token briefly so no session can re-START immediately.
			select {
			case <-time.After(enhCollisionBackoff):
			case <-ctx.Done():
			}
			if !ownedBySession {
				server.releaseBusToken()
			}
			return
		}
	case <-time.After(5 * time.Second):
		server.clearPendingStart(sessionID)
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		server.releaseBusIfOwner(sessionID)
		if !ownedBySession {
			server.releaseBusToken()
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	case <-server.upstreamLost:
		server.clearPendingStart(sessionID)
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		server.releaseBusIfOwner(sessionID)
		if !ownedBySession {
			server.releaseBusToken()
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	case <-sess.done:
		server.clearPendingStart(sessionID)
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		server.releaseBusIfOwner(sessionID)
		if !ownedBySession {
			server.releaseBusToken()
		}
		return
	case <-ctx.Done():
		server.clearPendingStart(sessionID)
		if !ownedBySession {
			server.releaseLease(sessionID)
		}
		server.releaseBusIfOwner(sessionID)
		if !ownedBySession {
			server.releaseBusToken()
		}
		return
	}
}

func (server *Server) handleStartCancel(sessionID uint64) {
	server.pendingStartMu.Lock()
	pending := server.pendingStart
	if pending != nil && pending.sessionID == sessionID {
		server.nilPendingStartLocked()
	}
	server.pendingStartMu.Unlock()

	// PX17: Send cancel signal on respCh so handleStart goroutine does not
	// wait the full 5s timeout before noticing the cancellation.
	if pending != nil && pending.sessionID == sessionID && pending.respCh != nil {
		cancelFrame := downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		}
		select {
		case pending.respCh <- cancelFrame:
		default:
		}
	}

	server.releaseBusIfOwner(sessionID)
	server.releaseLease(sessionID)
}

func (server *Server) handleStartUDPPlain(ctx context.Context, sessionID uint64, initiator byte) {
	server.mutex.Lock()
	sess := server.sessions[sessionID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}

	ownedBySession := func() bool {
		server.mutex.Lock()
		defer server.mutex.Unlock()
		if server.busOwner != sessionID {
			return false
		}
		// Match the ENH upstream path: a session may reuse ownership only for
		// a bounded window, otherwise lower-latency restarts can starve peers.
		if !server.busOwned.IsZero() && time.Since(server.busOwned) > maxOwnershipDuration {
			return false
		}
		return true
	}()

	if ownedBySession {
		server.clearSessionCollision(sessionID)
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResStarted),
			Payload: []byte{initiator},
		})
		return
	}

	// If this session held the token past the bounded ownership window,
	// release it before rejoining the shared FIFO arbitration queue.
	server.releaseBusIfOwner(sessionID)

	waitStart := time.Now()
	if !server.waitForStartArbitration(ctx, sess, sessionID, initiator) {
		server.releaseLease(sessionID)
		return
	}
	if server.cfg.Debug {
		log.Printf("session=%d start_wait=%s", sessionID, time.Since(waitStart))
	}

	defer func() {
		server.mutex.Lock()
		owner := server.busOwner
		server.mutex.Unlock()
		if owner != sessionID {
			server.releaseLease(sessionID)
			server.releaseBusToken()
		}
	}()

	for attempt := 0; attempt < udpPlainMaxAttempts; attempt++ {
		server.clearSynSignal()

		waitedForSyn, ok := server.waitForUDPPlainIdleSyn(ctx, sess, sessionID, attempt+1)
		if !ok {
			return
		}
		if server.cfg.Debug && waitedForSyn {
			log.Printf("session=%d attempt=%d udp_plain_syn_acquired=true", sessionID, attempt+1)
		}

		// Register pendingStart AFTER SYN wait — registering before would
		// cause deliverPendingStartFromArbByte to consume unrelated bus
		// bytes as arbitration results before we've sent our initiator.
		respCh := make(chan downstream.Frame, 1)
		server.pendingStartMu.Lock()
		server.pendingStart = &pendingStart{
			sessionID: sessionID,

			respCh:    respCh,
			mode:      pendingStartModeUDPPlain,
			initiator: initiator,
		}
		server.pendingStartMu.Unlock()

		if err := server.writeUpstreamSerialized(downstream.Frame{
			Command: byte(southboundenh.ENHReqSend),
			Payload: []byte{initiator},
		}); err != nil {
			server.clearPendingStart(sessionID)
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResErrorHost),
				Payload: []byte{0x00},
			})
			return
		}

		select {
		case response := <-respCh:
			server.clearPendingStart(sessionID)
			switch southboundenh.ENHCommand(response.Command) {
			case southboundenh.ENHResStarted:
				server.setBusOwner(sessionID, initiator)
				server.clearSessionCollision(sessionID)
				server.reply(sessionID, downstream.Frame{
					Command: byte(southboundenh.ENHResStarted),
					Payload: []byte{initiator},
				})
				return
			case southboundenh.ENHResFailed:
				if attempt+1 >= udpPlainMaxAttempts {
					if len(response.Payload) == 1 {
						server.markSessionCollision(sessionID, response.Payload[0])
					} else {
						server.markSessionCollision(sessionID, 0x00)
					}
					server.reply(sessionID, downstream.Frame{
						Command: byte(southboundenh.ENHResFailed),
						Payload: append([]byte(nil), response.Payload...),
					})
					return
				}
				if server.cfg.Debug && len(response.Payload) == 1 {
					log.Printf(
						"session=%d attempt=%d arbitration_failed=0x%02X retrying=true",
						sessionID,
						attempt+1,
						response.Payload[0],
					)
				}
				backoff := udpPlainRetryBackoffWithJitter(attempt, server.cfg.UDPPlainRetryJitter, server.randomFloat64)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				case <-sess.done:
					return
				}
				continue
			default:
				return
			}
		case <-time.After(server.cfg.UDPPlainStartWait):
			server.clearPendingStart(sessionID)
			if server.cfg.DisableUDPPlainStartFallback {
				server.releaseLease(sessionID)
				server.reply(sessionID, downstream.Frame{
					Command: byte(southboundenh.ENHResErrorHost),
					Payload: []byte{0x00},
				})
				return
			}
			// PX43: Unconditional log for unconfirmed ownership.
			log.Printf("session=%d start_timeout_fallback=true ownership=unconfirmed", sessionID)
			server.setBusOwner(sessionID, initiator)
			server.clearSessionCollision(sessionID)
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResStarted),
				Payload: []byte{initiator},
			})
			return
		case <-ctx.Done():
			server.clearPendingStart(sessionID)
			server.releaseLease(sessionID)
			return
		case <-sess.done:
			server.clearPendingStart(sessionID)
			server.releaseLease(sessionID)
			return
		}
	}
}

// handleInfo implements single-flight INFO semantics: only one INFO request
// can be pending at a time. If a second session sends INFO while one is
// pending, the first is evicted with ErrorHost. This is a deliberate
// backpressure choice — the adapter's INFO response path is not multiplexed,
// so concurrent INFO would corrupt the response stream. Clients that need
// guaranteed INFO delivery should retry after ErrorHost.
func (server *Server) handleInfo(sessionID uint64, infoID byte) {
	server.mutex.Lock()
	sess := server.sessions[sessionID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}

	// Serve identity IDs from cache if available.
	if cached := server.infoCache.get(infoID); cached != nil {
		for _, f := range cached {
			server.reply(sessionID, f)
		}
		return
	}

	// PX60/PX68/PX6/PX22/PX34/PX65/AT-05: Guard concurrent handleInfo — if
	// another session has a pending INFO, capture it for eviction and replace
	// atomically under the same lock to prevent three-way races.
	server.pendingInfoMu.Lock()
	var evictSession uint64
	if existing := server.pendingInfo; existing != nil {
		evictSession = existing.sessionID
		// Evict ANY existing pending INFO (same or different session).
		// Same-session overlap can corrupt response correlation since INFO
		// responses carry no request identifier.
	}
	server.pendingInfoSeq++
	server.pendingInfo = &pendingInfo{
		sessionID: sessionID,
		seq:       server.pendingInfoSeq,
		remaining: -1,
		infoID:    infoID,
		createdAt: time.Now(),
	}
	// Capture seq under lock before releasing — used for post-write ownership check.
	mySeq := server.pendingInfoSeq
	server.pendingInfoMu.Unlock()

	// Send eviction error outside the lock.
	if evictSession != 0 {
		server.reply(evictSession, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
	}

	infoFrame := downstream.Frame{
		Command: byte(southboundenh.ENHReqInfo),
		Payload: []byte{infoID},
	}

	if err := server.writeUpstreamSerialized(infoFrame); err != nil {
		server.clearPendingInfo(sessionID)
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	// Re-check ownership after write. If another session evicted us during
	// WriteFrame, our upstream INFO request is orphaned — the response will
	// be correlated against the new pending. Notify ourselves with ErrorHost.
	server.pendingInfoMu.Lock()
	if server.pendingInfo == nil || server.pendingInfo.seq != mySeq {
		server.pendingInfoMu.Unlock()
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}
	server.pendingInfoMu.Unlock()
}

func (server *Server) handleSend(sessionID uint64, data byte) {
	server.mutex.Lock()
	owner := server.busOwner
	allowResponderSend, lateResponderSend, lateTarget, lateReason := server.evaluateTargetResponderSendLocked(sessionID)
	server.mutex.Unlock()

	if owner != sessionID && !allowResponderSend {
		if lateResponderSend {
			server.lateResponderReject.Add(1)
			log.Printf(
				"session=%d target_responder_late_reject target=0x%02X reason=%s",
				sessionID,
				lateTarget,
				lateReason,
			)
		}
		if server.cfg.Debug {
			log.Printf("session=%d send_rejected owner=%d symbol=0x%02X", sessionID, owner, data)
		}
		if winner, ok := server.takeSessionCollision(sessionID); ok {
			server.reply(sessionID, downstream.Frame{
				Command: byte(southboundenh.ENHResFailed),
				Payload: []byte{winner},
			})
			return
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return
	}

	queuedObserverFrames := false
	if owner == sessionID {
		queuedObserverFrames = server.queueOwnerObserverReplay(sessionID, data)
	}

	server.mutex.Lock()
	server.busDirty = true
	// busOwned is only set in setBusOwner — not reset per SEND byte.
	// This ensures maxOwnershipDuration and busIdleReleaseGrace measure
	// from initial ownership, preventing indefinite chaining.
	server.mutex.Unlock()

	sendFrame := downstream.Frame{
		Command: byte(southboundenh.ENHReqSend),
		Payload: []byte{data},
	}
	if server.cfg.Debug {
		log.Printf("session=%d send symbol=0x%02X", sessionID, data)
	}
	if err := server.writeUpstreamSerialized(sendFrame); err != nil {
		if queuedObserverFrames {
			server.rollbackOwnerObserverReplay(sessionID)
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		server.releaseBusIfOwner(sessionID)
		return
	}

	if allowResponderSend {
		if server.cfg.Debug {
			log.Printf("session=%d target_responder_send_accepted target=0x%02X symbol=0x%02X", sessionID, lateTarget, data)
		}
	}

}
