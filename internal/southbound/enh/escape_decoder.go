// Package enh — frame-atomic visibility v8 AA-aware escape decoder.
//
// This decoder is the proxy-side companion to the FSM described in
// helianthus-docs-ebus/architecture/adaptermux/frame-atomic-visibility-v8.md
// §5 and invariant I4. It implements escape-pair decoding (0xA9 0x01 →
// logical 0xAA; 0xA9 0x00 → logical 0xA9) with the v8 §1.3 robustness
// improvements over the existing escape parser:
//
//   - **AA-injection absorption.** When a 0xA9 lead is followed by
//     spurious 0xAA bytes (the adapter buffer leaking AUTO-SYN bytes
//     mid-escape-pair, per the round-9 motivating bug), the decoder
//     silently absorbs up to 8 of them and continues waiting for the
//     real second byte. Without this, every AA-injection mid-escape
//     would corrupt the decoded byte stream and surface as a phantom
//     PROTOCOL_FAULT downstream.
//
//   - **Hard 32 ms wall-clock cap on ESCAPE_PENDING.** Per v8 §1.3:
//     "Beyond 32 ms, the wire is genuinely broken; better to abandon
//     and let downstream FSM detect protocol fault." When the cap is
//     reached, the decoder drops the orphaned 0xA9 plus any absorbed
//     AAs, logs to the admin channel, and re-processes the current
//     byte in NORMAL state to preserve data for next-byte resync (per
//     v8 §1.3 polish commit).
//
//   - **No data fabrication on malformed escape.** A 0xA9 followed by
//     anything other than 0x00 / 0x01 / 0xAA-within-budget is logged
//     as a protocol fault. The decoder emits NOTHING for the failed
//     escape pair (drops both the 0xA9 and the offending byte) and
//     returns to NORMAL state. v6's earlier emit-0xA9-as-raw recovery
//     was rejected because the 0xA9 may have been pure injection
//     noise; emitting it invents a byte the wire never carried.
//
// The decoder is intentionally a small, allocation-free state machine
// with no I/O. Callers feed bytes one at a time via Feed(b, now) and
// receive zero or one decoded byte plus optional admin-channel events.
//
// Step B (per v8 migration) wires this decoder into the proxy's
// adapter-facing read path so its emitted bytes feed the classifier
// described in v8 §4.
//
// This package only declares the decoder. Wiring into the proxy and
// emitting admin events to a real channel is Step B3 (task #4).
package enh

import "time"

// EscapeDecoderState is the current state of the AA-aware escape decoder.
type EscapeDecoderState int

const (
	// EscapeStateNormal is the decoder's idle state. Each byte is
	// emitted as-is unless it is the escape lead 0xA9.
	EscapeStateNormal EscapeDecoderState = iota

	// EscapeStatePending means the decoder has seen 0xA9 and is
	// waiting for the second byte of the escape pair (0x00, 0x01, or
	// up to 8 absorbed 0xAA injection bytes).
	EscapeStatePending
)

// Wire byte values per the eBUS application-layer escape spec.
const (
	// escapeLeadByte is the first byte of an escape pair (0xA9).
	escapeLeadByte = byte(0xA9)

	// autoSynByte is the eBUS wire AUTO-SYN byte (0xAA). When seen in
	// EscapeStatePending state, it is treated as an adapter-spurious
	// AA-injection within the escape pair (absorbed up to the budget).
	autoSynByte = byte(0xAA)

	// escapedZeroSecond means the escape pair decoded to logical 0xA9
	// (i.e., the wire sequence 0xA9 0x00 emits a logical 0xA9).
	escapedZeroSecond = byte(0x00)

	// escapedOneSecond means the escape pair decoded to logical 0xAA
	// (i.e., the wire sequence 0xA9 0x01 emits a logical 0xAA, which
	// is the escape-encoded form of a payload 0xAA byte).
	escapedOneSecond = byte(0x01)
)

// MaxAbsorptionsPerEscapePair is the count-bounded budget on
// AA-injection bytes absorbed while in EscapeStatePending. After 8
// absorptions without a real completion byte, the decoder declares
// escape failure. Per v8 §5 / I4. Mirrored from helianthus-ebusgo's
// FrameAtomicV8MaxAaAbsorptionsPerEscapePair constant; duplicated here
// only to keep this package free of cross-repo go-mod dependencies in
// Step B1. Step B2 (telegram FSM shared library extraction) will
// consolidate the constants into a single source of truth.
const MaxAbsorptionsPerEscapePair = 8

// EscapePendingTimeout is the wall-clock cap on EscapeStatePending.
// Beyond this, the decoder declares ESCAPE_PENDING timeout. Per v8
// §1.3 and I4 (32 ms). Same mirroring note as MaxAbsorptionsPerEscapePair.
const EscapePendingTimeout = 32 * time.Millisecond

// EscapeDecoder is a stateful one-byte-at-a-time decoder for the eBUS
// application-layer escape encoding, augmented with AA-injection
// absorption and a wall-clock cap on the pending state (v8 §1.3 / §5).
//
// EscapeDecoder is NOT safe for concurrent use. Callers must serialize
// access (per the per-session single-goroutine invariant v8 I11).
//
// The zero value is a ready-to-use decoder in EscapeStateNormal.
type EscapeDecoder struct {
	state          EscapeDecoderState
	leadObservedAt time.Time
	absorbedCount  int
}

// Reset returns the decoder to EscapeStateNormal. Useful after a
// transport reset or RESETTED event (per v8 invariant I5: FSM autonomy
// + RESETTED as resync).
func (d *EscapeDecoder) Reset() {
	d.state = EscapeStateNormal
	d.leadObservedAt = time.Time{}
	d.absorbedCount = 0
}

// State returns the current decoder state. Exposed primarily for
// testing and admin-channel observability.
func (d *EscapeDecoder) State() EscapeDecoderState {
	return d.state
}

// AbsorbedCount returns the number of 0xAA bytes absorbed in the
// current EscapeStatePending sequence. Returns 0 in EscapeStateNormal.
// Exposed for testing.
func (d *EscapeDecoder) AbsorbedCount() int {
	return d.absorbedCount
}

// DecodedByte is the output of a successful Feed call. EscapedSource
// distinguishes:
//   - byte arrived literally on the wire (escaped=false), or
//   - byte was reconstructed from an escape pair (escaped=true).
//
// Downstream classifier consumers (the v8 FSM) use this provenance to
// distinguish payload 0xAA (escaped=true) from wire AUTO-SYN
// (escaped=false) without re-deriving from the byte value.
type DecodedByte struct {
	Value      byte
	WasEscaped bool
}

// AdminEventKind classifies admin-channel events the decoder emits
// when it detects fault conditions per v8 §1.3 / §5. These events live
// strictly on the proxy admin channel; per v8 invariant I1 they are
// NEVER injected into client byte streams.
type AdminEventKind int

const (
	// AdminEventNone means the Feed call produced no admin event.
	AdminEventNone AdminEventKind = iota

	// AdminEventEscapePendingTimeout means the decoder spent more than
	// EscapePendingTimeout in EscapeStatePending without resolving.
	// The orphaned 0xA9 and any absorbed AAs are dropped; the current
	// byte is re-processed in NORMAL state. Per v8 §1.3.
	AdminEventEscapePendingTimeout

	// AdminEventEscapeRecovery means the decoder saw a byte in
	// EscapeStatePending that is neither a valid completion byte
	// (0x00 / 0x01) nor an absorbable 0xAA-within-budget. The orphaned
	// 0xA9 and any absorbed AAs are dropped; the current byte is also
	// dropped (treated as continuation of the corruption). Per v8 §5.
	AdminEventEscapeRecovery

	// AdminEventEscapeBudgetExhausted means the decoder saw the
	// (MaxAbsorptionsPerEscapePair + 1)-th 0xAA in EscapeStatePending,
	// exhausting the count-bounded absorption budget. The orphaned
	// 0xA9, the 8 absorbed AAs, AND the over-budget 0xAA are ALL
	// dropped — emit nothing. Per v8 §5 / I4: only the timeout path
	// re-processes the current byte; the count-bounded path drops
	// everything so a raw AUTO-SYN cannot leak into the downstream
	// classifier after declared escape failure.
	AdminEventEscapeBudgetExhausted
)

// String returns a human-readable label for the admin event kind.
// Used in admin-channel logs.
func (k AdminEventKind) String() string {
	switch k {
	case AdminEventNone:
		return "none"
	case AdminEventEscapePendingTimeout:
		return "escape_pending_timeout"
	case AdminEventEscapeRecovery:
		return "escape_decoder_recovery"
	case AdminEventEscapeBudgetExhausted:
		return "escape_budget_exhausted"
	default:
		return "unknown"
	}
}

// AdminEvent captures the per-Feed diagnostic that the decoder wishes
// to surface to the admin channel. Empty (Kind == AdminEventNone)
// means no event.
type AdminEvent struct {
	Kind     AdminEventKind
	Duration time.Duration // wall-clock duration in EscapeStatePending, if relevant
	Absorbed int           // number of AAs absorbed before the event, if relevant
}

// Feed consumes one wire byte and returns:
//   - decoded: a DecodedByte if Feed produced one, plus the boolean
//     hasDecoded indicating whether a byte was emitted at all.
//   - admin: a diagnostic event for the admin channel, or AdminEventNone.
//
// Callers must pass now as the monotonic-clock observation time for b
// (per v8 invariant I0). Tests pass synthetic times.
//
// Feed is allocation-free on the hot path.
func (d *EscapeDecoder) Feed(b byte, now time.Time) (decoded DecodedByte, hasDecoded bool, admin AdminEvent) {
	switch d.state {
	case EscapeStateNormal:
		return d.feedNormal(b, now)
	case EscapeStatePending:
		return d.feedPending(b, now)
	default:
		// Defensive: unreachable. Reset to recover.
		d.Reset()
		return DecodedByte{}, false, AdminEvent{}
	}
}

func (d *EscapeDecoder) feedNormal(b byte, now time.Time) (DecodedByte, bool, AdminEvent) {
	if b == escapeLeadByte {
		// Enter ESCAPE_PENDING; remember when the lead arrived for the
		// 32 ms wall-clock cap.
		d.state = EscapeStatePending
		d.leadObservedAt = now
		d.absorbedCount = 0
		return DecodedByte{}, false, AdminEvent{}
	}
	// Plain wire byte. WasEscaped=false because no escape pair was used.
	// Note: a wire AUTO-SYN byte (0xAA) flows through here too, as
	// (0xAA, was_escaped=false). The classifier in v8 §4 uses this
	// provenance to distinguish wire AUTO-SYN from escape-decoded
	// payload 0xAA.
	return DecodedByte{Value: b, WasEscaped: false}, true, AdminEvent{}
}

func (d *EscapeDecoder) feedPending(b byte, now time.Time) (DecodedByte, bool, AdminEvent) {
	elapsed := now.Sub(d.leadObservedAt)

	// Wall-clock cap fires before the byte-value branches. Per v8 §1.3:
	// "Beyond 32 ms, the wire is genuinely broken; better to abandon."
	if elapsed > EscapePendingTimeout {
		absorbed := d.absorbedCount
		d.state = EscapeStateNormal
		d.leadObservedAt = time.Time{}
		d.absorbedCount = 0
		// Re-process the current byte in NORMAL state to preserve it
		// for next-byte resync (v8 §1.3 polish commit).
		decoded, hasDecoded, _ := d.feedNormal(b, now)
		return decoded, hasDecoded, AdminEvent{
			Kind:     AdminEventEscapePendingTimeout,
			Duration: elapsed,
			Absorbed: absorbed,
		}
	}

	switch b {
	case escapedOneSecond:
		// 0xA9 0x01 → logical 0xAA, WasEscaped=true.
		d.state = EscapeStateNormal
		d.leadObservedAt = time.Time{}
		d.absorbedCount = 0
		return DecodedByte{Value: autoSynByte, WasEscaped: true}, true, AdminEvent{}

	case escapedZeroSecond:
		// 0xA9 0x00 → logical 0xA9, WasEscaped=true.
		d.state = EscapeStateNormal
		d.leadObservedAt = time.Time{}
		d.absorbedCount = 0
		return DecodedByte{Value: escapeLeadByte, WasEscaped: true}, true, AdminEvent{}

	case autoSynByte:
		// AA-injection mid-escape-pair. Absorb if budget allows.
		if d.absorbedCount < MaxAbsorptionsPerEscapePair {
			d.absorbedCount++
			return DecodedByte{}, false, AdminEvent{}
		}
		// Budget exhausted. Per v8 §5 / I4: drop the 0xA9, all
		// absorbed AAs, AND this over-budget AA. Emit NOTHING. Only
		// the timeout path (32 ms wall-clock cap above) re-processes
		// the current byte; the count-exhausted path drops everything
		// so an over-budget raw AUTO-SYN cannot leak into the
		// downstream classifier after declared escape failure.
		absorbed := d.absorbedCount
		d.state = EscapeStateNormal
		d.leadObservedAt = time.Time{}
		d.absorbedCount = 0
		return DecodedByte{}, false, AdminEvent{
			Kind:     AdminEventEscapeBudgetExhausted,
			Duration: elapsed,
			Absorbed: absorbed,
		}

	default:
		// Malformed escape: byte is neither a valid completion (0x00 /
		// 0x01) nor an absorbable 0xAA-within-budget. Drop the 0xA9
		// plus absorbed AAs AND drop this byte (treat as continuation
		// of corruption). Emit admin event.
		absorbed := d.absorbedCount
		d.state = EscapeStateNormal
		d.leadObservedAt = time.Time{}
		d.absorbedCount = 0
		return DecodedByte{}, false, AdminEvent{
			Kind:     AdminEventEscapeRecovery,
			Duration: elapsed,
			Absorbed: absorbed,
		}
	}
}
