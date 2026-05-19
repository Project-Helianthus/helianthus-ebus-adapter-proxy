package enh

import (
	"testing"
	"time"
)

// helper: feed a sequence of bytes at fixed inter-byte time spacing and
// collect every Feed result (decoded byte + admin event).
type feedStep struct {
	in      byte
	delta   time.Duration // time after previous feed (0 = same instant as previous)
	want    DecodedByte
	wantHas bool
	wantAdm AdminEventKind
}

func runSequence(t *testing.T, steps []feedStep) {
	t.Helper()
	d := &EscapeDecoder{}
	now := time.Unix(1_000_000_000, 0) // arbitrary monotonic-like origin
	for i, step := range steps {
		now = now.Add(step.delta)
		gotDec, gotHas, gotAdm := d.Feed(step.in, now)
		if gotHas != step.wantHas {
			t.Fatalf("step %d (in=0x%02X): hasDecoded=%v want %v", i, step.in, gotHas, step.wantHas)
		}
		if gotHas {
			if gotDec.Value != step.want.Value || gotDec.WasEscaped != step.want.WasEscaped {
				t.Fatalf("step %d (in=0x%02X): decoded=(0x%02X, escaped=%v) want (0x%02X, escaped=%v)",
					i, step.in, gotDec.Value, gotDec.WasEscaped, step.want.Value, step.want.WasEscaped)
			}
		}
		if gotAdm.Kind != step.wantAdm {
			t.Fatalf("step %d (in=0x%02X): adminKind=%v want %v", i, step.in, gotAdm.Kind, step.wantAdm)
		}
	}
}

// TestPlainByteFlowsThroughNormal covers the normal path: every byte
// other than the escape lead 0xA9 is emitted as-is with WasEscaped=false.
// Specifically, a raw 0xAA is emitted as (0xAA, was_escaped=false) so
// the downstream classifier can identify it as a wire AUTO-SYN.
func TestPlainByteFlowsThroughNormal(t *testing.T) {
	t.Parallel()
	runSequence(t, []feedStep{
		{in: 0x08, want: DecodedByte{Value: 0x08, WasEscaped: false}, wantHas: true},
		{in: 0xB5, want: DecodedByte{Value: 0xB5, WasEscaped: false}, wantHas: true},
		{in: 0x24, want: DecodedByte{Value: 0x24, WasEscaped: false}, wantHas: true},
		{in: 0xAA, want: DecodedByte{Value: 0xAA, WasEscaped: false}, wantHas: true},
	})
}

// TestEscapePairPayloadAA tests the canonical 0xA9 0x01 → logical 0xAA
// (was_escaped=true) decoding. This is how payload-0xAA bytes are
// recovered from their wire-encoded form.
func TestEscapePairPayloadAA(t *testing.T) {
	t.Parallel()
	runSequence(t, []feedStep{
		{in: 0xA9, delta: 0, wantHas: false},
		{in: 0x01, delta: 4 * time.Millisecond, want: DecodedByte{Value: 0xAA, WasEscaped: true}, wantHas: true},
	})
}

// TestEscapePairPayloadA9 tests the canonical 0xA9 0x00 → logical 0xA9
// (was_escaped=true) decoding. This is how payload-0xA9 bytes are
// recovered from their wire-encoded form.
func TestEscapePairPayloadA9(t *testing.T) {
	t.Parallel()
	runSequence(t, []feedStep{
		{in: 0xA9, delta: 0, wantHas: false},
		{in: 0x00, delta: 4 * time.Millisecond, want: DecodedByte{Value: 0xA9, WasEscaped: true}, wantHas: true},
	})
}

// TestEscapeAbsorbsOneAaInjection tests the round-9-motivating scenario:
// adapter injects one spurious 0xAA between the 0xA9 lead and the real
// 0x01 second byte. The decoder must absorb the 0xAA silently and emit
// the canonical (0xAA, was_escaped=true) once 0x01 arrives.
func TestEscapeAbsorbsOneAaInjection(t *testing.T) {
	t.Parallel()
	runSequence(t, []feedStep{
		{in: 0xA9, delta: 0, wantHas: false},
		{in: 0xAA, delta: 4 * time.Millisecond, wantHas: false}, // absorbed
		{in: 0x01, delta: 4 * time.Millisecond, want: DecodedByte{Value: 0xAA, WasEscaped: true}, wantHas: true},
	})
}

// TestEscapeAbsorbsMultipleAaInjections tests that the decoder can
// absorb up to MaxAbsorptionsPerEscapePair AA-injection bytes between
// the lead and the completion byte (v8 §5 / I4 budget).
func TestEscapeAbsorbsMultipleAaInjections(t *testing.T) {
	t.Parallel()
	steps := []feedStep{{in: 0xA9, delta: 0, wantHas: false}}
	for i := 0; i < 7; i++ {
		steps = append(steps, feedStep{in: 0xAA, delta: 2 * time.Millisecond, wantHas: false})
	}
	steps = append(steps, feedStep{
		in:      0x01,
		delta:   2 * time.Millisecond,
		want:    DecodedByte{Value: 0xAA, WasEscaped: true},
		wantHas: true,
	})
	runSequence(t, steps)
}

// TestEscapeBudgetExhaustedDropsAllBytes verifies v8 §5 / I4: after
// MaxAbsorptionsPerEscapePair (8) consecutive 0xAA absorptions, the
// 9th 0xAA exhausts the budget. The orphaned 0xA9 + 8 absorbed AAs +
// the over-budget 9th AA are ALL dropped (emit nothing). Admin emits
// AdminEventEscapeBudgetExhausted. Only the timeout path re-processes
// the current byte; the count-exhausted path drops everything so a
// raw AUTO-SYN cannot leak into the downstream classifier after
// declared escape failure.
func TestEscapeBudgetExhaustedDropsAllBytes(t *testing.T) {
	t.Parallel()
	d := &EscapeDecoder{}
	now := time.Unix(1_000_000_000, 0)
	d.Feed(0xA9, now)
	for i := 0; i < 8; i++ {
		now = now.Add(1 * time.Millisecond)
		dec, has, adm := d.Feed(0xAA, now)
		if has {
			t.Fatalf("AA #%d: hasDecoded=true unexpected, decoded=%v", i, dec)
		}
		if adm.Kind != AdminEventNone {
			t.Fatalf("AA #%d: admin event unexpected: %v", i, adm.Kind)
		}
		if d.AbsorbedCount() != i+1 {
			t.Fatalf("AA #%d: absorbedCount=%d want %d", i, d.AbsorbedCount(), i+1)
		}
	}
	// 9th 0xAA exhausts the budget — drop everything, emit nothing.
	now = now.Add(1 * time.Millisecond)
	dec, has, adm := d.Feed(0xAA, now)
	if has {
		t.Fatalf("9th AA: hasDecoded=true unexpected (should be dropped), decoded=%v", dec)
	}
	if adm.Kind != AdminEventEscapeBudgetExhausted {
		t.Fatalf("9th AA admin: got %v, want AdminEventEscapeBudgetExhausted", adm.Kind)
	}
	if adm.Absorbed != 8 {
		t.Fatalf("9th AA admin.Absorbed: got %d, want 8", adm.Absorbed)
	}
	if d.State() != EscapeStateNormal {
		t.Fatalf("9th AA: decoder state %v, want NORMAL", d.State())
	}
	// Subsequent bytes should be processed normally in NORMAL state.
	now = now.Add(1 * time.Millisecond)
	dec, has, adm = d.Feed(0x42, now)
	if !has || dec.Value != 0x42 || dec.WasEscaped {
		t.Fatalf("post-exhaustion plain byte: got (0x%02X, esc=%v, has=%v), want (0x42, esc=false, has=true)",
			dec.Value, dec.WasEscaped, has)
	}
	if adm.Kind != AdminEventNone {
		t.Fatalf("post-exhaustion admin: got %v, want None", adm.Kind)
	}
}

// TestEscapeExactlyEightAbsorptionsThenCompletionStillValid verifies
// the boundary case: exactly 8 AAs absorbed (full budget), then a
// completion byte (0x00 or 0x01) arrives. The escape pair must still
// decode successfully — the 8 absorbed AAs are within budget, not
// over it. Per v8 §5 / I4: "absorb up to 8."
func TestEscapeExactlyEightAbsorptionsThenCompletionStillValid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		completion byte
		wantValue  byte
	}{
		{"completion 0x01 decodes to logical 0xAA", 0x01, 0xAA},
		{"completion 0x00 decodes to logical 0xA9", 0x00, 0xA9},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &EscapeDecoder{}
			now := time.Unix(1_000_000_000, 0)
			d.Feed(0xA9, now)
			for i := 0; i < 8; i++ {
				now = now.Add(1 * time.Millisecond)
				if _, has, adm := d.Feed(0xAA, now); has || adm.Kind != AdminEventNone {
					t.Fatalf("AA #%d unexpected emit/admin (has=%v, adm=%v)", i, has, adm.Kind)
				}
			}
			if d.AbsorbedCount() != 8 {
				t.Fatalf("absorbedCount = %d, want 8", d.AbsorbedCount())
			}
			now = now.Add(1 * time.Millisecond)
			dec, has, adm := d.Feed(tc.completion, now)
			if !has {
				t.Fatalf("completion %#02x after 8 absorptions: expected emit, got none", tc.completion)
			}
			if dec.Value != tc.wantValue || !dec.WasEscaped {
				t.Fatalf("completion %#02x: got (0x%02X, esc=%v), want (0x%02X, esc=true)",
					tc.completion, dec.Value, dec.WasEscaped, tc.wantValue)
			}
			if adm.Kind != AdminEventNone {
				t.Fatalf("completion admin: got %v, want None", adm.Kind)
			}
			if d.State() != EscapeStateNormal {
				t.Fatalf("post-completion state: %v, want NORMAL", d.State())
			}
		})
	}
}

// TestEscapeLeadFollowedByEscapeLeadIsMalformed verifies that 0xA9
// followed by another 0xA9 (without the second-byte completion) is a
// malformed escape: the original 0xA9 is dropped, the second 0xA9 is
// dropped too, admin emits AdminEventEscapeRecovery. Per v8 §5
// non-fabrication: do not emit either as raw because neither was a
// real escape pair start.
//
// Note: the strict reading of the v8 §5 pseudocode treats the second
// 0xA9 as "any byte other than 0x00/0x01/0xAA-within-budget", so it
// falls into the malformed branch and is dropped along with the first.
// A more permissive reading might re-process the second 0xA9 as a
// fresh escape lead. The current implementation chose the strict
// interpretation; this test pins that choice.
func TestEscapeLeadFollowedByEscapeLeadIsMalformed(t *testing.T) {
	t.Parallel()
	d := &EscapeDecoder{}
	now := time.Unix(1_000_000_000, 0)
	d.Feed(0xA9, now)
	now = now.Add(2 * time.Millisecond)
	dec, has, adm := d.Feed(0xA9, now)
	if has {
		t.Fatalf("0xA9-after-0xA9: emit unexpected, got %v", dec)
	}
	if adm.Kind != AdminEventEscapeRecovery {
		t.Fatalf("0xA9-after-0xA9 admin: got %v, want AdminEventEscapeRecovery", adm.Kind)
	}
	if d.State() != EscapeStateNormal {
		t.Fatalf("post-malformed state: %v, want NORMAL (decoder does NOT treat second 0xA9 as fresh lead)", d.State())
	}
}

// TestPendingEscapeAtTransportResetClearsState verifies that calling
// Reset() while the decoder is mid-escape (in EscapeStatePending)
// correctly clears all state without emitting anything. Useful when
// the proxy receives a transport RESETTED event mid-escape-pair.
//
// Contract: Reset is fire-and-forget — no admin event, no emitted
// byte. Callers wishing to surface "we abandoned a pending escape on
// reset" should emit that admin signal at the call site.
func TestPendingEscapeAtTransportResetClearsState(t *testing.T) {
	t.Parallel()
	d := &EscapeDecoder{}
	now := time.Unix(1_000_000_000, 0)
	d.Feed(0xA9, now)
	d.Feed(0xAA, now.Add(1*time.Millisecond)) // absorbed
	d.Feed(0xAA, now.Add(2*time.Millisecond)) // absorbed
	if d.State() != EscapeStatePending {
		t.Fatalf("pre-reset state: %v, want PENDING", d.State())
	}
	if d.AbsorbedCount() != 2 {
		t.Fatalf("pre-reset absorbed: %d, want 2", d.AbsorbedCount())
	}
	// Reset (simulating transport RESETTED).
	d.Reset()
	if d.State() != EscapeStateNormal {
		t.Fatalf("post-reset state: %v, want NORMAL", d.State())
	}
	if d.AbsorbedCount() != 0 {
		t.Fatalf("post-reset absorbed: %d, want 0", d.AbsorbedCount())
	}
	// Decoder must function normally after reset; a fresh escape pair
	// from immediately after reset must decode correctly.
	d.Feed(0xA9, now.Add(100*time.Millisecond))
	dec, has, _ := d.Feed(0x01, now.Add(101*time.Millisecond))
	if !has || dec.Value != 0xAA || !dec.WasEscaped {
		t.Fatalf("post-reset fresh escape: got (0x%02X, esc=%v, has=%v), want (0xAA, esc=true, has=true)",
			dec.Value, dec.WasEscaped, has)
	}
}

// TestEscapePendingTimeoutDropsAndReprocesses verifies v8 §1.3: after
// 32 ms in EscapeStatePending without a completion byte, the orphaned
// 0xA9 + absorbed AAs are dropped, admin emits AdminEventEscapePendingTimeout,
// and the current byte is re-processed in NORMAL state.
func TestEscapePendingTimeoutDropsAndReprocesses(t *testing.T) {
	t.Parallel()
	d := &EscapeDecoder{}
	now := time.Unix(1_000_000_000, 0)
	d.Feed(0xA9, now)
	// Wait past the 32 ms cap.
	now = now.Add(33 * time.Millisecond)
	// Send a byte that would normally be a valid completion. Because
	// the cap has fired, the decoder treats it as a fresh NORMAL byte.
	dec, has, adm := d.Feed(0x01, now)
	if !has {
		t.Fatal("post-timeout: expected re-processed 0x01 to emit as plain byte")
	}
	if dec.Value != 0x01 || dec.WasEscaped {
		t.Fatalf("post-timeout decoded: got (0x%02X, esc=%v), want (0x01, esc=false)", dec.Value, dec.WasEscaped)
	}
	if adm.Kind != AdminEventEscapePendingTimeout {
		t.Fatalf("post-timeout admin: got %v, want AdminEventEscapePendingTimeout", adm.Kind)
	}
	if adm.Duration < 32*time.Millisecond {
		t.Fatalf("post-timeout admin.Duration: got %v, want >= 32ms", adm.Duration)
	}
	if d.State() != EscapeStateNormal {
		t.Fatalf("post-timeout: state %v, want NORMAL", d.State())
	}
}

// TestEscapeMalformedRecoveryDropsBothBytes verifies v8 §5: a 0xA9
// followed by anything other than 0x00 / 0x01 / 0xAA-within-budget is
// a malformed escape. Decoder drops the 0xA9 AND the offending byte
// (no data fabrication), emits AdminEventEscapeRecovery, returns to NORMAL.
func TestEscapeMalformedRecoveryDropsBothBytes(t *testing.T) {
	t.Parallel()
	d := &EscapeDecoder{}
	now := time.Unix(1_000_000_000, 0)
	d.Feed(0xA9, now)
	now = now.Add(4 * time.Millisecond)
	// 0x42 is not a valid escape completion (not 0x00/0x01/0xAA).
	dec, has, adm := d.Feed(0x42, now)
	if has {
		t.Fatalf("malformed: hasDecoded=true unexpected, decoded=%v", dec)
	}
	if adm.Kind != AdminEventEscapeRecovery {
		t.Fatalf("malformed admin: got %v, want AdminEventEscapeRecovery", adm.Kind)
	}
	if d.State() != EscapeStateNormal {
		t.Fatalf("malformed: state %v, want NORMAL", d.State())
	}
	// Next byte should be processed normally.
	now = now.Add(4 * time.Millisecond)
	dec2, has2, adm2 := d.Feed(0x55, now)
	if !has2 || dec2.Value != 0x55 || dec2.WasEscaped {
		t.Fatalf("post-malformed plain byte: got (0x%02X, esc=%v, has=%v), want (0x55, esc=false, has=true)",
			dec2.Value, dec2.WasEscaped, has2)
	}
	if adm2.Kind != AdminEventNone {
		t.Fatalf("post-malformed admin: got %v, want None", adm2.Kind)
	}
}

// TestEscapeMalformedAfterAbsorptionsAlsoDrops verifies the malformed
// recovery path correctly handles the case where AAs were absorbed
// before the malformed byte arrived: drop the 0xA9 + N absorbed AAs +
// the malformed byte; admin event carries the absorbed count.
func TestEscapeMalformedAfterAbsorptionsAlsoDrops(t *testing.T) {
	t.Parallel()
	d := &EscapeDecoder{}
	now := time.Unix(1_000_000_000, 0)
	d.Feed(0xA9, now)
	for i := 0; i < 3; i++ {
		now = now.Add(2 * time.Millisecond)
		d.Feed(0xAA, now)
	}
	now = now.Add(2 * time.Millisecond)
	dec, has, adm := d.Feed(0xCC, now)
	if has {
		t.Fatalf("malformed after absorptions: hasDecoded=true unexpected, decoded=%v", dec)
	}
	if adm.Kind != AdminEventEscapeRecovery {
		t.Fatalf("admin: got %v, want AdminEventEscapeRecovery", adm.Kind)
	}
	if adm.Absorbed != 3 {
		t.Fatalf("admin.Absorbed: got %d, want 3", adm.Absorbed)
	}
}

// TestResetReturnsDecoderToNormal verifies Reset clears any pending
// escape state. Useful after transport reset or RESETTED event.
func TestResetReturnsDecoderToNormal(t *testing.T) {
	t.Parallel()
	d := &EscapeDecoder{}
	now := time.Unix(1_000_000_000, 0)
	d.Feed(0xA9, now)
	d.Feed(0xAA, now.Add(1*time.Millisecond))
	if d.State() != EscapeStatePending {
		t.Fatalf("pre-reset: state %v, want PENDING", d.State())
	}
	d.Reset()
	if d.State() != EscapeStateNormal {
		t.Fatalf("post-reset: state %v, want NORMAL", d.State())
	}
	if d.AbsorbedCount() != 0 {
		t.Fatalf("post-reset: absorbedCount %d, want 0", d.AbsorbedCount())
	}
	// Verify the decoder works normally after reset.
	now = now.Add(10 * time.Millisecond)
	dec, has, _ := d.Feed(0x42, now)
	if !has || dec.Value != 0x42 {
		t.Fatalf("post-reset feed: got (0x%02X, has=%v), want (0x42, has=true)", dec.Value, has)
	}
}

// TestAdminEventKindString verifies the human-readable labels for
// admin events match the documented kind names.
func TestAdminEventKindString(t *testing.T) {
	t.Parallel()
	cases := map[AdminEventKind]string{
		AdminEventNone:                  "none",
		AdminEventEscapePendingTimeout:  "escape_pending_timeout",
		AdminEventEscapeRecovery:        "escape_decoder_recovery",
		AdminEventEscapeBudgetExhausted: "escape_budget_exhausted",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("AdminEventKind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

// TestEscapeBudgetConstantsMatchSpec drift-guards the count and time
// budgets against the v8 design doc (§1.3 / §5 / I4).
func TestEscapeBudgetConstantsMatchSpec(t *testing.T) {
	t.Parallel()
	if MaxAbsorptionsPerEscapePair != 8 {
		t.Fatalf("MaxAbsorptionsPerEscapePair = %d, want 8 (per v8 §5 / I4)", MaxAbsorptionsPerEscapePair)
	}
	if EscapePendingTimeout != 32*time.Millisecond {
		t.Fatalf("EscapePendingTimeout = %v, want 32ms (per v8 §1.3 / I4)", EscapePendingTimeout)
	}
}
