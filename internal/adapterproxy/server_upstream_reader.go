package adapterproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"time"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func (server *Server) runUDPPlainReader(ctx context.Context) {
	defer server.waitGroup.Done()

	if server.udpListener == nil {
		return
	}

	buffer := make([]byte, udpPlainReadBufferSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := setReadDeadline(server.udpListener, server.cfg.ReadTimeout); err != nil {
			continue
		}

		n, remoteAddr, err := server.udpListener.ReadFromUDP(buffer)
		if err != nil {
			if isTimeoutError(err) {
				continue
			}
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			continue
		}
		if n == 0 || remoteAddr == nil {
			continue
		}

		if !server.registerUDPPlainClient(remoteAddr) {
			// Client not admitted (cap reached) — drop datagram.
			continue
		}
		payload := append([]byte(nil), buffer[:n]...)
		if server.cfg.Debug {
			log.Printf(
				"udp_plain_rx client=%s len=%d first=0x%02X",
				remoteAddr.String(),
				len(payload),
				payload[0],
			)
		}

		select {
		case server.udpQueue <- udpDatagram{payload: payload}:
		default:
			if server.cfg.Debug {
				log.Printf("udp_plain_queue_full dropped_datagram_len=%d", len(payload))
			}
		}
	}
}

func (server *Server) runUDPPlainWriter(ctx context.Context) {
	defer server.waitGroup.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-server.upstreamLost:
			return
		case datagram := <-server.udpQueue:
			if len(datagram.payload) == 0 {
				continue
			}
			if err := server.forwardUDPPlainDatagram(ctx, datagram.payload); err != nil {
				if server.cfg.Debug {
					log.Printf("udp_plain_forward_failed len=%d err=%v", len(datagram.payload), err)
				}
			}
		}
	}
}

// maxEBUSTelegramLen is the maximum eBUS telegram length:
// SRC(1) + DST(1) + PB(1) + SB(1) + LEN(1) + DATA(16) + CRC(1) + ACK(1) + RESP_LEN(1) + RESP_DATA(16) + RESP_CRC(1) + RESP_ACK(1) = 42
const maxEBUSTelegramLen = 42

func (server *Server) forwardUDPPlainDatagram(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	// PX9: No payload-level byte filtering needed. ENH encoding wraps each
	// logical byte in a 2-byte pair, so 0xAA (SYN) and 0xA9 (ESC) in the
	// payload are encoded by the ENH encoder and never appear as raw control
	// bytes on the wire. On wire-plain upstream, all bytes are physical bus
	// symbols and pass through unmodified (including SYN for bus sync).
	// PX28/PX46: Enforce maximum telegram length to prevent adapter FIFO
	// truncation and unbounded forwarding under a single bus token.
	if len(payload) > maxEBUSTelegramLen {
		return fmt.Errorf("udp datagram exceeds max eBUS telegram length (%d > %d)", len(payload), maxEBUSTelegramLen)
	}
	if server.cfg.Debug {
		log.Printf(
			"udp_plain_forward begin len=%d first=0x%02X wire_plain_upstream=%t",
			len(payload),
			payload[0],
			server.isWirePlainUpstream(),
		)
	}

	if !server.isWirePlainUpstream() {
		initiator := payload[0]
		// P2: Validate initiator address before UDP-to-ENH bridge START.
		if !isInitiatorAddress(initiator) {
			return fmt.Errorf("udp bridge: invalid initiator 0x%02X", initiator)
		}
		// R3: Reject initiator observed on the bus from an external device,
		// same guard that handleStart uses for TCP sessions (see ~line 758).
		if server.isObservedExternalInitiator(initiator) {
			return fmt.Errorf("udp bridge: initiator 0x%02X observed externally", initiator)
		}
	}

	if err := server.waitForUDPBridgeArbitration(ctx, payload[0]); err != nil {
		return err
	}

	releaseToken := true
	defer func() {
		if releaseToken {
			server.releaseBusToken()
		}
	}()

	if !server.isWirePlainUpstream() {
		initiator := payload[0]
		if err := server.startUDPPlainBridge(ctx, initiator); err != nil {
			return err
		}
		server.setBusOwner(udpBridgeOwnerID, initiator)
		releaseToken = false
		payload = payload[1:]
		if len(payload) == 0 {
			// R5: Payload contained only the initiator byte — the bridge
			// was started but there is nothing to send. Release ownership
			// and bus token to avoid leaking them.
			server.releaseBusIfOwner(udpBridgeOwnerID)
			return nil
		}
	}

	for _, symbol := range payload {
		if err := server.writeUpstreamSerialized(downstream.Frame{
			Command: byte(southboundenh.ENHReqSend),
			Payload: []byte{symbol},
		}); err != nil {
			if !releaseToken {
				// PX63: releaseBusIfOwner already calls releaseBusToken
				// internally, so set releaseToken=false to prevent double-release
				// from the deferred cleanup.
				server.releaseBusIfOwner(udpBridgeOwnerID)
				// Do NOT set releaseToken=true — releaseBusIfOwner already did it.
			}
			return err
		}
	}
	if server.cfg.Debug {
		log.Printf("udp_plain_forward done len=%d", len(payload))
	}
	return nil
}

func (server *Server) waitForUDPBridgeArbitration(ctx context.Context, arbitrationSymbol byte) error {
	server.mutex.Lock()
	grantCh := server.registerStartArbContenderLocked(udpBridgeOwnerID, arbitrationSymbol)
	server.mutex.Unlock()

	defer func() {
		server.mutex.Lock()
		server.unregisterStartArbContenderLocked(udpBridgeOwnerID)
		server.mutex.Unlock()
	}()

	select {
	case <-grantCh:
	case <-ctx.Done():
		return ctx.Err()
	case <-server.upstreamLost:
		return ErrUpstreamLost
	}

	select {
	case <-server.busToken:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-server.upstreamLost:
		return ErrUpstreamLost
	}
}

const maxUDPClients = 64 // hard cap to prevent unbounded map growth

// registerUDPPlainClient registers or refreshes a UDP client. Returns true
// if the client is admitted (existing or newly registered), false if rejected
// (cap reached). Rejected clients' datagrams should be dropped.
func (server *Server) registerUDPPlainClient(remoteAddr *net.UDPAddr) bool {
	if remoteAddr == nil {
		return false
	}
	clientAddress := remoteAddr.String()
	now := time.Now()

	server.udpClientsMu.Lock()
	// CR-P2b: Refresh existing clients BEFORE cap check.
	if existing, ok := server.udpClients[clientAddress]; ok {
		existing.lastSeen = now
		server.udpClientsMu.Unlock()
		return true
	}
	// PX13: Evict stale entries before checking cap.
	for key, entry := range server.udpClients {
		if now.Sub(entry.lastSeen) > udpClientTTL {
			delete(server.udpClients, key)
		}
	}
	// Cap UDP clients. Unadmitted clients' datagrams are dropped.
	if len(server.udpClients) >= maxUDPClients {
		server.udpClientsMu.Unlock()
		return false
	}
	server.udpClients[clientAddress] = &udpClientEntry{addr: remoteAddr, lastSeen: now}
	server.udpClientsMu.Unlock()
	return true
}

func (server *Server) startUDPPlainBridge(ctx context.Context, initiator byte) error {
	respCh := make(chan downstream.Frame, 1)
	startMode := pendingStartModeENH
	if server.isWirePlainUpstream() {
		startMode = pendingStartModeUDPPlain
	}

	server.pendingStartMu.Lock()
	if server.pendingStart != nil {
		server.pendingStartMu.Unlock()
		return fmt.Errorf("upstream start already pending")
	}
	server.pendingStart = &pendingStart{
		sessionID: 0,

		respCh:    respCh,
		mode:      startMode,
		initiator: initiator,
	}
	server.pendingStartMu.Unlock()

	if err := server.writeUpstreamSerialized(downstream.Frame{
		Command: byte(southboundenh.ENHReqStart),
		Payload: []byte{initiator},
	}); err != nil {
		server.clearPendingStart(0)
		return err
	}

	select {
	case response := <-respCh:
		command := southboundenh.ENHCommand(response.Command)
		switch command {
		case southboundenh.ENHResStarted:
			return nil
		case southboundenh.ENHResFailed:
			if len(response.Payload) == 1 {
				return fmt.Errorf("upstream start failed (winner=0x%02X)", response.Payload[0])
			}
			return fmt.Errorf("upstream start failed")
		default:
			return fmt.Errorf("upstream start unexpected response 0x%02X", response.Command)
		}
	case <-server.upstreamLost:
		server.clearPendingStart(0)
		return ErrUpstreamLost
	case <-ctx.Done():
		server.clearPendingStart(0)
		return ctx.Err()
	case <-time.After(server.cfg.UDPPlainStartWait):
		server.clearPendingStart(0)
		if !server.cfg.DisableUDPPlainStartFallback {
			// PX43: Unconditional log — unconfirmed bus ownership is an
			// exceptional condition that operators must see in production.
			log.Printf("udp_plain_bridge_start_timeout_fallback=true initiator=0x%02X ownership=unconfirmed", initiator)
			return nil
		}
		return fmt.Errorf("upstream start timeout")
	}
}

func (server *Server) runUpstreamReader(ctx context.Context) {
	defer server.waitGroup.Done()

	// R2/CR-BLACKHOLE: Detect blackholed adapter by wall-clock silence, not
	// by consecutive read timeouts. A quiet bus with short read deadlines
	// (e.g. 200ms) generates many legitimate timeouts that are NOT upstream
	// loss. Only trip upstreamLost if no frame has been received for an
	// extended period AND we have evidence the adapter was alive earlier
	// (lastWireRXAtNano > 0).
	const blackholeSilenceThreshold = 5 * time.Minute
	blackholeLogged := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		frame, err := server.upstream.ReadFrame()
		if err != nil {
			if isTimeoutError(err) {
				// Check wall-clock silence since last received byte.
				lastRX := server.lastWireRXAtNano.Load()
				if lastRX > 0 && time.Since(time.Unix(0, lastRX)) > blackholeSilenceThreshold {
					if !blackholeLogged {
						log.Printf("upstream_blackhole_detected silence=%s", time.Since(time.Unix(0, lastRX)))
						blackholeLogged = true
					}
					server.upstreamLostOnce.Do(func() {
						if server.upstreamLost != nil {
							close(server.upstreamLost)
						}
					})
					return
				}
				continue
			}
			if errors.Is(err, io.EOF) || isClosedNetworkError(err) {
				log.Printf("upstream_connection_lost error=%q", err)
				observerFrames, skipPendingID := server.takeObserverReplayForAbort()
				if len(observerFrames) > 0 {
					server.broadcastObserverFrames(observerFrames, skipPendingID)
				}
				// CR-P1: Close-based broadcast so all goroutines are notified.
				server.upstreamLostOnce.Do(func() {
					if server.upstreamLost != nil {
						close(server.upstreamLost)
					}
				})
				return
			}
			continue
		}

		// R2/CR-BLACKHOLE: Reset blackhole log flag on any successful read.
		blackholeLogged = false

		switch southboundenh.ENHCommand(frame.Command) {
		case southboundenh.ENHResResetted:
			features := byte(0x00)
			if len(frame.Payload) == 1 {
				features = frame.Payload[0]
				server.upstreamFeatures.Store(uint32(features))
			}
			server.infoCache.invalidateAll()
			log.Printf("upstream_resetted features=0x%02X", features)

			// Abort pending START — adapter reset means arbitration is void.
			// Use ErrorHost (not FAILED) to avoid false collision marking in handleStart.
			server.pendingStartMu.Lock()
			if ps := server.pendingStart; ps != nil {
				server.nilPendingStartLocked()
				server.pendingStartMu.Unlock()
				log.Printf("session=%d resetted_abort_pending_start initiator=0x%02X", ps.sessionID, ps.initiator)
				abortFrame := downstream.Frame{
					Command: byte(southboundenh.ENHResErrorHost),
					Payload: []byte{0x00},
				}
				select {
				case ps.respCh <- cloneFrame(abortFrame):
				default:
				}
				server.reply(ps.sessionID, abortFrame)
			} else {
				server.pendingStartMu.Unlock()
			}

			// Abort pending INFO — adapter reset invalidates in-flight info.
			server.pendingInfoMu.Lock()
			if pi := server.pendingInfo; pi != nil {
				server.pendingInfo = nil
				server.pendingInfoMu.Unlock()
				log.Printf("session=%d resetted_abort_pending_info infoID=0x%02X", pi.sessionID, pi.infoID)
				server.reply(pi.sessionID, downstream.Frame{
					Command: byte(southboundenh.ENHResErrorHost),
					Payload: []byte{0x00},
				})
			} else {
				server.pendingInfoMu.Unlock()
			}

			// Release bus if owned — adapter reset invalidates bus ownership.
			if owner := server.currentBusOwner(); owner != 0 {
				log.Printf("session=%d resetted_release_bus_owner", owner)
				server.releaseBusIfOwner(owner)
			}

			// Re-INIT upstream — adapter needs fresh handshake after reset.
			// Skip if this RESETTED is itself a response to our recent INIT
			// (avoids INIT→RESETTED→INIT feedback loop). The timestamp auto-
			// expires so adapters that ignore INIT don't block future recovery.
			if sentAt := server.initSentAtNano.Swap(0); sentAt > 0 && time.Since(time.Unix(0, sentAt)) < initResponseWindow {
				log.Printf("resetted_is_init_response reinit_skipped=true age=%s", time.Since(time.Unix(0, sentAt)))
			} else {
				select {
				case server.reinitGuard <- struct{}{}:
					// PX20/PX62: Track reinitGuard goroutine in waitGroup so it
					// cannot call SendInit on a closed upstream after Serve returns.
					server.waitGroup.Add(1)
					go func() {
						defer server.waitGroup.Done()
						defer func() { <-server.reinitGuard }()
						// Stabilization delay: give the adapter's eBUS
						// transceiver time to re-initialize before we
						// send the INIT handshake.
						time.Sleep(resettedStabilizationDelay)
						// Store timestamp BEFORE SendInit so a fast
						// RESETTED response is correctly classified as
						// an INIT response, not a spontaneous reset.
						server.initSentAtNano.Store(time.Now().UnixNano())
						if err := server.upstream.SendInit(0x01); err != nil {
							server.initSentAtNano.Store(0) // clear stale marker on failure
							log.Printf("resetted_reinit_failed error=%q", err)
							return
						}
					}()
				default:
					log.Printf("resetted_reinit_skipped already_in_flight=true")
				}
			}

			server.broadcast(frame)
		case southboundenh.ENHResReceived:
			if len(frame.Payload) == 1 {
				server.lastWireRXAtNano.Store(time.Now().UTC().UnixNano())
				if server.cfg.Debug {
					log.Printf("wire_rx symbol=0x%02X", frame.Payload[0])
				}
				server.logWireRX(frame.Payload[0])
				server.broadcastUDPPlainByte(frame.Payload[0])
				server.noteObservedInitiatorByte(frame.Payload[0])
				server.noteBusWireSymbol(frame.Payload[0])
				if frame.Payload[0] == ebusSyn {
					select {
					case server.synCh <- struct{}{}:
					default:
					}
				}

				if server.shouldUseWireArbitrationResult() && server.isStartPending() && server.deliverPendingStartFromArbByte(frame.Payload[0]) {
					continue
				}

				observerFrames, skipPendingID, suppressObservers := server.takeObserverReplayForReceived(frame.Payload[0])
				if len(observerFrames) > 0 {
					server.broadcastObserverFrames(observerFrames, skipPendingID)
				}
				if suppressObservers {
					server.broadcastReceivedToOwner(frame, server.currentBusOwner())
					continue
				}

				// PX-SYN-RACE: Suppress idle SYN delivered to owner between
				// grant (setBusOwner) and first owner SEND. The owner's ENH
				// client expects to see the echo of its own first SEND byte;
				// delivering an idle SYN causes it to read SYN instead of
				// the echo and desync. awaitingFirstOwnerSend is true only
				// in this specific window. Observer sessions still see the
				// SYN via broadcastExceptOwner.
				if frame.Payload[0] == ebusSyn {
					if owner, waiting := server.ownerAwaitingFirstSend(); waiting {
						server.broadcastExceptOwner(frame, owner)
						continue
					}
				}
			}
			server.broadcast(frame)
		case southboundenh.ENHResInfo:
			if server.deliverPendingInfo(frame) {
				continue
			}
		case southboundenh.ENHResErrorEBUS, southboundenh.ENHResErrorHost:
			if server.deliverPendingStart(frame) {
				continue
			}
			server.deliverUpstreamError(frame)
		case southboundenh.ENHResStarted, southboundenh.ENHResFailed:
			if server.deliverPendingStart(frame) {
				continue
			}
			if southboundenh.ENHCommand(frame.Command) == southboundenh.ENHResFailed {
				server.deliverUpstreamFailed(frame)
			}
		default:
		}
	}
}

func (server *Server) waitForUDPPlainIdleSyn(
	ctx context.Context,
	sess *session,
	sessionID uint64,
	attempt int,
) (bool, bool) {
	waitTimeout := udpPlainSynWait
	bootstrapMode := !server.hasRecentWireRX(udpPlainSynWait)
	if bootstrapMode {
		waitTimeout = udpPlainBootstrapWait
	}

	synWait := time.Now()
	select {
	case <-server.synCh:
		if server.cfg.Debug {
			log.Printf("session=%d attempt=%d syn_wait=%s", sessionID, attempt, time.Since(synWait))
		}
		return true, true
	case <-time.After(waitTimeout):
		if bootstrapMode {
			if server.cfg.Debug {
				log.Printf(
					"session=%d attempt=%d syn_wait_bootstrap_timeout=%s proceeding_without_syn=true",
					sessionID,
					attempt,
					waitTimeout,
				)
			}
			return false, true
		}
		server.reply(sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return true, false
	case <-ctx.Done():
		return false, false
	case <-sess.done:
		return false, false
	}
}

func (server *Server) hasRecentWireRX(window time.Duration) bool {
	if window <= 0 {
		return false
	}

	lastSeen := server.lastWireRXAtNano.Load()
	if lastSeen <= 0 {
		return false
	}

	return time.Since(time.Unix(0, lastSeen)) <= window
}

func (server *Server) deliverPendingStart(frame downstream.Frame) bool {
	server.pendingStartMu.Lock()
	pending := server.pendingStart
	if pending == nil {
		server.pendingStartMu.Unlock()
		return false
	}

	// PX11: The stale-response heuristic has been removed. The ENH protocol
	// does not carry per-request tokens, so a time-based guard cannot
	// distinguish stale from legitimate fast responses and risks eating
	// valid results (GH-P1). The existing stale-absorb logic (below) handles
	// the case where the adapter responds with a different initiator.

	frameData := byte(0x00)
	if len(frame.Payload) > 0 {
		frameData = frame.Payload[0]
	}
	command := southboundenh.ENHCommand(frame.Command)
	now := time.Now()

	if pending.mode == pendingStartModeENH &&
		command == southboundenh.ENHResStarted &&
		frameData != pending.initiator {
		if !pending.staleObserved {
			pending.staleObserved = true
			pending.staleWinner = frameData
			pending.staleDeadline = now.Add(startStaleAbsorbWindow)
			server.pendingStartMu.Unlock()
			log.Printf(
				"session=%d start_stale_absorb_wait requested=0x%02X adapter_won=0x%02X window=%s",
				pending.sessionID,
				pending.initiator,
				frameData,
				startStaleAbsorbWindow,
			)
			server.schedulePendingStartStaleExpiry(pending)
			return true
		}

		pending.staleWinner = frameData
		if now.Before(pending.staleDeadline) {
			remaining := time.Until(pending.staleDeadline)
			server.pendingStartMu.Unlock()
			log.Printf(
				"session=%d start_stale_absorb_wait requested=0x%02X adapter_won=0x%02X remaining=%s",
				pending.sessionID,
				pending.initiator,
				frameData,
				remaining,
			)
			return true
		}

		server.nilPendingStartLocked()
		server.pendingStartMu.Unlock()
		log.Printf(
			"session=%d start_stale_absorb_expired requested=0x%02X adapter_won=0x%02X window=%s -> converting STARTED to FAILED",
			pending.sessionID,
			pending.initiator,
			frameData,
			startStaleAbsorbWindow,
		)
		server.staleStartExpired.Add(1)

		forwarded := downstream.Frame{
			Command: byte(southboundenh.ENHResFailed),
			Payload: []byte{frameData},
		}
		select {
		case pending.respCh <- cloneFrame(forwarded):
		default:
		}
		server.reply(pending.sessionID, forwarded)
		return true
	}

	// Clear pending once a terminal START result frame is consumed so that
	// subsequent wire bytes are not dropped by the "start pending" fast-path.
	server.nilPendingStartLocked()
	hadStaleAbsorb := pending.mode == pendingStartModeENH &&
		pending.staleObserved &&
		command == southboundenh.ENHResStarted &&
		frameData == pending.initiator
	staleWinner := pending.staleWinner
	server.pendingStartMu.Unlock()

	log.Printf(
		"session=%d upstream_start_result cmd=0x%02X data=0x%02X initiator=0x%02X",
		pending.sessionID,
		frame.Command,
		frameData,
		pending.initiator,
	)

	if hadStaleAbsorb {
		server.staleStartAbsorbed.Add(1)
		log.Printf(
			"session=%d start_stale_absorbed requested=0x%02X adapter_won=0x%02X",
			pending.sessionID,
			pending.initiator,
			staleWinner,
		)
	}

	forwarded := cloneFrame(frame)
	select {
	case pending.respCh <- cloneFrame(forwarded):
	default:
	}

	if pending.mode == pendingStartModeUDPPlain {
		return true
	}

	// Preserve upstream ordering for ENH upstream mode: enqueue the response
	// immediately on the owning session. The waiting START handler still uses
	// respCh for internal ownership state updates.
	server.reply(pending.sessionID, forwarded)

	return true
}

func (server *Server) schedulePendingStartStaleExpiry(pending *pendingStart) {
	time.AfterFunc(startStaleAbsorbWindow, func() {
		server.expirePendingStartStale(pending)
	})
}

func (server *Server) expirePendingStartStale(expected *pendingStart) {
	server.pendingStartMu.Lock()
	pending := server.pendingStart
	if pending == nil || pending != expected || !pending.staleObserved {
		server.pendingStartMu.Unlock()
		return
	}
	if time.Now().Before(pending.staleDeadline) {
		server.pendingStartMu.Unlock()
		return
	}

	server.nilPendingStartLocked()
	winner := pending.staleWinner
	server.pendingStartMu.Unlock()

	log.Printf(
		"session=%d start_stale_absorb_expired requested=0x%02X adapter_won=0x%02X window=%s -> converting STARTED to FAILED",
		pending.sessionID,
		pending.initiator,
		winner,
		startStaleAbsorbWindow,
	)
	server.staleStartExpired.Add(1)

	failed := downstream.Frame{
		Command: byte(southboundenh.ENHResFailed),
		Payload: []byte{winner},
	}
	select {
	case pending.respCh <- cloneFrame(failed):
	default:
	}

	if pending.mode == pendingStartModeUDPPlain {
		return
	}

	server.reply(pending.sessionID, failed)
}

func (server *Server) isStartPending() bool {
	server.pendingStartMu.Lock()
	defer server.pendingStartMu.Unlock()
	return server.pendingStart != nil
}

func (server *Server) shouldUseWireArbitrationResult() bool {
	return server.isWirePlainUpstream()
}

func (server *Server) isWirePlainUpstream() bool {
	switch server.cfg.UpstreamTransport {
	case UpstreamUDPPlain, UpstreamTCPPlain:
		return true
	default:
		return false
	}
}

func (server *Server) clearSynSignal() {
	for {
		select {
		case <-server.synCh:
		default:
			return
		}
	}
}

func udpPlainRetryBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// PX61: Clamp shift to prevent overflow — at attempt >= 40 the shift
	// wraps time.Duration negative, causing time.After to fire immediately.
	if attempt > 30 {
		return udpPlainBackoffMax
	}
	delay := udpPlainBackoffBase << attempt
	if delay > udpPlainBackoffMax || delay <= 0 {
		return udpPlainBackoffMax
	}
	return delay
}

func udpPlainRetryBackoffWithJitter(
	attempt int,
	jitterFactor float64,
	randomFloat64 func() float64,
) time.Duration {
	backoff := udpPlainRetryBackoff(attempt)
	if jitterFactor <= 0 {
		return backoff
	}
	if jitterFactor > 1 {
		jitterFactor = 1
	}
	if randomFloat64 == nil {
		randomFloat64 = rand.Float64
	}

	// Uniform jitter in [-jitterFactor, +jitterFactor].
	jitter := (randomFloat64()*2 - 1) * jitterFactor
	jittered := time.Duration(float64(backoff) * (1 + jitter))
	if jittered <= 0 {
		jittered = time.Millisecond
	}
	if jittered > udpPlainBackoffMax {
		return udpPlainBackoffMax
	}
	return jittered
}

func (server *Server) logWireRX(value byte) {
	if server.wireLog == nil {
		return
	}
	server.wireLog.LogLine("RX %02X", value)
}

func (server *Server) logWireTX(value byte) {
	if server.wireLog == nil {
		return
	}
	server.wireLog.LogLine("TX %02X", value)
}

// writeUpstreamSerialized serializes ALL upstream writes under wireWriteMu.
// TX logging only applies to SEND commands (data bytes on the wire).
func (server *Server) writeUpstreamSerialized(frame downstream.Frame) error {
	server.wireWriteMu.Lock()
	defer server.wireWriteMu.Unlock()
	if len(frame.Payload) == 1 && southboundenh.ENHCommand(frame.Command) == southboundenh.ENHReqSend {
		server.logWireTX(frame.Payload[0])
	}
	return server.upstream.WriteFrame(frame)
}

func (server *Server) deliverPendingStartFromArbByte(byteValue byte) bool {
	if byteValue == ebusSyn {
		return false
	}

	server.pendingStartMu.Lock()
	pending := server.pendingStart
	allowFromWire := false
	if pending != nil {
		allowFromWire = pending.mode == pendingStartModeUDPPlain
	}
	if pending == nil || !allowFromWire || pending.delivered {
		server.pendingStartMu.Unlock()
		return false
	}
	pending.delivered = true
	initiator := pending.initiator
	server.pendingStartMu.Unlock()

	result := downstream.Frame{
		Command: byte(southboundenh.ENHResFailed),
		Payload: []byte{byteValue},
	}
	if byteValue == initiator {
		result.Command = byte(southboundenh.ENHResStarted)
		result.Payload = []byte{initiator}
	}

	return server.deliverPendingStart(result)
}

func (server *Server) deliverPendingInfo(frame downstream.Frame) bool {
	// PX35: Capture pendingInfo snapshot under lock. The lock is released for
	// the session lookup (to avoid mutex nesting with server.mutex), then
	// re-acquired for the seq re-check and state mutation. Reply is sent
	// AFTER releasing pendingInfoMu to prevent lock-order inversion.
	server.pendingInfoMu.Lock()
	pending := server.pendingInfo
	if pending == nil {
		server.pendingInfoMu.Unlock()
		return false
	}
	// GH-P1: Expire stale pending INFO that never received a response.
	if !pending.createdAt.IsZero() && time.Since(pending.createdAt) > pendingInfoTimeout {
		server.pendingInfo = nil
		server.pendingInfoMu.Unlock()
		server.reply(pending.sessionID, downstream.Frame{
			Command: byte(southboundenh.ENHResErrorHost),
			Payload: []byte{0x00},
		})
		return false
	}
	pendingSessionID := pending.sessionID
	pendingSeq := pending.seq
	server.pendingInfoMu.Unlock()

	server.mutex.Lock()
	sess := server.sessions[pendingSessionID]
	server.mutex.Unlock()
	if sess == nil {
		server.clearPendingInfo(pendingSessionID)
		return false
	}

	server.pendingInfoMu.Lock()
	// Re-check under lock using both sessionID and seq.
	if server.pendingInfo == nil || server.pendingInfo.sessionID != pendingSessionID || server.pendingInfo.seq != pendingSeq {
		server.pendingInfoMu.Unlock()
		// R7: Return false — the pending changed between snapshot and
		// re-check, so this frame was not consumed. Returning true would
		// silently drop a valid frame that other handlers (broadcast, etc.)
		// could process.
		return false
	}

	// R6: Refresh the timeout on each successfully delivered frame so that
	// actively-arriving multi-frame INFO responses are not killed by the
	// hard 5s cutoff from the original creation time.
	server.pendingInfo.createdAt = time.Now()

	// Collect frames for identity caching.
	if isIdentityID(server.pendingInfo.infoID) {
		server.pendingInfo.frames = append(server.pendingInfo.frames, frame)
	}

	// Track remaining and finalize cache before releasing the lock.
	if len(frame.Payload) == 1 {
		if server.pendingInfo.remaining < 0 {
			server.pendingInfo.remaining = int(frame.Payload[0])
			if server.pendingInfo.remaining <= 0 {
				if isIdentityID(server.pendingInfo.infoID) {
					server.infoCache.put(server.pendingInfo.infoID, server.pendingInfo.frames)
				}
				server.pendingInfo = nil
			}
		} else {
			server.pendingInfo.remaining--
			if server.pendingInfo.remaining <= 0 {
				if isIdentityID(server.pendingInfo.infoID) {
					server.infoCache.put(server.pendingInfo.infoID, server.pendingInfo.frames)
				}
				server.pendingInfo = nil
			}
		}
	}
	// Release pendingInfoMu BEFORE calling reply to prevent lock-order
	// inversion with server.mutex (handleInfo takes mutex then pendingInfoMu).
	server.pendingInfoMu.Unlock()

	server.reply(pendingSessionID, frame)
	return true
}

// nilPendingStartLocked nils pendingStart under pendingStartMu. All code
// paths that clear pendingStart must use this for consistency.
func (server *Server) nilPendingStartLocked() {
	server.pendingStart = nil
}

func (server *Server) clearPendingStart(sessionID uint64) {
	server.pendingStartMu.Lock()
	if server.pendingStart != nil && server.pendingStart.sessionID == sessionID {
		server.nilPendingStartLocked()
	}
	server.pendingStartMu.Unlock()
}

func (server *Server) clearPendingInfo(sessionID uint64) {
	server.pendingInfoMu.Lock()
	if server.pendingInfo != nil && server.pendingInfo.sessionID == sessionID {
		server.pendingInfo = nil
	}
	server.pendingInfoMu.Unlock()
}
