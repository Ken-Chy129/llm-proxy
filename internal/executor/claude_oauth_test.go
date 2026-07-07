package executor

import (
	"testing"
	"time"
)

// A single 429 must never sideline the whole Claude account for longer than
// maxClaudeReactiveCooldown. Anthropic reports the weekly boundary as the reset
// even when a model-specific cap (e.g. Fable/Opus weekly) was hit, so a far
// reset gets clamped and its "known" reset flag dropped. Short, legit resets
// (e.g. a Retry-After of a few seconds) pass through untouched.
func TestCapReactiveCooldown(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)

	// Weekly-boundary reset (~6 days out) is clamped to now + max, known→false.
	weekly := now.Add(6 * 24 * time.Hour)
	got, known := capReactiveCooldown(weekly, true, now)
	if want := now.Add(maxClaudeReactiveCooldown); !got.Equal(want) {
		t.Errorf("far reset: until=%v want %v", got, want)
	}
	if known {
		t.Errorf("far reset: known must be forced to false")
	}

	// A short, authoritative reset within the cap is left as-is.
	short := now.Add(30 * time.Second)
	got, known = capReactiveCooldown(short, true, now)
	if !got.Equal(short) || !known {
		t.Errorf("short reset changed: until=%v known=%v", got, known)
	}

	// The default-cooldown case (estimated, within cap) is preserved too.
	def := now.Add(60 * time.Second)
	got, known = capReactiveCooldown(def, false, now)
	if !got.Equal(def) || known {
		t.Errorf("default cooldown changed: until=%v known=%v", got, known)
	}
}
