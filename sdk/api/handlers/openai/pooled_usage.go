package openai

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// CodexUsage serves the account usage read performed by Codex app-server. This
// request uses chatgpt_base_url rather than the model provider's base_url.
func (h *OpenAIResponsesAPIHandler) CodexUsage(c *gin.Context) {
	if h.AuthManager == nil || h.AuthManager.HomeEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "local account quota is unavailable"})
		return
	}
	headers := h.AuthManager.PooledQuotaHeaders(c.Request.Context(), []string{"codex"}, "", coreexecutor.Options{}, nil)
	payload, err := pooledCodexUsagePayload(headers, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode account quota"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json", payload)
}

func pooledCodexUsagePayload(headers http.Header, now time.Time) ([]byte, error) {
	doc := ojson.NewDoc([]byte(`{"plan_type":"unknown","rate_limit":null}`))
	windows := make(map[string]any)
	allowed := true
	for _, name := range []string{"primary", "secondary"} {
		prefix := "X-Codex-" + name + "-"
		used, errUsed := strconv.ParseFloat(headers.Get(prefix+"Used-Percent"), 64)
		minutes, errMinutes := strconv.ParseInt(headers.Get(prefix+"Window-Minutes"), 10, 64)
		reset, errReset := strconv.ParseInt(headers.Get(prefix+"Reset-At"), 10, 64)
		if errUsed != nil || errMinutes != nil || errReset != nil {
			continue
		}
		// Codex's account endpoint schema uses whole percentages, unlike its stream headers.
		windows[name+"_window"] = map[string]any{"used_percent": int(math.Round(used)), "limit_window_seconds": minutes * 60, "reset_after_seconds": max(int64(0), reset-now.Unix()), "reset_at": reset}
		if used >= 100 {
			allowed = false
		}
	}
	if len(windows) > 0 {
		windows["allowed"] = allowed
		windows["limit_reached"] = !allowed
		if errSet := doc.Set("rate_limit", windows); errSet != nil {
			return nil, errSet
		}
	}
	return doc.Bytes(), nil
}
