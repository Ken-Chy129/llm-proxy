package auth

import "testing"

func TestParseClaudeUsageLimitsReal(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":100.0,"resets_at":"2026-07-01T09:30:00.019821+00:00"},"seven_day":{"utilization":35.0,"resets_at":"2026-07-01T09:00:00.019840+00:00"},"seven_day_opus":null,"seven_day_sonnet":null,"limits":[{"kind":"session","group":"session","percent":100,"severity":"critical","resets_at":"2026-07-01T09:30:00.019821+00:00","is_active":true},{"kind":"weekly_all","group":"weekly","percent":35,"severity":"normal","resets_at":"2026-07-01T09:00:00.019840+00:00","is_active":false}]}`)
	info, err := ParseClaudeUsageLimits(body)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if info.Primary == nil || info.Secondary == nil {
		t.Fatalf("expected primary+secondary, got %+v", info)
	}
	if info.Primary.Label != "Current session (5h)" || info.Primary.RemainingPercent != 0 || !info.Primary.LimitReached {
		t.Errorf("primary wrong: %+v", info.Primary)
	}
	if info.Secondary.Label != "Weekly (all models)" || info.Secondary.RemainingPercent != 65 {
		t.Errorf("secondary wrong: %+v", info.Secondary)
	}
	t.Logf("primary=%+v secondary=%+v reset=%s", *info.Primary, *info.Secondary, info.Primary.ResetAt)
}

// A "critical" severity below 100% is a near-limit warning, not a refusal: the
// account is still usable, so LimitReached must stay false (it would otherwise
// bench the account early via Exhausted). Only percent >= 100 counts as reached.
func TestParseClaudeUsageLimitsCriticalUnder100(t *testing.T) {
	body := []byte(`{"limits":[{"kind":"session","group":"session","percent":90,"severity":"critical","resets_at":"2026-07-01T09:30:00Z","is_active":true}]}`)
	info, err := ParseClaudeUsageLimits(body)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if info.Primary == nil {
		t.Fatalf("expected primary, got %+v", info)
	}
	if info.Primary.LimitReached {
		t.Errorf("critical @90%% must not be LimitReached: %+v", info.Primary)
	}
	if info.Primary.RemainingPercent != 10 {
		t.Errorf("expected 10%% remaining, got %+v", info.Primary)
	}
}
