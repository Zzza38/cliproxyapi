package management

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func (h *Handler) liveQuotaHistoryCredentials() map[string]bool {
	result := map[string]bool{}
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			name := auth.FileName
			if name == "" {
				name = auth.ID
			}
			result[auth.Provider+":"+name] = true
		}
	}
	return result
}

func (h *Handler) GetQuotaHistory(c *gin.Context) {
	store := h.authManager.SubscriptionHistoryStore()
	if store == nil {
		c.JSON(http.StatusOK, quotahistory.History{Version: 1, Credentials: map[string]*quotahistory.Credential{}})
		return
	}
	c.JSON(http.StatusOK, store.History(h.liveQuotaHistoryCredentials(), time.Now()))
}

func (h *Handler) ImportQuotaHistory(c *gin.Context) {
	store := h.authManager.SubscriptionHistoryStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota history unavailable"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20)
	var history quotahistory.History
	if errBind := c.ShouldBindJSON(&history); errBind != nil || history.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid quota history"})
		return
	}
	count, err := store.Import(history, h.liveQuotaHistoryCredentials(), time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "quota history import failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"imported": count})
}

func (h *Handler) GetTokenHistory(c *gin.Context) {
	to := time.Now().UnixMilli()
	from := to - int64(24*time.Hour/time.Millisecond)
	for name, target := range map[string]*int64{"from": &from, "to": &to} {
		if raw := c.Query(name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(400, gin.H{"error": "invalid time range"})
				return
			}
			*target = value
		}
	}
	if from >= to || from < time.Now().Add(-quotahistory.Retention).UnixMilli()-60000 || to > time.Now().Add(time.Minute).UnixMilli() {
		c.JSON(400, gin.H{"error": "invalid time range"})
		return
	}
	store := h.authManager.SubscriptionHistoryStore()
	providers := map[string]*quotahistory.TokenTotals{}
	if store != nil {
		providers = store.Tokens(from, to)
	}
	c.JSON(200, gin.H{"enabled": store != nil, "fromMs": from, "toMs": to, "providers": providers})
}

// ImportTokenHistory imports existing request counters, never credentials or prompts.
func (h *Handler) ImportTokenHistory(c *gin.Context) {
	store := h.authManager.SubscriptionHistoryStore()
	if store == nil {
		c.JSON(503, gin.H{"error": "token history unavailable"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<20)
	var records []struct {
		quotahistory.TokenPoint
		Legacy    bool  `json:"legacy"`
		Reasoning int64 `json:"reasoning"`
	}
	if err := c.ShouldBindJSON(&records); err != nil || len(records) > 100000 {
		c.JSON(400, gin.H{"error": "invalid token history"})
		return
	}
	points := make([]quotahistory.TokenPoint, len(records))
	for i, record := range records {
		points[i] = record.TokenPoint
		if record.Legacy {
			breakdown := usage.EnsureTokenBreakdownForProvider(usage.Detail{InputTokens: record.Input, OutputTokens: record.Output, CacheReadTokens: record.CacheRead, CacheCreationTokens: record.CacheWrite, ReasoningTokens: record.Reasoning, TotalTokens: record.Total}, record.Provider, record.Provider).TokenBreakdown
			points[i].Input = breakdown.Input.TotalTokens
			points[i].Output = breakdown.Output.TotalTokens
			points[i].CacheRead = breakdown.Input.CacheReadTokens
			points[i].CacheWrite = breakdown.Input.CacheWriteTokens
			points[i].Total = breakdown.TotalTokens
		}
	}
	count, err := store.RecordTokens(points, time.Now())
	if err != nil {
		c.JSON(500, gin.H{"error": "token history import failed"})
		return
	}
	c.JSON(200, gin.H{"imported": count})
}
