package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexAutoExecutorHTTPRetryBufferFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name         string
			status       int
			body         string
			websockets   bool
			wantFallback bool
		}{
			{"retry buffer", 507, "exceeded request buffer limit while retrying upstream", true, true},
			{"websockets disabled", 507, "exceeded request buffer limit while retrying upstream", false, false},
			{"unrelated storage error", 507, "storage is full", true, false},
			{"different status", 500, "exceeded request buffer limit while retrying upstream", true, false},
			{"structured stream error", 507, `{"error":{"status":507,"message":"exceeded request buffer limit while retrying upstream"}}`, true, false},
		} {
			t.Run(tc.name+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				var posts, sockets atomic.Int32
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				checkPayload := func(body []byte) {
					if len(gjson.GetBytes(body, "input.0.content").String()) != (1<<20)+100 {
						t.Error("large input changed during transport fallback")
					}
					if gjson.GetBytes(body, "instructions").String() != "configured instructions" || gjson.GetBytes(body, "prompt_cache_key").Exists() {
						t.Error("payload rules were overwritten or filtered fields restored")
					}
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost {
						posts.Add(1)
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						checkPayload(body)
						w.WriteHeader(tc.status)
						_, _ = io.WriteString(w, tc.body)
						return
					}
					sockets.Add(1)
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, body, err := conn.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					checkPayload(body)
					if gjson.GetBytes(body, "type").String() != "response.create" {
						t.Error("missing WebSocket transport frame")
					}
					if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-buffer-fallback","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)); err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()
				models := []config.PayloadModelRule{{Name: "*"}}
				cfg := &config.Config{Payload: config.PayloadConfig{
					Override: []config.PayloadRule{{Models: models, Params: map[string]any{"instructions": "configured instructions"}}},
					Filter:   []config.PayloadFilterRule{{Models: models, Params: []string{"prompt_cache_key"}}},
				}}
				exec := NewCodexAutoExecutor(cfg)
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}, Metadata: map[string]any{"websockets": tc.websockets}}
				req := core.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":[{"role":"user","content":"` + strings.Repeat("x", (1<<20)+100) + `"}],"instructions":"original","prompt_cache_key":"remove-me"}`)}
				opts := core.Options{SourceFormat: translator.FromString("codex"), Stream: stream}
				var err error
				if stream {
					var result *core.StreamResult
					result, err = exec.ExecuteStream(context.Background(), auth, req, opts)
					if err == nil {
						var output strings.Builder
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
							output.Write(chunk.Payload)
						}
						if !strings.Contains(output.String(), "resp-buffer-fallback") {
							t.Error("fallback completion did not reach client")
						}
					}
				} else {
					var result core.Response
					result, err = exec.Execute(context.Background(), auth, req, opts)
					if tc.wantFallback && !strings.Contains(string(result.Payload), "resp-buffer-fallback") {
						t.Error("fallback response did not reach client")
					}
				}
				if tc.wantFallback && err != nil {
					t.Fatalf("fallback failed: %v", err)
				}
				if !tc.wantFallback && err == nil {
					t.Fatal("expected original HTTP error")
				}
				wantSockets := int32(0)
				if tc.wantFallback {
					wantSockets = 1
				}
				if posts.Load() != 1 || sockets.Load() != wantSockets {
					t.Fatalf("attempts: HTTP=%d WebSocket=%d", posts.Load(), sockets.Load())
				}
			})
		}
	}
}
