package management

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuotaHistoryHandlersPersistImportAndTokenTotals(t *testing.T) {
	now := time.Now()
	store, err := quotahistory.Open(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetSubscriptionHistoryStore(store)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "a", FileName: "a.json", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: manager}
	router := gin.New()
	router.POST("/history", h.ImportQuotaHistory)
	router.GET("/history", h.GetQuotaHistory)
	router.POST("/tokens", h.ImportTokenHistory)
	router.GET("/tokens", h.GetTokenHistory)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(r, req)
		return r
	}
	payload := quotahistory.History{Version: 1, Credentials: map[string]*quotahistory.Credential{"codex:a.json": {Provider: "codex", Series: map[string]*quotahistory.Series{"weekly": {WindowMinutes: 10080, Readings: []quotahistory.Reading{{float64(now.UnixMilli()), 20, float64(now.Add(time.Hour).UnixMilli())}}}}}}}
	data, _ := json.Marshal(payload)
	if r := request("POST", "/history", string(data)); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	var result quotahistory.History
	r := request("GET", "/history", "")
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Enabled || len(result.Credentials) != 1 {
		t.Fatal(r.Body.String())
	}
	tokenData := `[{"id":"legacy","provider":"claude","at":` + jsonNumber(now.UnixMilli()) + `,"input":100,"output":10,"cacheRead":20,"cacheWrite":5,"total":135,"legacy":true}]`
	if r := request("POST", "/tokens", tokenData); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	r = request("GET", "/tokens", "")
	var tokenResult struct {
		Providers map[string]*quotahistory.TokenTotals `json:"providers"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &tokenResult); err != nil {
		t.Fatal(err)
	}
	if tokenResult.Providers["claude"].Input != 125 || tokenResult.Providers["claude"].CacheRead != 20 {
		t.Fatal(r.Body.String())
	}
	if r := request("GET", "/tokens?from=garbage", ""); r.Code != 400 {
		t.Fatalf("invalid range=%d", r.Code)
	}
	if r := request("POST", "/history", `{"version":99}`); r.Code != 400 {
		t.Fatalf("invalid version=%d", r.Code)
	}
}
func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }
