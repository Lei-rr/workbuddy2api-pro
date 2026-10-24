package pool

import (
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

// issue #171 问题一：软冷却到期后 until/coolKind/reason 字段按设计不清理，
// statusOf 不应再把过期 reason 透出给面板（避免「可用」旁挂着历史 429）。
func TestStatusReasonHiddenAfterCooldownExpires(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 冷却中：reason 正常透出。
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	st, ok := p.Status("u1")
	if !ok || !st.Cooling || st.Reason != "429 rate limit" {
		t.Fatalf("cooling status=%+v ok=%v", st, ok)
	}
	// 冷却到期、字段残留（NoteSuccess 不碰 until/coolKind/reason）：不再透出。
	p.Cooldown("u1", CoolSoft, -time.Minute, "429 rate limit")
	st, ok = p.Status("u1")
	if !ok || st.Cooling {
		t.Fatalf("expired cooldown should not be cooling: %+v", st)
	}
	if st.Reason != "" {
		t.Fatalf("expired cooldown reason leaked to status: %q", st.Reason)
	}
}

func TestStatusCoolKindDefaultsWhenNotCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	st, _ := p.Status("u1")
	if st.CoolKind != "" || st.CoolRemaining != 0 {
		t.Errorf("non-cooling portrait=%+v", st)
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42, 0)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestRemoveMissingFromDir(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestStatusExposesRuntimeFields(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)
	p.Acquire("u1") // in_flight=1
	st, _ := p.Status("u1")
	if st.InFlight != 1 {
		t.Errorf("in_flight=%d want 1", st.InFlight)
	}
	p.SetBreaker(2, time.Hour, 2*time.Hour)
	p.NoteError("u1") // breaker_fails=1
	st, _ = p.Status("u1")
	if st.BreakerFails != 1 {
		t.Errorf("breaker_fails=%d want 1", st.BreakerFails)
	}
	p.Release("u1")
}

func TestRecordTokenUsage(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.RecordTokenUsage("u1", TokenUsageDelta{
		Model:               "glm-5.2",
		HasPromptTokens:     true,
		PromptTokens:        5,
		HasCompletionTokens: true,
		CompletionTokens:    7,
		HasTotalTokens:      true,
		TotalTokens:         12,
		HasLatencyMs:        true,
		LatencyMs:           1250,
		HasTokensPerSecond:  true,
		TokensPerSecond:     9.6,
	})
	p.RecordTokenUsage("u1", TokenUsageDelta{Model: "glm-5.2", HasLatencyMs: true, LatencyMs: 300})
	st, _ := p.Status("u1")
	if st.TokenUsage.RequestCount != 2 {
		t.Errorf("request_count=%d want 2", st.TokenUsage.RequestCount)
	}
	if st.TokenUsage.UsageCount != 1 {
		t.Errorf("usage_count=%d want 1", st.TokenUsage.UsageCount)
	}
	if st.TokenUsage.PromptTokens != 5 || st.TokenUsage.CompletionTokens != 7 || st.TokenUsage.TotalTokens != 12 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs != 300 || st.TokenUsage.LastTokensPerSecond != nil {
		t.Errorf("latest performance should replace speed with unknown: %+v", st.TokenUsage)
	}
	if st.TokenUsage.LastModel != "glm-5.2" || st.TokenUsage.LastUsedAt.Before(before) {
		t.Errorf("last usage=%+v", st.TokenUsage)
	}
}
