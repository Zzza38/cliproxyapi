package openai

import (
	"net/http"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
)

func TestPooledCodexUsagePayload(t *testing.T) {
	now := time.Unix(1800000000, 0)
	headers := make(http.Header)
	headers.Set("X-Codex-Primary-Used-Percent", "35.5")
	headers.Set("X-Codex-Primary-Window-Minutes", "300")
	headers.Set("X-Codex-Primary-Reset-At", "1800003600")
	body, err := pooledCodexUsagePayload(headers, now)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]int64{"rate_limit.primary_window.used_percent": 36, "rate_limit.primary_window.limit_window_seconds": 18000, "rate_limit.primary_window.reset_after_seconds": 3600, "rate_limit.primary_window.reset_at": 1800003600}
	for key, want := range checks {
		if got := ojson.GetBytesNoCopy(body, key).Int(); got != want {
			t.Errorf("%s = %d, want %d: %s", key, got, want, body)
		}
	}
	if !ojson.GetBytesNoCopy(body, "rate_limit.allowed").Bool() {
		t.Fatal("available pool reported as blocked")
	}
	if ojson.GetBytesNoCopy(body, "rate_limit_reset_credits").Exists() {
		t.Fatal("fabricated reset-credit count")
	}
	if ojson.GetBytesNoCopy(body, "rate_limit.secondary_window").Exists() {
		t.Fatal("fabricated unknown window")
	}
}

func TestPooledCodexUsageUnknown(t *testing.T) {
	body, err := pooledCodexUsagePayload(nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"plan_type":"unknown","rate_limit":null}` {
		t.Fatalf("unknown usage = %s", body)
	}
}
