package promptarchive

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// ParseConversation converts a request body into protocol-neutral messages.
func ParseConversation(protocol string, request []byte) []Message {
	if len(request) == 0 || !ojson.ValidBytes(request) {
		return nil
	}
	root := ojson.ParseBytes(request)
	if inner := root.Get("request"); inner.IsObject() && !root.Get("contents").Exists() {
		root = inner
	}
	family := protocolFamily(protocol)
	if family == familyUnknown {
		switch {
		case root.Get("messages").IsArray() && root.Get("system").Exists():
			family = familyClaude
		case root.Get("messages").IsArray():
			family = familyOpenAI
		case root.Get("input").Exists():
			family = familyResponses
		case root.Get("contents").Exists():
			family = familyGemini
		}
	}
	var msgs []Message
	switch family {
	case familyOpenAI:
		for _, m := range root.Get("messages").Array() {
			msgs = append(msgs, openAIMessage(m))
		}
	case familyResponses:
		if v := root.Get("instructions").String(); v != "" {
			msgs = append(msgs, Message{Role: "system", Parts: []Part{{Type: "text", Text: v}}})
		}
		input := root.Get("input")
		if input.Type == ojson.String {
			msgs = append(msgs, Message{Role: "user", Parts: []Part{{Type: "text", Text: input.Str}}})
		} else {
			for _, item := range input.Array() {
				msgs = append(msgs, responsesItem(item))
			}
		}
	case familyClaude:
		if sys := root.Get("system"); sys.Exists() {
			msgs = append(msgs, Message{Role: "system", Parts: claudeBlocks(sys)})
		}
		for _, m := range root.Get("messages").Array() {
			msgs = append(msgs, Message{Role: m.Get("role").String(), Parts: claudeBlocks(m.Get("content"))})
		}
	case familyGemini:
		if sys := root.Get("systemInstruction"); sys.Exists() {
			msgs = append(msgs, Message{Role: "system", Parts: geminiParts(sys.Get("parts"))})
		}
		for _, c := range root.Get("contents").Array() {
			msgs = append(msgs, Message{Role: geminiRole(c.Get("role").String()), Parts: geminiParts(c.Get("parts"))})
		}
	default:
		if p := root.Get("prompt"); p.Type == ojson.String {
			msgs = append(msgs, Message{Role: "user", Parts: []Part{{Type: "text", Text: p.Str}}})
		}
	}
	return mergeMessages(msgs)
}

// ParseReply converts an assembled response into one assistant message.
func ParseReply(protocol string, response []byte) *Message {
	if len(response) == 0 || !ojson.ValidBytes(response) {
		return nil
	}
	root := ojson.ParseBytes(response)
	if inner := root.Get("response"); inner.IsObject() {
		root = inner
	}
	var parts []Part
	switch protocolFamily(protocol) {
	case familyOpenAI:
		parts = openAIMessage(root.Get("choices.0.message")).Parts
	case familyResponses:
		for _, item := range root.Get("output").Array() {
			parts = append(parts, responsesItem(item).Parts...)
		}
	case familyClaude:
		parts = claudeBlocks(root.Get("content"))
	case familyGemini:
		parts = geminiParts(root.Get("candidates.0.content.parts"))
	}
	if errMsg := root.Get("error.message").String(); errMsg != "" {
		parts = append(parts, Part{Type: "other", Name: "error", Text: errMsg})
	}
	if len(parts) == 0 {
		return nil
	}
	return &Message{Role: "assistant", Parts: parts}
}

func openAIMessage(m ojson.Result) Message {
	msg := Message{Role: m.Get("role").String()}
	if r := m.Get("reasoning_content").String(); r != "" {
		msg.Parts = append(msg.Parts, Part{Type: "thinking", Text: r})
	}
	content := m.Get("content")
	if msg.Role == "tool" {
		msg.Parts = append(msg.Parts, Part{Type: "tool_result", Name: m.Get("tool_call_id").String(), Text: contentText(content)})
	} else if content.Type == ojson.String {
		msg.Parts = append(msg.Parts, Part{Type: "text", Text: content.Str})
	} else {
		for _, c := range content.Array() {
			switch c.Get("type").String() {
			case "text", "input_text":
				msg.Parts = append(msg.Parts, Part{Type: "text", Text: c.Get("text").String()})
			case "image_url", "input_image":
				msg.Parts = append(msg.Parts, Part{Type: "image"})
			default:
				msg.Parts = append(msg.Parts, Part{Type: "other", Name: c.Get("type").String()})
			}
		}
	}
	for _, tc := range m.Get("tool_calls").Array() {
		msg.Parts = append(msg.Parts, Part{Type: "tool_call", Name: tc.Get("function.name").String(), Text: tc.Get("function.arguments").String()})
	}
	return msg
}

func responsesItem(item ojson.Result) Message {
	typ := item.Get("type").String()
	switch typ {
	case "", "message":
		role := item.Get("role").String()
		content := item.Get("content")
		if content.Type == ojson.String {
			return Message{Role: role, Parts: []Part{{Type: "text", Text: content.Str}}}
		}
		var parts []Part
		for _, c := range content.Array() {
			switch c.Get("type").String() {
			case "input_text", "output_text", "text":
				parts = append(parts, Part{Type: "text", Text: c.Get("text").String()})
			case "refusal":
				parts = append(parts, Part{Type: "text", Text: c.Get("refusal").String()})
			case "input_image":
				parts = append(parts, Part{Type: "image"})
			default:
				parts = append(parts, Part{Type: "other", Name: c.Get("type").String()})
			}
		}
		return Message{Role: role, Parts: parts}
	case "function_call":
		return Message{Role: "assistant", Parts: []Part{{Type: "tool_call", Name: item.Get("name").String(), Text: item.Get("arguments").String()}}}
	case "custom_tool_call":
		return Message{Role: "assistant", Parts: []Part{{Type: "tool_call", Name: item.Get("name").String(), Text: item.Get("input").String()}}}
	case "local_shell_call":
		return Message{Role: "assistant", Parts: []Part{{Type: "tool_call", Name: "local_shell", Text: item.Get("action").Raw}}}
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		return Message{Role: "tool", Parts: []Part{{Type: "tool_result", Name: item.Get("call_id").String(), Text: contentText(item.Get("output"))}}}
	case "reasoning":
		var texts []string
		for _, s := range item.Get("summary").Array() {
			texts = append(texts, s.Get("text").String())
		}
		return Message{Role: "assistant", Parts: []Part{{Type: "thinking", Text: strings.Join(texts, "\n")}}}
	}
	return Message{Role: "assistant", Parts: []Part{{Type: "other", Name: typ}}}
}

func claudeBlocks(content ojson.Result) []Part {
	if content.Type == ojson.String {
		return []Part{{Type: "text", Text: content.Str}}
	}
	var parts []Part
	for _, b := range content.Array() {
		typ := b.Get("type").String()
		switch typ {
		case "text":
			parts = append(parts, Part{Type: "text", Text: b.Get("text").String()})
		case "thinking":
			parts = append(parts, Part{Type: "thinking", Text: b.Get("thinking").String()})
		case "redacted_thinking":
			parts = append(parts, Part{Type: "thinking", Text: "[redacted]"})
		case "tool_use", "server_tool_use", "mcp_tool_use":
			parts = append(parts, Part{Type: "tool_call", Name: b.Get("name").String(), Text: b.Get("input").Raw})
		case "tool_result", "mcp_tool_result":
			parts = append(parts, Part{Type: "tool_result", Name: b.Get("tool_use_id").String(), Text: contentText(b.Get("content"))})
		case "image":
			parts = append(parts, Part{Type: "image"})
		default:
			parts = append(parts, Part{Type: "other", Name: typ})
		}
	}
	return parts
}

func geminiRole(role string) string {
	if role == "model" {
		return "assistant"
	}
	if role == "" {
		return "user"
	}
	return role
}

func geminiParts(parts ojson.Result) []Part {
	var out []Part
	for _, p := range parts.Array() {
		switch {
		case p.Get("functionCall").Exists():
			out = append(out, Part{Type: "tool_call", Name: p.Get("functionCall.name").String(), Text: p.Get("functionCall.args").Raw})
		case p.Get("functionResponse").Exists():
			out = append(out, Part{Type: "tool_result", Name: p.Get("functionResponse.name").String(), Text: p.Get("functionResponse.response").Raw})
		case p.Get("text").Exists():
			typ := "text"
			if p.Get("thought").Bool() {
				typ = "thinking"
			}
			out = append(out, Part{Type: typ, Text: p.Get("text").String()})
		case p.Get("inlineData").Exists(), p.Get("fileData").Exists():
			out = append(out, Part{Type: "image"})
		default:
			out = append(out, Part{Type: "other"})
		}
	}
	return out
}

// contentText flattens tool output that may be a string or an array of text blocks.
func contentText(v ojson.Result) string {
	if v.Type == ojson.String {
		return v.Str
	}
	if v.IsArray() {
		var texts []string
		for _, item := range v.Array() {
			if t := item.Get("text"); t.Exists() {
				texts = append(texts, t.String())
			} else if item.Type == ojson.String {
				texts = append(texts, item.Str)
			}
		}
		return strings.Join(texts, "\n")
	}
	if !v.Exists() {
		return ""
	}
	return v.Raw
}

func mergeMessages(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Parts) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			out[n-1].Parts = append(out[n-1].Parts, m.Parts...)
			continue
		}
		out = append(out, m)
	}
	return out
}

// previewOf returns the latest human-written user text, skipping injected
// wrapper blocks such as <system-reminder> or <environment_context>.
func previewOf(msgs []Message) string {
	var fallback string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		parts := msgs[i].Parts
		for j := len(parts) - 1; j >= 0; j-- {
			if parts[j].Type != "text" {
				continue
			}
			text := strings.TrimSpace(parts[j].Text)
			if text == "" {
				continue
			}
			if !strings.HasPrefix(text, "<") {
				return truncateRunes(strings.Join(strings.Fields(text), " "), maxPreviewRunes)
			}
			if fallback == "" {
				fallback = text
			}
		}
	}
	return truncateRunes(strings.Join(strings.Fields(fallback), " "), maxPreviewRunes)
}

// BuildDetail expands a stored record for display.
func BuildDetail(rec *Record) *Detail {
	d := &Detail{Summary: rec.Meta.summary()}
	d.Request = rawOrString(rec.Request)
	d.Response = rawOrString(rec.Response)
	d.Conversation = ParseConversation(rec.Meta.Protocol, rec.Request)
	d.Reply = ParseReply(rec.Meta.replyProtocol(), rec.Response)
	return d
}

func rawOrString(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	if ojson.ValidBytes(b) {
		return b
	}
	return ojson.AppendJSONString(nil, string(b))
}
