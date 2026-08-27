package adapterproxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	emutargets "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/emulation/targets"
	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/sourcepolicy"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func (server *Server) broadcast(frame downstream.Frame) {
	server.mutex.Lock()
	sessions := make([]*session, 0, len(server.sessions))
	for _, sess := range server.sessions {
		sessions = append(sessions, sess)
	}
	server.mutex.Unlock()

	for _, sess := range sessions {
		server.enqueueOrClose(sess, frame, "broadcast")
	}
}

func (server *Server) takeObserverReplayForReceived(symbol byte) ([]downstream.Frame, uint64, bool) {
	server.mutex.Lock()
	owner := server.busOwner
	suppress := false
	var frames []downstream.Frame
	if symbol == ebusSyn {
		if len(server.ownerObserverSeen) > 0 {
			frames = appendObserverRequestSegmentFrames(
				frames,
				server.busOwnerInitiator,
				server.ownerObserverSeen,
				server.ownerObserverAtStart,
			)
		}
		server.ownerObserverExpected = nil
		server.ownerObserverSeen = nil
		if owner != 0 {
			server.ownerObserverAtStart = true
		} else {
			server.ownerObserverAtStart = false
		}
		server.mutex.Unlock()
		if len(frames) == 0 {
			return nil, 0, false
		}
		return frames, server.pendingUDPPlainStartSessionID(), true
	}
	if len(server.ownerObserverExpected) > 0 {
		if symbol == server.ownerObserverExpected[0] {
			server.ownerObserverExpected = server.ownerObserverExpected[1:]
			server.ownerObserverSeen = append(server.ownerObserverSeen, symbol)
			suppress = true
			server.mutex.Unlock()
			return nil, 0, suppress
		}
		if len(server.ownerObserverSeen) > 0 {
			frames = appendRawObserverFrames(frames, server.ownerObserverSeen)
		}
		server.ownerObserverExpected = nil
		server.ownerObserverSeen = nil
		server.ownerObserverAtStart = false
	}
	if len(server.ownerObserverSeen) > 0 {
		frames = appendObserverRequestSegmentFrames(
			frames,
			server.busOwnerInitiator,
			server.ownerObserverSeen,
			server.ownerObserverAtStart,
		)
		server.ownerObserverSeen = nil
		server.ownerObserverAtStart = false
	}
	server.mutex.Unlock()

	if len(frames) == 0 {
		return nil, 0, suppress
	}
	return frames, server.pendingUDPPlainStartSessionID(), suppress
}

func (server *Server) broadcastObserverFrames(
	frames []downstream.Frame,
	skipPendingID uint64,
) {
	if len(frames) == 0 {
		return
	}

	server.mutex.Lock()
	ownerID := server.busOwner
	sessions := make([]*session, 0, len(server.sessions))
	for _, sess := range server.sessions {
		sessions = append(sessions, sess)
	}
	server.mutex.Unlock()

	for _, sess := range sessions {
		if sess.id == ownerID || sess.id == skipPendingID {
			continue
		}
		for _, frame := range frames {
			server.enqueueOrClose(sess, frame, "broadcast_observer_prefix")
		}
	}
}

func (server *Server) currentBusOwner() uint64 {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	return server.busOwner
}

// ownerAwaitingFirstSend returns (busOwner, true) if the bus is owned and
// the owner has not yet sent its first SEND byte. Used to detect the
// post-grant pre-first-SEND window where idle SYN must not be delivered
// to the owner (would be misread as echo and cause desync).
func (server *Server) ownerAwaitingFirstSend() (uint64, bool) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if server.busOwner == 0 || !server.awaitingFirstOwnerSend {
		return 0, false
	}
	return server.busOwner, true
}

func (server *Server) broadcastReceivedToOwner(frame downstream.Frame, ownerID uint64) {
	if ownerID == 0 {
		return
	}
	server.mutex.Lock()
	sess := server.sessions[ownerID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}
	server.enqueueOrClose(sess, frame, "broadcast_owner_only")
}

// broadcastExceptOwner delivers the frame to all sessions except the owner.
// Used to suppress idle SYN from the owner between grant and first SEND
// (PX-SYN-RACE) while still letting observer sessions see the bus activity.
func (server *Server) broadcastExceptOwner(frame downstream.Frame, ownerID uint64) {
	server.mutex.Lock()
	sessions := make([]*session, 0, len(server.sessions))
	for _, sess := range server.sessions {
		if sess.id == ownerID {
			continue
		}
		sessions = append(sessions, sess)
	}
	server.mutex.Unlock()

	for _, sess := range sessions {
		server.enqueueOrClose(sess, frame, "broadcast_except_owner")
	}
}

func (server *Server) broadcastUDPPlainByte(value byte) {
	if server.udpListener == nil {
		return
	}

	server.udpClientsMu.RLock()
	clients := make([]*udpClientEntry, 0, len(server.udpClients))
	for _, entry := range server.udpClients {
		clients = append(clients, entry)
	}
	server.udpClientsMu.RUnlock()

	for _, entry := range clients {
		if entry == nil || entry.addr == nil {
			continue
		}
		_, err := server.udpListener.WriteToUDP([]byte{value}, entry.addr)
		if err != nil {
			server.removeUDPPlainClient(entry.addr.String())
		}
	}
}

func (server *Server) removeUDPPlainClient(clientAddress string) {
	if strings.TrimSpace(clientAddress) == "" {
		return
	}
	server.udpClientsMu.Lock()
	delete(server.udpClients, clientAddress)
	server.udpClientsMu.Unlock()
}

func (server *Server) reply(sessionID uint64, frame downstream.Frame) {
	server.mutex.Lock()
	sess := server.sessions[sessionID]
	server.mutex.Unlock()
	if sess == nil {
		return
	}

	server.enqueueOrClose(sess, frame, "reply")
}

func (server *Server) enqueueOrClose(sess *session, frame downstream.Frame, reason string) {
	select {
	case <-sess.done:
		return
	default:
	}

	if sess.enqueue(frame) {
		return
	}

	dropped := server.backpressureDrops.Add(1)
	closed := server.backpressureCloses.Add(1)
	if server.cfg.Debug {
		log.Printf(
			"session=%d outbound_backpressure reason=%s dropped=%d closed=%d queue_len=%d queue_cap=%d",
			sess.id,
			reason,
			dropped,
			closed,
			len(sess.sendCh),
			cap(sess.sendCh),
		)
	}

	_ = sess.Close()
}

func (server *Server) deliverUpstreamError(frame downstream.Frame) {
	server.mutex.Lock()
	owner := server.busOwner
	server.mutex.Unlock()

	if owner != 0 {
		winner := byte(0x00)
		if len(frame.Payload) == 1 {
			winner = frame.Payload[0]
		}
		server.markSessionCollision(owner, winner)
		server.reply(owner, frame)
		server.releaseBusIfOwner(owner)
		return
	}

	server.broadcast(frame)
}

func (server *Server) deliverUpstreamFailed(frame downstream.Frame) {
	server.mutex.Lock()
	owner := server.busOwner
	server.mutex.Unlock()

	if owner != 0 {
		server.reply(owner, frame)
		server.releaseBusIfOwner(owner)
		return
	}

	server.broadcast(frame)
}

func (server *Server) releaseBusIfOwner(sessionID uint64) {
	var observerFrames []downstream.Frame
	var sessions []*session

	server.mutex.Lock()
	if server.busOwner != sessionID {
		server.mutex.Unlock()
		return
	}
	if len(server.ownerObserverSeen) > 0 {
		observerFrames = appendRawObserverFrames(nil, server.ownerObserverSeen)
		for _, sess := range server.sessions {
			if sess.id == sessionID {
				continue
			}
			sessions = append(sessions, sess)
		}
	}
	server.busOwner = 0
	server.busOwnerInitiator = 0
	server.awaitingFirstOwnerSend = false
	server.ownerObserverAtStart = false
	server.ownerObserverExpected = nil
	server.ownerObserverSeen = nil
	server.busDirty = false
	server.busOwned = time.Time{}
	server.targetResponderWindow = targetResponderWindow{}
	server.resetBusWirePhaseLocked(busWirePhaseIdle)
	server.mutex.Unlock()

	if len(observerFrames) > 0 {
		skipPendingID := server.pendingUDPPlainStartSessionID()
		for _, sess := range sessions {
			if sess.id == skipPendingID {
				continue
			}
			for _, frame := range observerFrames {
				server.enqueueOrClose(sess, frame, "broadcast_observer_release")
			}
		}
	}

	server.releaseBusToken()
}

func (server *Server) setBusOwner(sessionID uint64, initiator byte) {
	server.mutex.Lock()
	server.busOwner = sessionID
	server.busOwnerInitiator = initiator
	server.awaitingFirstOwnerSend = true // PX-SYN-RACE
	server.ownerObserverAtStart = true
	server.ownerObserverExpected = nil
	server.ownerObserverSeen = nil
	server.busDirty = true
	server.busOwned = time.Now().UTC()
	server.targetResponderWindow = targetResponderWindow{}
	server.resetBusWirePhaseLocked(busWirePhaseIdle)
	server.learnSessionInitiatorLocked(sessionID, initiator, "start")
	server.maybeAssociateTargetResponderLocked(sessionID, initiator)
	server.mutex.Unlock()
}

// PX69: Cap observer replay slices to prevent unbounded growth at ENH-TCP
// loopback speeds. maxEBUSTelegramLen is already defined for MTU enforcement.
const maxObserverReplayLen = maxEBUSTelegramLen * 2

func (server *Server) queueOwnerObserverReplay(sessionID uint64, data byte) bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if server.busOwner != sessionID {
		return false
	}
	// PX-SYN-RACE: Owner is sending → no longer in post-grant pre-SEND window.
	server.awaitingFirstOwnerSend = false
	if server.busWirePhase == busWirePhaseIdle {
		server.resetBusWirePhaseLocked(busWirePhaseCollectRequest)
	}
	// PX69: Cap growth to prevent unbounded accumulation.
	if len(server.ownerObserverExpected) >= maxObserverReplayLen {
		return false
	}
	server.ownerObserverExpected = append(server.ownerObserverExpected, data)
	return true
}

func (server *Server) rollbackOwnerObserverReplay(sessionID uint64) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if server.busOwner != sessionID || len(server.ownerObserverExpected) == 0 {
		return
	}
	server.ownerObserverExpected = server.ownerObserverExpected[:len(server.ownerObserverExpected)-1]
}

func appendObserverReplayFrames(
	dst []downstream.Frame,
	initiator byte,
	symbols []byte,
	includeInitiator bool,
) []downstream.Frame {
	if len(symbols) == 0 {
		return dst
	}
	if includeInitiator {
		dst = append(dst, downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{initiator},
		})
	}
	return appendRawObserverFrames(dst, symbols)
}

// PX19: The trailing SYN is intentional — it terminates the partial/truncated
// telegram segment so observers can resync their parser state. Without it,
// observers would interpret the next telegram's first byte as a continuation
// of the truncated frame.
func appendObserverRequestSegmentFrames(
	dst []downstream.Frame,
	initiator byte,
	symbols []byte,
	includeInitiator bool,
) []downstream.Frame {
	dst = appendObserverReplayFrames(dst, initiator, symbols, includeInitiator)
	return appendRawObserverFrames(dst, []byte{ebusSyn})
}

func appendRawObserverFrames(dst []downstream.Frame, symbols []byte) []downstream.Frame {
	for _, symbol := range symbols {
		dst = append(dst, downstream.Frame{
			Command: byte(southboundenh.ENHResReceived),
			Payload: []byte{symbol},
		})
	}
	return dst
}

func (server *Server) takeObserverReplayForAbort() ([]downstream.Frame, uint64) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if len(server.ownerObserverSeen) == 0 {
		server.ownerObserverExpected = nil
		server.ownerObserverSeen = nil
		server.ownerObserverAtStart = false
		return nil, 0
	}

	frames := appendRawObserverFrames(nil, server.ownerObserverSeen)
	server.ownerObserverExpected = nil
	server.ownerObserverSeen = nil
	server.ownerObserverAtStart = false
	return frames, server.pendingUDPPlainStartSessionID()
}

func (server *Server) pendingUDPPlainStartSessionID() uint64 {
	server.pendingStartMu.Lock()
	defer server.pendingStartMu.Unlock()
	if server.pendingStart != nil && server.pendingStart.mode == pendingStartModeUDPPlain {
		return server.pendingStart.sessionID
	}
	return 0
}

func (server *Server) releaseBusIfIdleSyn() {
	server.mutex.Lock()
	owner := server.busOwner
	dirty := server.busDirty
	ownedAt := server.busOwned
	if owner == 0 {
		server.mutex.Unlock()
		return
	}
	if !ownedAt.IsZero() && time.Since(ownedAt) < busIdleReleaseGrace {
		server.mutex.Unlock()
		return
	}
	if dirty {
		// First SYN after activity marks a telegram boundary. Keep ownership
		// until a subsequent idle SYN is observed.
		server.busDirty = false
		server.mutex.Unlock()
		return
	}
	server.mutex.Unlock()

	if server.cfg.Debug {
		log.Printf("session=%d release_reason=idle_syn", owner)
	}

	server.releaseBusIfOwner(owner)
}

func (server *Server) noteBusWireSymbol(symbol byte) {
	if symbol == ebusSyn {
		if server.releaseBusIfSynWhileWaiting() {
			return
		}
		// PX4: Reset wire phase tracking on SYN during CollectRequest so
		// subsequent bytes aren't misinterpreted as mid-request continuation.
		// Ownership release is handled by releaseBusIfIdleSyn's dirty/grace logic.
		server.mutex.Lock()
		if server.busOwner != 0 && server.busWirePhase == busWirePhaseCollectRequest && server.requestBytesSeen > 0 {
			server.resetBusWirePhaseLocked(busWirePhaseIdle)
		}
		server.mutex.Unlock()
		server.releaseBusIfIdleSyn()
		return
	}

	server.mutex.Lock()
	if server.busOwner != 0 {
		server.busDirty = true
		server.advanceBusWirePhaseLocked(symbol)
	}
	server.mutex.Unlock()
}

func (server *Server) releaseBusIfSynWhileWaiting() bool {
	server.mutex.Lock()
	owner := server.busOwner
	phase := server.busWirePhase
	if owner == 0 || !phase.isSynTimeoutBoundary() {
		server.mutex.Unlock()
		return false
	}
	server.resetBusWirePhaseLocked(busWirePhaseIdle)
	server.mutex.Unlock()

	if phase == busWirePhaseWaitCmdAck {
		server.synWaitCmdAckTO.Add(1)
	} else {
		server.synWaitResponseTO.Add(1)
	}
	log.Printf("session=%d syn_while_waiting_timeout phase=%s -> release_owner=true", owner, phase)
	server.releaseBusIfOwner(owner)
	return true
}

func (server *Server) resetBusWirePhaseLocked(phase busWirePhase) {
	server.busWirePhase = phase
	server.requestBytesSeen = 0
	server.requestDataLength = -1
	server.requestSrc = 0
	server.requestDst = 0
	server.requestPB = 0
	server.requestSB = 0
	server.requestLEN = 0
	server.requestHeaderCaptured = false
	server.responseBytesRemain = 0
	if phase == busWirePhaseIdle {
		server.targetResponderWindow = targetResponderWindow{}
	}
}

func (server *Server) advanceBusWirePhaseLocked(symbol byte) {
	switch server.busWirePhase {
	case busWirePhaseIdle:
		return
	case busWirePhaseCollectRequest:
		server.requestBytesSeen++
		switch server.requestBytesSeen {
		case 1:
			server.requestSrc = symbol
		case 2:
			server.requestDst = symbol
		case 3:
			server.requestPB = symbol
		case 4:
			server.requestSB = symbol
		case 5:
			server.requestLEN = symbol
			server.requestHeaderCaptured = true
			server.requestDataLength = int(symbol)
			return
		}
		if server.requestDataLength < 0 {
			return
		}
		if server.requestBytesSeen >= 6+server.requestDataLength {
			server.learnSessionInitiatorLocked(server.busOwner, server.requestSrc, "request")
			server.busWirePhase = busWirePhaseWaitCmdAck
			server.maybeOpenTargetResponderWindowLocked(server.requestDst)
		}
	case busWirePhaseWaitCmdAck:
		switch symbol {
		case ebusACK:
			// PX5: Broadcast (DST=0xFE) has no response phase.
			// PX2: Initiator-to-initiator (i2i) frames also have no response
			// phase — the ACK is from the destination initiator directly.
			if server.requestDst == 0xFE || isInitiatorAddress(server.requestDst) {
				server.resetBusWirePhaseLocked(busWirePhaseIdle)
			} else {
				server.busWirePhase = busWirePhaseWaitResponseLen
			}
		case ebusNACK:
			// PX3: NACK should trigger a single retry of the request, not
			// immediate idle. However, the retry is handled at the transport
			// layer by the initiator re-sending. We transition to idle to
			// allow the retransmission to be tracked as a fresh request.
			server.resetBusWirePhaseLocked(busWirePhaseIdle)
		}
	case busWirePhaseWaitResponseLen:
		// PX36: Response length = data bytes + CRC. LEN=0 means 0 data + 1 CRC = 1.
		server.responseBytesRemain = int(symbol) + 1
		server.busWirePhase = busWirePhaseWaitResponseBody
	case busWirePhaseWaitResponseBody:
		if server.responseBytesRemain > 0 {
			server.responseBytesRemain--
		}
		if server.responseBytesRemain <= 0 {
			server.busWirePhase = busWirePhaseWaitResponseAck
			server.targetResponderWindow = targetResponderWindow{}
		}
	case busWirePhaseWaitResponseAck:
		// PX2: Check for initiator-to-initiator (i2i) frames.
		// If DST is an initiator address, this is an i2i exchange where
		// the ACK is from the destination initiator, not a responder.
		// Any non-SYN symbol here is the response ACK/NACK.
		server.resetBusWirePhaseLocked(busWirePhaseIdle)
	}
}

func (server *Server) releaseBusToken() {
	select {
	case server.busToken <- struct{}{}:
	default:
	}
	server.mutex.Lock()
	server.maybeGrantStartArbLocked()
	server.mutex.Unlock()
}

func (server *Server) waitForStartArbitration(
	ctx context.Context,
	sess *session,
	sessionID uint64,
	initiator byte,
) bool {
	server.mutex.Lock()
	grantCh := server.registerStartArbContenderLocked(sessionID, initiator)
	server.mutex.Unlock()

	defer func() {
		server.mutex.Lock()
		server.unregisterStartArbContenderLocked(sessionID)
		server.mutex.Unlock()
	}()

	select {
	case <-grantCh:
	case <-ctx.Done():
		return false
	case <-sess.done:
		return false
	}

	select {
	case <-server.busToken:
		return true
	case <-ctx.Done():
		return false
	case <-sess.done:
		return false
	}
}

func (server *Server) registerStartArbContenderLocked(sessionID uint64, initiator byte) chan struct{} {
	if server.startArbContenders == nil {
		server.startArbContenders = make(map[uint64]*startArbContender)
	}
	server.startArbSeq++
	contender := &startArbContender{
		sessionID: sessionID,
		initiator: initiator,
		seq:       server.startArbSeq,
		grantCh:   make(chan struct{}),
	}
	server.startArbContenders[sessionID] = contender
	server.maybeGrantStartArbLocked()
	return contender.grantCh
}

func (server *Server) unregisterStartArbContenderLocked(sessionID uint64) {
	delete(server.startArbContenders, sessionID)
	if server.startArbGrantSession == sessionID {
		server.startArbGrantSession = 0
	}
	server.maybeGrantStartArbLocked()
}

func (server *Server) maybeGrantStartArbLocked() {
	if server.startArbGrantSession != 0 {
		return
	}
	if server.busOwner != 0 {
		return
	}
	if len(server.busToken) == 0 {
		return
	}
	winner := server.pickStartArbWinnerLocked()
	if winner == nil {
		return
	}
	server.startArbGrantSession = winner.sessionID
	close(winner.grantCh)
}

func (server *Server) pickStartArbWinnerLocked() *startArbContender {
	// PX29: Use FIFO ordering (seq) as primary, with initiator as tiebreaker.
	// This prevents starvation of higher-numbered initiators that would
	// otherwise never win against a continuously-contending lower initiator.
	var winner *startArbContender
	for _, contender := range server.startArbContenders {
		if winner == nil {
			winner = contender
			continue
		}
		if contender.seq < winner.seq {
			winner = contender
			continue
		}
		if contender.seq == winner.seq && contender.initiator < winner.initiator {
			winner = contender
		}
	}
	return winner
}

func (server *Server) acquireLease(sessionID uint64, initiator byte) (byte, error) {
	if server.leaseManager == nil {
		return initiator, nil
	}

	server.leasesMu.Lock()
	defer server.leasesMu.Unlock()

	if existing, ok := server.leasedBySess[sessionID]; ok {
		// PX10: Check expiry before returning existing lease.
		if existing.ExpiresAt.Before(time.Now()) {
			// Lease expired — release it and fall through to re-acquire.
			delete(server.leasedBySess, sessionID)
			_, _ = server.leaseManager.Release(existing.OwnerID)
		} else if existing.Address == initiator {
			// PX38: Renew lease on re-use to prevent 30min expiry.
			ownerID := fmt.Sprintf("session/%d", sessionID)
			if renewed, err := server.leaseManager.Renew(ownerID); err == nil {
				server.leasedBySess[sessionID] = renewed
			}
			return existing.Address, nil
		} else {
			return 0, fmt.Errorf(
				"session already leased initiator 0x%02X (requested 0x%02X)",
				existing.Address,
				initiator,
			)
		}
	}

	lease, err := server.leaseManager.Acquire(
		fmt.Sprintf("session/%d", sessionID),
		sourcepolicy.AcquireOptions{
			Candidates:        []uint8{initiator},
			AllowSoftReserved: true,
		},
	)
	if err != nil {
		return 0, err
	}

	server.leasedBySess[sessionID] = lease
	return lease.Address, nil
}

func (server *Server) releaseLease(sessionID uint64) {
	if server.leaseManager == nil {
		return
	}

	server.leasesMu.Lock()
	lease, ok := server.leasedBySess[sessionID]
	if ok {
		delete(server.leasedBySess, sessionID)
	}
	server.leasesMu.Unlock()

	if ok {
		_, _ = server.leaseManager.Release(lease.OwnerID)
	}
}

func (server *Server) sessionLeaseAddress(sessionID uint64) (byte, bool) {
	if server.leaseManager == nil {
		return 0, false
	}

	server.leasesMu.Lock()
	defer server.leasesMu.Unlock()

	lease, ok := server.leasedBySess[sessionID]
	if !ok {
		return 0, false
	}
	return lease.Address, true
}

func (server *Server) noteObservedInitiatorByte(byteValue byte) {
	server.observedMu.Lock()
	defer server.observedMu.Unlock()

	if byteValue == ebusSyn {
		server.startOfTelegram = true
		return
	}
	if !server.startOfTelegram {
		return
	}
	server.startOfTelegram = false
	if !isInitiatorAddress(byteValue) {
		return
	}

	now := time.Now().UTC()
	server.observedInitiatorAt[byteValue] = now
	server.pruneObservedInitiatorsLocked(now.Add(-server.cfg.AutoJoinActivityWindow))
}

func (server *Server) pruneObservedInitiatorsLocked(cutoff time.Time) {
	for address, seenAt := range server.observedInitiatorAt {
		if !seenAt.After(cutoff) {
			delete(server.observedInitiatorAt, address)
		}
	}
}

// PX24: Check if an initiator address was recently observed on the bus from
// an external (non-proxy-managed) device within the activity window.
func (server *Server) isObservedExternalInitiator(initiator byte) bool {
	if server.cfg.AutoJoinActivityWindow <= 0 {
		return false
	}
	server.observedMu.Lock()
	seenAt, found := server.observedInitiatorAt[initiator]
	server.observedMu.Unlock()
	if !found {
		return false
	}
	return time.Since(seenAt) <= server.cfg.AutoJoinActivityWindow
}

func (server *Server) selectAutoInitiator() (byte, error) {
	now := time.Now().UTC()
	observedSet := make(map[byte]struct{})

	server.observedMu.Lock()
	server.pruneObservedInitiatorsLocked(now.Add(-server.cfg.AutoJoinActivityWindow))
	for address := range server.observedInitiatorAt {
		observedSet[address] = struct{}{}
	}
	server.observedMu.Unlock()

	leasedSet := make(map[byte]struct{})
	server.leasesMu.Lock()
	for _, lease := range server.leasedBySess {
		leasedSet[lease.Address] = struct{}{}
	}
	server.leasesMu.Unlock()

	server.mutex.Lock()
	previous := server.autoJoinInitiator
	server.mutex.Unlock()
	if previous != 0 {
		if _, observed := observedSet[previous]; !observed {
			if _, leased := leasedSet[previous]; !leased {
				return previous, nil
			}
		}
	}

	// PX12: Also exclude companion target addresses of observed initiators.
	// If initiator 0x10 is observed, its companion target 0x15 should not
	// be selected as an initiator to avoid address space conflicts.
	companionExcluded := make(map[byte]struct{})
	for addr := range observedSet {
		if target, ok := companionTargetAddress(addr); ok {
			companionExcluded[target] = struct{}{}
		}
	}

	for _, candidate := range preferredInitiatorAddresses {
		if _, observed := observedSet[candidate]; observed {
			continue
		}
		if _, leased := leasedSet[candidate]; leased {
			continue
		}
		if _, excluded := companionExcluded[candidate]; excluded {
			continue
		}
		return candidate, nil
	}
	return 0, fmt.Errorf("no initiator address available for auto join")
}

func (server *Server) markSessionCollision(sessionID uint64, winner byte) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.collisionBySession[sessionID] = winner
}

func (server *Server) clearSessionCollision(sessionID uint64) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	delete(server.collisionBySession, sessionID)
}

func (server *Server) takeSessionCollision(sessionID uint64) (byte, bool) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	winner, ok := server.collisionBySession[sessionID]
	if !ok {
		return 0, false
	}
	delete(server.collisionBySession, sessionID)
	return winner, true
}

func (server *Server) learnSessionInitiator(sessionID uint64, initiator byte, source string) {
	server.mutex.Lock()
	server.learnSessionInitiatorLocked(sessionID, initiator, source)
	server.mutex.Unlock()
}

func (server *Server) learnSessionInitiatorLocked(sessionID uint64, initiator byte, source string) {
	if sessionID == 0 || sessionID == udpBridgeOwnerID {
		return
	}
	if !isInitiatorAddress(initiator) {
		return
	}
	if _, ok := server.sessions[sessionID]; !ok {
		return
	}
	if source == "" {
		source = "unknown"
	}

	server.learnedBySession[sessionID] = sessionInitiatorLearning{
		Initiator: initiator,
		LearnedAt: time.Now().UTC(),
		Source:    source,
	}
}

func (server *Server) sessionLearnedInitiator(sessionID uint64) (byte, bool) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	learning, ok := server.learnedBySession[sessionID]
	if !ok {
		return 0, false
	}
	return learning.Initiator, true
}

func (server *Server) registerLocalTargetResponder(targetAddress byte, sessionID uint64) {
	server.mutex.Lock()
	if server.localRespondersByTarget == nil {
		server.localRespondersByTarget = make(map[byte]targetResponderAssociation)
	}
	server.localRespondersByTarget[targetAddress] = targetResponderAssociation{
		targetAddress: targetAddress,
		sessionID:     sessionID,
		mode:          targetResponderModeLocal,
	}
	server.mutex.Unlock()
}

func (server *Server) registerExperimentalChildTargetResponder(targetAddress byte, sessionID uint64) {
	server.mutex.Lock()
	if server.localRespondersByTarget == nil {
		server.localRespondersByTarget = make(map[byte]targetResponderAssociation)
	}
	server.localRespondersByTarget[targetAddress] = targetResponderAssociation{
		targetAddress: targetAddress,
		sessionID:     sessionID,
		mode:          targetResponderModeChildExperimental,
	}
	server.mutex.Unlock()
}

func (server *Server) clearLocalResponderAssociationsForSessionLocked(sessionID uint64) {
	for targetAddress, association := range server.localRespondersByTarget {
		if association.sessionID == sessionID {
			delete(server.localRespondersByTarget, targetAddress)
		}
	}
}

func (server *Server) maybeOpenTargetResponderWindowLocked(targetAddress byte) {
	association, ok := server.localRespondersByTarget[targetAddress]
	if !ok {
		return
	}
	if association.mode == targetResponderModeChildExperimental && !server.cfg.EnableExperimentalChildTargetResponder {
		return
	}
	if association.sessionID == 0 || association.sessionID == server.busOwner {
		return
	}
	if _, ok := server.sessions[association.sessionID]; !ok {
		return
	}

	server.targetResponderWindow = targetResponderWindow{
		open:               true,
		targetAddress:      targetAddress,
		ownerSessionID:     server.busOwner,
		responderSessionID: association.sessionID,
		mode:               association.mode,
		openedAt:           time.Now().UTC(),
	}

	if server.cfg.Debug {
		log.Printf(
			"session=%d target_responder_window_open target=0x%02X responder=%d mode=%s",
			server.busOwner,
			targetAddress,
			association.sessionID,
			association.mode,
		)
	}
}

func (server *Server) maybeAssociateTargetResponderLocked(sessionID uint64, initiator byte) {
	if sessionID == 0 || sessionID == udpBridgeOwnerID {
		return
	}
	if _, ok := server.sessions[sessionID]; !ok {
		return
	}

	if targetAddress, ok := builtInLocalTargetForInitiator(initiator); ok {
		server.localRespondersByTarget[targetAddress] = targetResponderAssociation{
			targetAddress: targetAddress,
			sessionID:     sessionID,
			mode:          targetResponderModeLocal,
		}
		return
	}

	targetAddress, ok := companionTargetAddress(initiator)
	if !ok {
		return
	}
	server.localRespondersByTarget[targetAddress] = targetResponderAssociation{
		targetAddress: targetAddress,
		sessionID:     sessionID,
		mode:          targetResponderModeChildExperimental,
	}
}

func (server *Server) targetResponderWindowAllowsPhaseLocked() bool {
	switch server.busWirePhase {
	case busWirePhaseWaitCmdAck, busWirePhaseWaitResponseLen, busWirePhaseWaitResponseBody:
		return true
	default:
		return false
	}
}

func (server *Server) lookupTargetResponderBySessionLocked(sessionID uint64) (targetResponderAssociation, bool) {
	for _, association := range server.localRespondersByTarget {
		if association.sessionID == sessionID {
			return association, true
		}
	}
	return targetResponderAssociation{}, false
}

func (server *Server) evaluateTargetResponderSendLocked(sessionID uint64) (allow bool, late bool, targetAddress byte, reason string) {
	if sessionID == 0 {
		return false, false, 0, ""
	}
	if server.targetResponderWindow.open &&
		server.targetResponderWindow.responderSessionID == sessionID &&
		server.targetResponderWindow.ownerSessionID == server.busOwner &&
		server.targetResponderWindowAllowsPhaseLocked() {
		return true, false, server.targetResponderWindow.targetAddress, ""
	}

	association, ok := server.lookupTargetResponderBySessionLocked(sessionID)
	if !ok {
		return false, false, 0, ""
	}
	if association.mode == targetResponderModeChildExperimental && !server.cfg.EnableExperimentalChildTargetResponder {
		return false, true, association.targetAddress, "experimental_child_disabled"
	}
	if !server.targetResponderWindow.open {
		return false, true, association.targetAddress, "window_not_open"
	}
	if server.targetResponderWindow.responderSessionID != sessionID {
		return false, true, association.targetAddress, "not_assigned_for_active_target"
	}
	if server.targetResponderWindow.ownerSessionID != server.busOwner {
		return false, true, association.targetAddress, "owner_mismatch"
	}
	if !server.targetResponderWindowAllowsPhaseLocked() {
		return false, true, association.targetAddress, "outside_responder_phase"
	}

	return false, true, association.targetAddress, "unknown"
}

func builtInLocalTargetForInitiator(initiator byte) (byte, bool) {
	// Built-in local emulation profile support starts with VR90-like pairings.
	// For this issue, we keep a single deterministic pairing.
	if initiator == 0x10 {
		return emutargets.BuiltInProfileVR90TargetAddress, true
	}
	return 0, false
}

func companionTargetAddress(initiator byte) (byte, bool) {
	if !isInitiatorAddress(initiator) {
		return 0, false
	}
	target := uint16(initiator) + 0x05
	if target > 0xFE {
		return 0, false
	}
	if target == 0x00 || target == 0xFF {
		return 0, false
	}
	return byte(target), true
}

func isInitiatorAddress(address byte) bool {
	// PX18/PX25: Exclude protocol-reserved bytes (ACK=0x00, NACK=0xFF, ESC=0xA9, SYN=0xAA)
	// from the initiator set. These can appear on the bus but are never valid initiator addresses.
	switch address {
	case 0x00, 0xFF, 0xA9, 0xAA:
		return false
	}
	return initiatorPart(address&0x0F) > 0 && initiatorPart((address&0xF0)>>4) > 0
}

func initiatorPart(bits byte) byte {
	switch bits {
	case 0x0:
		return 1
	case 0x1:
		return 2
	case 0x3:
		return 3
	case 0x7:
		return 4
	case 0xF:
		return 5
	default:
		return 0
	}
}

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, net.ErrClosed)
}
