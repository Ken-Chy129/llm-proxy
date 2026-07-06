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

// The API may return the model-specific weekly window (e.g. Fable, labeled
// "Weekly limit") before the all-models weekly window. Primary/Secondary must be
// pinned to the session and all-models-weekly windows by role, not by array
// position, so the account-level "limited" badge and quota-aware selection key
// off the all-models weekly — never off a model-specific limit that only caps
// one model. Here the Fable window is exhausted (100%) but the all-models weekly
// is fine (30%), so Secondary must be the all-models weekly and the Fable window
// must land in Additional.
func TestParseClaudeUsageLimitsPinsWeeklyAll(t *testing.T) {
	body := []byte(`{"limits":[
		{"kind":"session","group":"session","percent":10,"resets_at":"2026-07-01T09:30:00Z"},
		{"kind":"weekly_fable","group":"weekly","percent":100,"resets_at":"2026-07-08T17:00:00Z"},
		{"kind":"weekly_all","group":"weekly","percent":70,"resets_at":"2026-07-08T16:59:00Z"}
	]}`)
	info, err := ParseClaudeUsageLimits(body)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if info.Primary == nil || info.Primary.Label != "Current session (5h)" {
		t.Fatalf("primary must be the session window, got %+v", info.Primary)
	}
	if info.Secondary == nil || info.Secondary.Label != "Weekly (all models)" {
		t.Fatalf("secondary must be the all-models weekly window, got %+v", info.Secondary)
	}
	if info.Secondary.LimitReached {
		t.Errorf("all-models weekly at 30%% must not be limit-reached: %+v", info.Secondary)
	}
	// The exhausted Fable window must be Additional, so it never benches the
	// account via Primary/Secondary.
	if len(info.Additional) != 1 || info.Additional[0].Primary == nil {
		t.Fatalf("expected the model-specific window in Additional, got %+v", info.Additional)
	}
	if info.Additional[0].Primary.Label != "Weekly limit" || !info.Additional[0].Primary.LimitReached {
		t.Errorf("Fable window wrong: %+v", info.Additional[0].Primary)
	}
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
