package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type pooledQuotaExecutor struct {
	websocketCaptureExecutor
	headers http.Header
}

func (e *pooledQuotaExecutor) Identifier() string { return "codex" }

func (e *pooledQuotaExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 3)
	for _, s := range []string{`data: {"type":"response.created","response":{"id":"pool-response"}}`, `data: {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":99}}}`, `data: {"type":"response.completed","response":{"id":"pool-response","output":[]}}`} {
		chunks <- coreexecutor.StreamChunk{Payload: []byte(s + "\n\n")}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Headers: e.headers.Clone(), Chunks: chunks}, nil
}

func TestWebsocketReportsPooledQuotaAndSuppressesAccountEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	headers := make(http.Header)
	headers.Set("X-Codex-Primary-Used-Percent", "80")
	headers.Set("X-Codex-Primary-Window-Minutes", "300")
	headers.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	executor := &pooledQuotaExecutor{headers: headers}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	model := "pooled-websocket-model"
	for _, id := range []string{"pooled-ws-a", "pooled-ws-b"} {
		a := &coreauth.Auth{ID: id, Provider: executor.Identifier(), Status: coreauth.StatusActive}
		signals := headers.Clone()
		signals.Set("X-Codex-Primary-Used-Percent", "20")
		a.Quota.ObserveResponseHeadersForProvider(a.Provider, signals, time.Now())
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, a.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"User-Agent": []string{"codex_cli_rs/1.0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"pooled-websocket-model","input":[]}`)); errWrite != nil {
		t.Fatal(errWrite)
	}
	count := 0
	for {
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatal(errRead)
		}
		typ := ojson.GetBytesNoCopy(payload, "type").String()
		if typ == "error" {
			t.Fatalf("error: %s", payload)
		}
		if typ == "codex.rate_limits" {
			count++
			if used := ojson.GetBytesNoCopy(payload, "rate_limits.primary.used_percent").Float(); used != 50 {
				t.Fatalf("used=%v, want pool 50: %s", used, payload)
			}
		}
		if typ == "response.completed" {
			break
		}
	}
	if count != 1 {
		t.Fatalf("quota events=%d, want exactly one pooled event", count)
	}
}
