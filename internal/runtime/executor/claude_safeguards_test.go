package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeSafeguardsRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			const verdict = `{"tool_use_id":"tool:original","decision":"allow","future_field":true}`
			captured := make(chan struct {
				body    []byte
				headers http.Header
			}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				captured <- struct {
					body    []byte
					headers http.Header
				}{body, r.Header.Clone()}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_safe\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
					io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"safeguard_results\":["+verdict+"],\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"id":"msg_safe","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":"end_turn","safeguard_results":[`+verdict+`],"usage":{"input_tokens":1,"output_tokens":1}}`)
				}
			}))
			defer server.Close()
			payload := []byte(`{"model":"claude-opus-5-5","thinking":{"type":"adaptive"},"max_tokens":32,"safeguards":[{"type":"future_classifier","tool_use_id":"tool:original","opaque":{"keep":true}}],"future_request_field":{"keep":true},"messages":[{"role":"user","content":"run"},{"role":"assistant","content":[{"type":"tool_use","id":"tool:original","name":"Bash","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool:original","content":"ok"}]}]}`)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL, "fingerprint_profile": "claude-code-cli"}}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"Anthropic-Beta": {"future-safeguards-beta,effort-2025-11-24"}, "Anthropic-Version": {"2099-01-01"}, "Anthropic-Future-Feature": {"opaque"}}}
			executor := NewClaudeExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}
			var output []byte
			if stream {
				result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var buf bytes.Buffer
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					buf.Write(chunk.Payload)
				}
				output = buf.Bytes()
			} else {
				result, err := executor.Execute(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				output = result.Payload
			}
			got := <-captured
			if gjson.GetBytes(got.body, "thinking.display").String() != "updates" {
				t.Fatal("fixture must exercise the proxy-generated progress display")
			}
			for _, path := range []string{"safeguards", "future_request_field"} {
				if gjson.GetBytes(got.body, path).Raw != gjson.GetBytes(payload, path).Raw {
					t.Errorf("changed %s: %s", path, got.body)
				}
			}
			if !bytes.Contains(got.body, []byte(`"id":"tool:original"`)) || !bytes.Contains(got.body, []byte(`"tool_use_id":"tool:original"`)) {
				t.Errorf("rewrote tool IDs: %s", got.body)
			}
			for _, header := range []string{"Anthropic-Beta", "Anthropic-Version", "Anthropic-Future-Feature"} {
				want := opts.Headers.Get(header)
				if header == "Anthropic-Beta" {
					if !strings.HasPrefix(got.headers.Get(header), want) {
						t.Fatalf("caller betas changed: %q", got.headers.Get(header))
					}
					if gjson.GetBytes(got.body, "thinking.display").String() == "updates" && !strings.Contains(got.headers.Get(header), "thinking-display-updates-2026-08-18") {
						t.Fatal("display=updates lost its required beta")
					}
					continue
				}
				if got.headers.Get(header) != want {
					t.Errorf("%s = %q, want %q", header, got.headers.Get(header), want)
				}
			}
			if !strings.Contains(string(output), `"safeguard_results":[`+verdict+`]`) {
				t.Errorf("lost verdict: %s", output)
			}
		})
	}
}
