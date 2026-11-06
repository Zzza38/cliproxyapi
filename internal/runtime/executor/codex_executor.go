package executor

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// CodexExecutor is a stateless executor for Codex (OpenAI Responses API entrypoint).
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type CodexExecutor struct {
	cfg *config.Config
}

func NewCodexExecutor(cfg *config.Config) *CodexExecutor { return &CodexExecutor{cfg: cfg} }

func (e *CodexExecutor) Identifier() string { return "codex" }

func (e *CodexExecutor) FetchSubscriptionQuota(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.SubscriptionQuota, error) {
	baseURL := "https://chatgpt.com/backend-api"
	if custom := strings.TrimRight(auth.Attributes["base_url"], "/"); custom != "" {
		baseURL = strings.TrimSuffix(custom, "/codex")
	}
	return helps.FetchSubscriptionQuota(ctx, auth, "codex", baseURL, e.HttpRequest)
}

func (e *CodexExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.Codex.ModelLevelCooling
}

// SupportsApplyPatch reports the actual executor contract, independent of its provider name.
func (e *CodexExecutor) SupportsApplyPatch() bool { return e != nil }
