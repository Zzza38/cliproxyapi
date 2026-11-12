package promptarchive

import (
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func chunks(lines ...string) [][]byte {
	out := make([][]byte, len(lines))
	for i, l := range lines {
		out[i] = []byte(l)
	}
	return out
}

func TestAssembleOpenAIChatStream(t *testing.T) {
	out := AssembleStream("openai", chunks(
		`{"id":"c1","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think "}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":", world"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":"{\"pa"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\".\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":7}}`,
		`[DONE]`,
	))
	r := ojson.ParseBytes(out)
	if got := r.Get("choices.0.message.content").String(); got != "Hello, world" {
		t.Fatalf("content = %q in %s", got, out)
	}
	if got := r.Get("choices.0.message.reasoning_content").String(); got != "think " {
		t.Fatalf("reasoning = %q", got)
	}
	if got := r.Get("choices.0.message.tool_calls.0.function.arguments").String(); got != `{"path":"."}` {
		t.Fatalf("arguments = %q", got)
	}
	if r.Get("choices.0.finish_reason").String() != "tool_calls" || r.Get("model").String() != "gpt-5" {
		t.Fatalf("missing finish/model: %s", out)
	}
	if in, outTok := usageOf(out); in != 12 || outTok != 7 {
		t.Fatalf("usage = %d/%d", in, outTok)
	}
}

func TestAssembleClaudeStream(t *testing.T) {
	out := AssembleStream("claude", chunks(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-x\",\"content\":[],\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":90,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi \"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"there\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"Bash\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"ls\\\"}\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":42}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	))
	r := ojson.ParseBytes(out)
	if r.Get("content.0.thinking").String() != "hmm" || r.Get("content.0.signature").String() != "sig" {
		t.Fatalf("thinking block wrong: %s", out)
	}
	if r.Get("content.1.text").String() != "Hi there" {
		t.Fatalf("text block wrong: %s", out)
	}
	if r.Get("content.2.input.command").String() != "ls" {
		t.Fatalf("tool input wrong: %s", out)
	}
	if r.Get("stop_reason").String() != "tool_use" || r.Get("usage.output_tokens").Int() != 42 {
		t.Fatalf("stop/usage wrong: %s", out)
	}
	if in, outTok := usageOf(out); in != 100 || outTok != 42 {
		t.Fatalf("usage = %d/%d", in, outTok)
	}
	reply := ParseReply("claude", out)
	if reply == nil || len(reply.Parts) != 3 || reply.Parts[2].Type != "tool_call" || reply.Parts[2].Name != "Bash" {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestAssembleResponsesStreamDropsEchoedRequest(t *testing.T) {
	out := AssembleStream("openai-response", chunks(
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"output\":[]}}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n",
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"instructions\":\"long system prompt\",\"tools\":[{\"type\":\"function\",\"name\":\"x\"}],\"output\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n",
	))
	r := ojson.ParseBytes(out)
	if r.Get("instructions").Exists() || r.Get("tools").Exists() {
		t.Fatalf("echoed request fields kept: %s", out)
	}
	if r.Get("output.0.content.0.text").String() != "Hello" || r.Get("status").String() != "completed" {
		t.Fatalf("output not rebuilt: %s", out)
	}
	if in, outTok := usageOf(out); in != 5 || outTok != 2 {
		t.Fatalf("usage = %d/%d", in, outTok)
	}
}

func TestAssembleGeminiStream(t *testing.T) {
	out := AssembleStream("gemini", chunks(
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"f","args":{"a":1}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"thoughtsTokenCount":1},"modelVersion":"gemini-x"}`,
	))
	r := ojson.ParseBytes(out)
	parts := r.Get("candidates.0.content.parts").Array()
	if len(parts) != 3 || parts[0].Get("thought").Bool() != true || parts[1].Get("text").String() != "Hello" || parts[2].Get("functionCall.name").String() != "f" {
		t.Fatalf("parts wrong: %s", out)
	}
	if in, outTok := usageOf(out); in != 3 || outTok != 5 {
		t.Fatalf("usage = %d/%d", in, outTok)
	}
}

func TestAssembleUnknownKeepsRawChunks(t *testing.T) {
	out := AssembleStream("speech", chunks("abc", "def"))
	if string(out) != "abc\ndef" {
		t.Fatalf("raw = %q", out)
	}
}

func TestParseConversationAndPreview(t *testing.T) {
	cases := []struct {
		protocol, request, preview string
		roles                      []string
	}{
		{"openai", `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello there"},{"role":"assistant","content":null,"tool_calls":[{"function":{"name":"ls","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"out"}]}`,
			"hello there", []string{"system", "user", "assistant", "tool"}},
		{"claude", `{"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":"fix   the\nbug"}]},{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"files"}]}]}]}`,
			"fix the bug", []string{"system", "user", "assistant", "user"}},
		{"openai-response", `{"instructions":"sys","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd</environment_context>"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"write tests"}]},{"type":"function_call","name":"shell","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}]}`,
			"write tests", []string{"system", "user", "assistant", "tool"}},
		{"gemini", `{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"why is the sky blue"}]},{"role":"model","parts":[{"text":"because","thought":true}]}]}`,
			"why is the sky blue", []string{"system", "user", "assistant"}},
		{"openai-response", `{"input":"just a string"}`, "just a string", []string{"user"}},
	}
	for _, tc := range cases {
		msgs := ParseConversation(tc.protocol, []byte(tc.request))
		var roles []string
		for _, m := range msgs {
			roles = append(roles, m.Role)
		}
		if strings.Join(roles, ",") != strings.Join(tc.roles, ",") {
			t.Errorf("%s roles = %v, want %v", tc.protocol, roles, tc.roles)
		}
		if got := previewOf(msgs); got != tc.preview {
			t.Errorf("%s preview = %q, want %q", tc.protocol, got, tc.preview)
		}
	}
}
