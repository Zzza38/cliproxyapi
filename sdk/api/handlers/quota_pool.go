package handlers

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"sync"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// pooledResponseHeaders replaces single-account quota signals even when general
// upstream header passthrough is disabled. Other upstream headers keep their
// existing passthrough policy.
func (h *BaseAPIHandler) pooledResponseHeaders(ctx context.Context, providers []string, model string, opts coreexecutor.Options, raw, downstream http.Header) http.Header {
	if h == nil || h.AuthManager == nil || h.AuthManager.HomeEnabled() {
		return downstream
	}
	supported := false
	for _, provider := range providers {
		if provider == "codex" || provider == "claude" {
			supported = true
		}
	}
	if !supported {
		return downstream
	}
	out := downstream.Clone()
	if out == nil {
		out = make(http.Header)
	}
	for key := range out {
		if isPooledQuotaHeader(key) {
			delete(out, key)
		}
	}
	for key, values := range h.AuthManager.PooledQuotaHeaders(ctx, providers, model, opts, raw) {
		out[key] = values
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Keep non-quota Codex metadata such as turn state and beta capabilities.
func isPooledQuotaHeader(key string) bool {
	lower := strings.ToLower(key)
	if strings.HasPrefix(lower, "anthropic-ratelimit-unified-") {
		return true
	}
	if !strings.HasPrefix(lower, "x-codex-") {
		return false
	}
	for _, marker := range []string{"-used-percent", "-reset-", "-window-minutes", "-credits-", "-limit-reached", "-allowed", "-limit-name", "-over-secondary-limit-percent"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return lower == "x-codex-active-limit" || lower == "x-codex-plan-type" || lower == "x-codex-rate-limit-reached-type"
}

// The manager clones execution metadata before selecting credentials. Capture
// its callback so response reporting knows which account supplied fresh headers.
func captureQuotaSelection(opts coreexecutor.Options) (coreexecutor.Options, func() coreexecutor.Options) {
	var mu sync.Mutex
	var selected string
	previous, _ := opts.Metadata[coreexecutor.SelectedAuthCallbackMetadataKey].(func(string))
	opts.Metadata = maps.Clone(opts.Metadata)
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]any)
	}
	opts.Metadata[coreexecutor.SelectedAuthCallbackMetadataKey] = func(id string) {
		mu.Lock()
		selected = id
		mu.Unlock()
		if previous != nil {
			previous(id)
		}
	}
	return opts, func() coreexecutor.Options {
		result := opts
		result.Metadata = maps.Clone(opts.Metadata)
		mu.Lock()
		result.Metadata[coreexecutor.SelectedAuthMetadataKey] = selected
		mu.Unlock()
		return result
	}
}
