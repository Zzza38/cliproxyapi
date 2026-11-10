package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestExecutionReportsPooledQuotaWithoutPassthrough(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			model := "pool-" + provider
			usedKey, resetKey, durationKey := "X-Codex-Primary-Used-Percent", "X-Codex-Primary-Reset-At", "X-Codex-Primary-Window-Minutes"
			first, second, want := "80", "20", "50"
			if provider == "claude" {
				usedKey, resetKey, durationKey = "Anthropic-Ratelimit-Unified-5h-Utilization", "Anthropic-Ratelimit-Unified-5h-Reset", ""
				first, second, want = "0.8", "0.2", "0.5"
			}
			upstream := make(http.Header)
			upstream.Set(usedKey, first)
			upstream.Set(resetKey, strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			if durationKey != "" {
				upstream.Set(durationKey, "300")
			}
			upstream.Set("Set-Cookie", "secret")
			upstream.Set("X-Unrelated", "hidden")
			executor := &modelExecutionCaptureExecutor{provider: provider,
				execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
					return coreexecutor.Response{Payload: []byte(`{"ok":true}`), Headers: upstream.Clone()}, nil
				},
				stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					ch := make(chan coreexecutor.StreamChunk, 1)
					ch <- coreexecutor.StreamChunk{Payload: []byte("data: {\"ok\":true}\n\n")}
					close(ch)
					return &coreexecutor.StreamResult{Headers: upstream.Clone(), Chunks: ch}, nil
				},
			}
			h := newModelExecutionHandler(t, model, executor, &sdkconfig.SDKConfig{})
			other := &coreauth.Auth{ID: "pool-other-" + provider, Provider: provider, Status: coreauth.StatusActive}
			observed := upstream.Clone()
			observed.Set(usedKey, second)
			other.Quota.ObserveResponseHeadersForProvider(provider, observed, time.Now())
			if _, err := h.AuthManager.Register(context.Background(), other); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(other.ID, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(other.ID) })
			ctx := WithPinnedAuthID(context.Background(), "model-execution-"+model)
			check := func(headers http.Header) {
				t.Helper()
				if got := headers.Get(usedKey); got != want {
					t.Fatalf("pooled quota = %q, want %s: %v", got, want, headers)
				}
				if headers.Get("Set-Cookie") != "" || headers.Get("X-Unrelated") != "" {
					t.Fatalf("unrelated upstream headers leaked: %v", headers)
				}
			}
			body := []byte(fmt.Sprintf(`{"model":%q}`, model))
			_, headers, errMsg := h.ExecuteWithAuthManager(ctx, "openai", model, body, "")
			if errMsg != nil {
				t.Fatal(errMsg)
			}
			check(headers)
			internal, internalErr := h.ExecuteModel(ctx, ModelExecutionRequest{EntryProtocol: "openai", ExitProtocol: "openai", Model: model, Body: body})
			if internalErr != nil {
				t.Fatal(internalErr)
			}
			if got := internal.Headers.Get(usedKey); got != first {
				t.Fatalf("internal quota = %s, want original %s", got, first)
			}
			data, headers, errs := h.ExecuteStreamWithAuthManager(ctx, "openai", model, body, "")
			check(headers)
			for range data {
			}
			for errMsg := range errs {
				t.Fatal(errMsg)
			}
		})
	}
}

func TestPooledResponseHeadersPreserveTransportMetadata(t *testing.T) {
	h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))
	original := http.Header{}
	original.Set("X-Codex-Turn-State", "turn-state")
	original.Set("X-Codex-Beta-Features", "feature")
	original.Set("X-Codex-Primary-Used-Percent", "99")
	original.Set("X-Codex-Rate-Limit-Reached-Type", "weekly")
	original.Set("X-Codex-Credits-Balance", "12")
	original.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	result := h.pooledResponseHeaders(context.Background(), []string{"codex"}, "", coreexecutor.Options{}, nil, original)
	if len(result) != 2 || result.Get("X-Codex-Turn-State") != "turn-state" || result.Get("X-Codex-Beta-Features") != "feature" {
		t.Fatalf("filtered headers: %v", result)
	}
	if original.Get("X-Codex-Primary-Used-Percent") != "99" {
		t.Fatal("mutated source headers")
	}
}
