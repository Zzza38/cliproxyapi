package claude

import (
	"net/http"
	"strconv"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ClaudeUsage reports the normalized Claude account pool using the OAuth usage
// response format. Reset credits and spend limits remain unknown, not zero.
func (h *ClaudeCodeAPIHandler) ClaudeUsage(c *gin.Context) {
	if h.AuthManager == nil || h.AuthManager.HomeEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "local account quota is unavailable"})
		return
	}
	headers := h.AuthManager.PooledQuotaHeaders(c.Request.Context(), []string{"claude"}, "", coreexecutor.Options{}, nil)
	payload, err := pooledClaudeUsagePayload(headers)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode account quota"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json", payload)
}

func pooledClaudeUsagePayload(headers http.Header) ([]byte, error) {
	doc := ojson.NewDoc([]byte(`{"five_hour":null,"seven_day":null}`))
	for _, window := range []struct{ name, key string }{{"five_hour", "5h"}, {"seven_day", "7d"}, {"seven_day_sonnet", "7d-sonnet"}, {"seven_day_opus", "7d-opus"}} {
		prefix := "Anthropic-Ratelimit-Unified-" + window.key + "-"
		used, errUsed := strconv.ParseFloat(headers.Get(prefix+"Utilization"), 64)
		reset, errReset := strconv.ParseInt(headers.Get(prefix+"Reset"), 10, 64)
		if errUsed != nil || errReset != nil {
			continue
		}
		if errSet := doc.Set(window.name, map[string]any{"utilization": used * 100, "resets_at": time.Unix(reset, 0).UTC().Format(time.RFC3339)}); errSet != nil {
			return nil, errSet
		}
	}
	return doc.Bytes(), nil
}
