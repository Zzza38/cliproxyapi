package promptarchive

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCaptureRecordsThroughConfigure(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{PromptArchive: config.PromptArchiveConfig{Enable: true, Dir: dir}}
	Configure(cfg)
	t.Cleanup(Shutdown)
	if !Enabled() {
		t.Fatal("archive should be enabled")
	}

	req := []byte(`{"model":"claude-x","messages":[{"role":"user","content":"ping please"}],"stream":true}`)
	c := Begin(CaptureInfo{Protocol: "claude", Model: "claude-x", Requested: "alias-x", Key: "sk-secret-key-1234", Stream: true, Endpoint: "POST /v1/messages", UserAgent: "claude-cli/2.0"}, req)
	req[2] = 'X' // the capture must own its copy
	c.SetSession("sess-1")
	c.AddChunk([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"content\":[]}}\n\n"))
	c.AddChunk([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
	c.AddChunk([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n"))
	c.SetAuth("first-try.json")
	c.SetAuth("claude-auth.json")
	c.Finish("ok", 200, "")
	c.Finish("failed", 500, "late") // second finish is ignored

	// Reconfiguring with the same directory keeps the open store.
	store := CurrentStore()
	Configure(cfg)
	if CurrentStore() != store {
		t.Fatal("same-dir reconfigure reopened the store")
	}

	Shutdown()
	if Enabled() {
		t.Fatal("shutdown should disable")
	}
	var nilCapture *Capture
	nilCapture.AddChunk([]byte("x"))
	nilCapture.Finish("ok", 200, "")
	if Begin(CaptureInfo{}, req) != nil {
		t.Fatal("Begin should return nil when disabled")
	}

	ro, errOpen := Open(dir, true)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer ro.Close()
	page, errList := ro.List(Filter{})
	if errList != nil || len(page.Items) != 1 {
		t.Fatalf("list = %+v, %v", page, errList)
	}
	it := page.Items[0]
	if it.Key != "sk-…1234" || strings.Contains(it.Key, "secret") {
		t.Fatalf("key not masked: %q", it.Key)
	}
	if it.Outcome != "ok" || it.Status != 200 || it.Auth != "claude-auth.json" || it.Session != "sess-1" || it.RequestedModel != "alias-x" || it.Preview != "ping please" {
		t.Fatalf("summary = %+v", it)
	}
	id, _ := ParseID(it.ID)
	rec, errGet := ro.Get(id)
	if errGet != nil {
		t.Fatal(errGet)
	}
	if !bytes.HasPrefix(rec.Request, []byte(`{"model"`)) {
		t.Fatalf("request not copied at Begin: %s", rec.Request)
	}
	if got := ojson.GetBytes(rec.Response, "content.0.text").String(); got != "pong" {
		t.Fatalf("response = %s", rec.Response)
	}
	d := BuildDetail(rec)
	if d.Reply == nil || d.Reply.Parts[0].Text != "pong" || len(d.Conversation) != 1 {
		t.Fatalf("detail = %+v", d)
	}
}

func TestParseFilterAndTimes(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	q := url.Values{"since": {"2h"}, "until": {"2026-11-11"}, "model": {"gpt"}, "deep": {"true"}, "limit": {"5"}, "q": {"hello"}}
	f, errParse := ParseFilter(q, now)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if !f.Since.Equal(now.Add(-2*time.Hour)) || !f.Until.Equal(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)) || !f.Deep || f.Limit != 5 || f.Model != "gpt" {
		t.Fatalf("filter = %+v", f)
	}
	round, errRound := ParseFilter(f.Values(), now)
	if errRound != nil || !round.Since.Equal(f.Since) || round.Query != "hello" || !round.Deep {
		t.Fatalf("round trip = %+v, %v", round, errRound)
	}
	for _, bad := range []string{"yesterday", "5x"} {
		if _, errBad := ParseTime(bad, now); errBad == nil {
			t.Errorf("ParseTime(%q) should fail", bad)
		}
	}
	if d, _ := ParseTime("1.5d", now); !d.Equal(now.Add(-36 * time.Hour)) {
		t.Errorf("1.5d = %v", d)
	}
}

func TestMaskKey(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "…bc", "sk-1234567890abcd": "sk-…abcd"} {
		if got := MaskKey(in); got != want {
			t.Errorf("MaskKey(%q) = %q, want %q", in, got, want)
		}
	}
}
