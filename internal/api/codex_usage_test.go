package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexAccountUsageRoute(t *testing.T) {
	server := newTestServer(t)
	access := &revocableServerAccessProvider{}
	server.accessManager.SetProviders([]sdkaccess.Provider{access})
	for i, used := range []string{"20", "80"} {
		entry := &auth.Auth{ID: "account-usage-" + strconv.Itoa(i), Provider: "codex", Status: auth.StatusActive}
		headers := make(http.Header)
		headers.Set("X-Codex-Primary-Used-Percent", used)
		headers.Set("X-Codex-Primary-Window-Minutes", "300")
		headers.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		entry.Quota.ObserveResponseHeadersForProvider("codex", headers, time.Now())
		if _, err := server.handlers.AuthManager.Register(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/backend-api/wham/usage", nil)
	req.Header.Set("Authorization", "Bearer key-1")
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := ojson.GetBytesNoCopy(rec.Body.Bytes(), "rate_limit.primary_window.used_percent").Int(); got != 50 {
		t.Fatalf("usage=%d, want 50: %s", got, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("quota response may be cached")
	}
	access.revoked.Store(true)
	rejected := httptest.NewRecorder()
	server.engine.ServeHTTP(rejected, req)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key status=%d", rejected.Code)
	}
}

func TestClaudeAccountUsageRoute(t *testing.T) {
	server := newTestServer(t)
	access := &revocableServerAccessProvider{}
	server.accessManager.SetProviders([]sdkaccess.Provider{access})
	for i, used := range []string{"0.2", "0.8"} {
		entry := &auth.Auth{ID: "claude-account-usage-" + strconv.Itoa(i), Provider: "claude", Status: auth.StatusActive}
		headers := make(http.Header)
		headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", used)
		headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		entry.Quota.ObserveResponseHeadersForProvider("claude", headers, time.Now())
		if _, err := server.handlers.AuthManager.Register(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/oauth/usage?skip_spend=1", nil)
	req.Header.Set("Authorization", "Bearer key-1")
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := ojson.GetBytesNoCopy(rec.Body.Bytes(), "five_hour.utilization").Float(); got != 50 {
		t.Fatalf("usage=%v, want 50", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("quota response may be cached")
	}
	access.revoked.Store(true)
	rejected := httptest.NewRecorder()
	server.engine.ServeHTTP(rejected, req)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key status=%d", rejected.Code)
	}
}
