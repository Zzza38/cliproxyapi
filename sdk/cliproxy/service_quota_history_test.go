package cliproxy

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestTokenHistoryUsesCanonicalAccounting(t *testing.T) {
	now := time.Now()
	store, err := quotahistory.Open(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	p := &subscriptionTokenRecorder{store: store}
	p.HandleUsage(context.Background(), usage.Record{RequestID: "a", Provider: "claude", RequestedAt: now, Detail: usage.Detail{InputTokens: 100, CacheReadTokens: 30, CacheCreationTokens: 20, OutputTokens: 10}})
	p.HandleUsage(context.Background(), usage.Record{RequestID: "b", Provider: "codex", RequestedAt: now, Detail: usage.Detail{InputTokens: 100, CacheReadTokens: 30, OutputTokens: 10}})
	totals := store.Tokens(now.Add(-time.Second).UnixMilli(), now.Add(time.Second).UnixMilli())
	if totals["claude"].Input != 150 || totals["claude"].Total != 160 {
		t.Fatalf("Claude tokens double counted or omitted: %+v", totals["claude"])
	}
	if totals["codex"].Input != 100 || totals["codex"].Total != 110 {
		t.Fatalf("Codex cache counted twice: %+v", totals["codex"])
	}
}
