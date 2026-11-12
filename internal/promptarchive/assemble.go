package promptarchive

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// streamEvents extracts JSON event objects from streamed chunks. Chunks may be
// bare JSON, SSE frames ("event:"/"data:" lines) or Gemini JSON-array pieces.
func streamEvents(chunks [][]byte) []ojson.Result {
	var events []ojson.Result
	for _, chunk := range chunks {
		trimmed := bytes.TrimSpace(chunk)
		piece := bytes.TrimSpace(bytes.Trim(trimmed, "[],"))
		if len(piece) > 0 && piece[0] == '{' && ojson.ValidBytes(piece) {
			events = append(events, ojson.ParseBytes(piece))
			continue
		}
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				line = bytes.TrimSpace(after)
			}
			line = bytes.TrimSpace(bytes.Trim(line, "[],"))
			if len(line) > 0 && line[0] == '{' && ojson.ValidBytes(line) {
				events = append(events, ojson.ParseBytes(line))
			}
		}
	}
	return events
}

// AssembleStream turns streamed chunks into one response shaped like the
// protocol's non-streaming reply. Unknown formats are kept verbatim.
func AssembleStream(protocol string, chunks [][]byte) []byte {
	if len(chunks) == 0 {
		return nil
	}
	events := streamEvents(chunks)
	var out []byte
	if len(events) > 0 {
		switch protocolFamily(protocol) {
		case familyOpenAI:
			out = assembleOpenAIChat(events)
		case familyResponses:
			out = assembleResponses(events)
		case familyClaude:
			out = assembleClaude(events)
		case familyGemini:
			out = assembleGemini(events)
		}
	}
	if out == nil {
		return bytes.Join(chunks, []byte("\n"))
	}
	return CompactResponse(protocol, out)
}

// CompactResponse drops fields a Responses API reply echoes back from the
// request (instructions, tools); the request already stores them.
func CompactResponse(protocol string, body []byte) []byte {
	if protocolFamily(protocol) != familyResponses || len(body) == 0 || body[0] != '{' {
		return body
	}
	root := ojson.ParseBytes(body)
	if !root.Get("instructions").Exists() && !root.Get("tools").Exists() {
		return body
	}
	doc := ojson.NewDoc(body)
	_ = doc.Delete("instructions")
	_ = doc.Delete("tools")
	return doc.Bytes()
}

const (
	familyUnknown = iota
	familyOpenAI
	familyResponses
	familyClaude
	familyGemini
)

func protocolFamily(protocol string) int {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "openai":
		return familyOpenAI
	case "openai-response", "codex":
		return familyResponses
	case "claude":
		return familyClaude
	case "gemini", "gemini-cli", "antigravity":
		return familyGemini
	}
	return familyUnknown
}

func assembleOpenAIChat(events []ojson.Result) []byte {
	type toolAcc struct {
		id, typ, name string
		args          strings.Builder
	}
	type choiceAcc struct {
		content, reasoning strings.Builder
		tools              map[int64]*toolAcc
		toolOrder          []int64
		finish             string
	}
	choices := map[int64]*choiceAcc{}
	var order []int64
	var id, model, usage string
	for _, ev := range events {
		if v := ev.Get("id").String(); v != "" && id == "" {
			id = v
		}
		if v := ev.Get("model").String(); v != "" {
			model = v
		}
		if u := ev.Get("usage"); u.IsObject() {
			usage = u.Raw
		}
		for _, ch := range ev.Get("choices").Array() {
			idx := ch.Get("index").Int()
			acc := choices[idx]
			if acc == nil {
				acc = &choiceAcc{tools: map[int64]*toolAcc{}}
				choices[idx] = acc
				order = append(order, idx)
			}
			delta := ch.Get("delta")
			if !delta.Exists() {
				delta = ch.Get("message")
			}
			if c := delta.Get("content"); c.Type == ojson.String {
				acc.content.WriteString(c.Str)
			}
			for _, key := range []string{"reasoning_content", "reasoning"} {
				if r := delta.Get(key); r.Type == ojson.String {
					acc.reasoning.WriteString(r.Str)
				}
			}
			for _, tc := range delta.Get("tool_calls").Array() {
				ti := tc.Get("index").Int()
				tool := acc.tools[ti]
				if tool == nil {
					tool = &toolAcc{typ: "function"}
					acc.tools[ti] = tool
					acc.toolOrder = append(acc.toolOrder, ti)
				}
				if v := tc.Get("id").String(); v != "" {
					tool.id = v
				}
				if v := tc.Get("type").String(); v != "" {
					tool.typ = v
				}
				if v := tc.Get("function.name").String(); v != "" {
					tool.name = v
				}
				tool.args.WriteString(tc.Get("function.arguments").String())
			}
			if fr := ch.Get("finish_reason"); fr.Type == ojson.String {
				acc.finish = fr.Str
			}
		}
	}
	if len(order) == 0 && usage == "" {
		return nil
	}
	doc := ojson.NewDocString(`{"object":"chat.completion"}`)
	if id != "" {
		_ = doc.Set("id", id)
	}
	if model != "" {
		_ = doc.Set("model", model)
	}
	_ = doc.SetRaw("choices", "[]")
	for n, idx := range order {
		acc := choices[idx]
		base := fmt.Sprintf("choices.%d", n)
		_ = doc.Set(base+".index", idx)
		_ = doc.Set(base+".message.role", "assistant")
		if acc.content.Len() > 0 || len(acc.toolOrder) == 0 {
			_ = doc.Set(base+".message.content", acc.content.String())
		} else {
			_ = doc.SetRaw(base+".message.content", "null")
		}
		if acc.reasoning.Len() > 0 {
			_ = doc.Set(base+".message.reasoning_content", acc.reasoning.String())
		}
		for tn, ti := range acc.toolOrder {
			tool := acc.tools[ti]
			tbase := fmt.Sprintf("%s.message.tool_calls.%d", base, tn)
			_ = doc.Set(tbase+".id", tool.id)
			_ = doc.Set(tbase+".type", tool.typ)
			_ = doc.Set(tbase+".function.name", tool.name)
			_ = doc.Set(tbase+".function.arguments", tool.args.String())
		}
		if acc.finish != "" {
			_ = doc.Set(base+".finish_reason", acc.finish)
		}
	}
	if usage != "" {
		_ = doc.SetRaw("usage", usage)
	}
	return doc.Bytes()
}

func assembleResponses(events []ojson.Result) []byte {
	var final, base string
	var items []string
	var text strings.Builder
	for _, ev := range events {
		switch ev.Get("type").String() {
		case "response.completed", "response.done", "response.incomplete", "response.failed":
			if r := ev.Get("response"); r.IsObject() {
				final = r.Raw
			}
		case "response.created", "response.in_progress":
			if r := ev.Get("response"); r.IsObject() {
				base = r.Raw
			}
		case "response.output_item.done":
			if item := ev.Get("item"); item.IsObject() {
				items = append(items, item.Raw)
			}
		case "response.output_text.delta":
			text.WriteString(ev.Get("delta").String())
		}
	}
	if final == "" && base == "" && len(items) == 0 && text.Len() == 0 {
		return nil
	}
	doc := ojson.NewDocString(`{"object":"response","status":"incomplete"}`)
	if final != "" {
		doc = ojson.NewDocString(final)
	} else if base != "" {
		doc = ojson.NewDocString(base)
		_ = doc.Set("status", "incomplete")
	}
	// Some upstreams send an empty output array in the terminal event.
	if out := doc.Get("output"); !out.IsArray() || len(out.Array()) == 0 {
		if len(items) > 0 {
			_ = doc.SetRaw("output", "["+strings.Join(items, ",")+"]")
		} else if text.Len() > 0 {
			msg := ojson.NewDocString(`{"type":"message","role":"assistant","content":[{"type":"output_text"}]}`)
			_ = msg.Set("content.0.text", text.String())
			_ = doc.SetRaw("output", "["+msg.String()+"]")
		}
	}
	return doc.Bytes()
}

func assembleClaude(events []ojson.Result) []byte {
	type blockAcc struct {
		start                      string
		typ                        string
		text, thinking, sig, input strings.Builder
	}
	blocks := map[int64]*blockAcc{}
	var order []int64
	var message, deltaUsage, stopReason, stopSeq, errRaw string
	for _, ev := range events {
		switch ev.Get("type").String() {
		case "message_start":
			message = ev.Get("message").Raw
		case "content_block_start":
			idx := ev.Get("index").Int()
			cb := ev.Get("content_block")
			blocks[idx] = &blockAcc{start: cb.Raw, typ: cb.Get("type").String()}
			order = append(order, idx)
		case "content_block_delta":
			b := blocks[ev.Get("index").Int()]
			if b == nil {
				continue
			}
			d := ev.Get("delta")
			switch d.Get("type").String() {
			case "text_delta":
				b.text.WriteString(d.Get("text").String())
			case "thinking_delta":
				b.thinking.WriteString(d.Get("thinking").String())
			case "signature_delta":
				b.sig.WriteString(d.Get("signature").String())
			case "input_json_delta":
				b.input.WriteString(d.Get("partial_json").String())
			}
		case "message_delta":
			if v := ev.Get("delta.stop_reason"); v.Type == ojson.String {
				stopReason = v.Str
			}
			if v := ev.Get("delta.stop_sequence"); v.Type == ojson.String {
				stopSeq = v.Str
			}
			if u := ev.Get("usage"); u.IsObject() {
				deltaUsage = u.Raw
			}
		case "error":
			errRaw = ev.Raw
		case "message":
			// A non-streamed message delivered as a single chunk.
			if message == "" && len(order) == 0 {
				return []byte(ev.Raw)
			}
		}
	}
	if message == "" && len(order) == 0 {
		if errRaw != "" {
			return []byte(errRaw)
		}
		return nil
	}
	if message == "" {
		message = `{"type":"message","role":"assistant"}`
	}
	doc := ojson.NewDocString(message)
	_ = doc.SetRaw("content", "[]")
	for _, idx := range order {
		b := blocks[idx]
		start := b.start
		if start == "" {
			start = `{}`
		}
		block := ojson.NewDocString(start)
		switch b.typ {
		case "text":
			_ = block.Set("text", block.Get("text").String()+b.text.String())
		case "thinking":
			_ = block.Set("thinking", block.Get("thinking").String()+b.thinking.String())
			if b.sig.Len() > 0 {
				_ = block.Set("signature", b.sig.String())
			}
		case "tool_use", "server_tool_use", "mcp_tool_use":
			if input := b.input.String(); input != "" && ojson.Valid(input) {
				_ = block.SetRaw("input", input)
			}
		}
		_ = doc.SetRaw("content.-1", block.String())
	}
	if stopReason != "" {
		_ = doc.Set("stop_reason", stopReason)
	}
	if stopSeq != "" {
		_ = doc.Set("stop_sequence", stopSeq)
	}
	if deltaUsage != "" {
		ojson.Parse(deltaUsage).ForEach(func(k, v ojson.Result) bool {
			if v.Type != ojson.Null {
				_ = doc.SetRaw("usage."+ojson.Escape(k.Str), v.Raw)
			}
			return true
		})
	}
	if errRaw != "" {
		_ = doc.SetRaw("error", errRaw)
	}
	return doc.Bytes()
}

func assembleGemini(events []ojson.Result) []byte {
	type partAcc struct {
		raw     string
		text    strings.Builder
		thought bool
		sig     string
		isText  bool
	}
	var parts []*partAcc
	var finish, usage, modelVersion, responseID string
	for _, ev := range events {
		if inner := ev.Get("response"); inner.IsObject() {
			ev = inner
		}
		cand := ev.Get("candidates.0")
		for _, part := range cand.Get("content.parts").Array() {
			text := part.Get("text")
			if text.Type == ojson.String && !part.Get("functionCall").Exists() {
				thought := part.Get("thought").Bool()
				var last *partAcc
				if n := len(parts); n > 0 {
					last = parts[n-1]
				}
				if last == nil || !last.isText || last.thought != thought || last.sig != "" {
					last = &partAcc{isText: true, thought: thought}
					parts = append(parts, last)
				}
				last.text.WriteString(text.Str)
				if sig := part.Get("thoughtSignature").String(); sig != "" {
					last.sig = sig
				}
				continue
			}
			parts = append(parts, &partAcc{raw: part.Raw})
		}
		if v := cand.Get("finishReason").String(); v != "" {
			finish = v
		}
		if u := ev.Get("usageMetadata"); u.IsObject() {
			usage = u.Raw
		}
		if v := ev.Get("modelVersion").String(); v != "" {
			modelVersion = v
		}
		if v := ev.Get("responseId").String(); v != "" {
			responseID = v
		}
	}
	if len(parts) == 0 && usage == "" && finish == "" {
		return nil
	}
	doc := ojson.NewDocString(`{"candidates":[{"content":{"role":"model","parts":[]}}]}`)
	for _, p := range parts {
		if !p.isText {
			_ = doc.SetRaw("candidates.0.content.parts.-1", p.raw)
			continue
		}
		part := ojson.NewDocString(`{}`)
		_ = part.Set("text", p.text.String())
		if p.thought {
			_ = part.Set("thought", true)
		}
		if p.sig != "" {
			_ = part.Set("thoughtSignature", p.sig)
		}
		_ = doc.SetRaw("candidates.0.content.parts.-1", part.String())
	}
	if finish != "" {
		_ = doc.Set("candidates.0.finishReason", finish)
	}
	if usage != "" {
		_ = doc.SetRaw("usageMetadata", usage)
	}
	if modelVersion != "" {
		_ = doc.Set("modelVersion", modelVersion)
	}
	if responseID != "" {
		_ = doc.Set("responseId", responseID)
	}
	return doc.Bytes()
}

// usageOf extracts input and output token counts from an assembled reply.
func usageOf(resp []byte) (int64, int64) {
	if len(resp) == 0 || resp[0] != '{' {
		return 0, 0
	}
	root := ojson.ParseBytes(resp)
	if u := root.Get("usage"); u.IsObject() {
		if u.Get("prompt_tokens").Exists() {
			return u.Get("prompt_tokens").Int(), u.Get("completion_tokens").Int()
		}
		in := u.Get("input_tokens").Int()
		// Claude reports cached input separately from input_tokens.
		in += u.Get("cache_read_input_tokens").Int() + u.Get("cache_creation_input_tokens").Int()
		return in, u.Get("output_tokens").Int()
	}
	u := root.Get("usageMetadata")
	if !u.Exists() {
		u = root.Get("response.usageMetadata")
	}
	if u.IsObject() {
		return u.Get("promptTokenCount").Int(), u.Get("candidatesTokenCount").Int() + u.Get("thoughtsTokenCount").Int()
	}
	return 0, 0
}
