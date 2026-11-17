package claude

import (
	"net/http"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestPooledClaudeUsagePayload(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.5")
	headers.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1800003600")
	body, err := pooledClaudeUsagePayload(headers)
	if err != nil {
		t.Fatal(err)
	}
	if got := ojson.GetBytesNoCopy(body, "five_hour.utilization").Float(); got != 50 {
		t.Fatalf("used percent=%v, want 50", got)
	}
	if got := ojson.GetBytesNoCopy(body, "five_hour.resets_at").String(); got != "2027-01-15T09:00:00Z" {
		t.Fatalf("reset=%s", got)
	}
	if !ojson.GetBytesNoCopy(body, "seven_day").Exists() || ojson.GetBytesNoCopy(body, "seven_day").Raw != "null" {
		t.Fatalf("invented unknown window: %s", body)
	}
	if ojson.GetBytesNoCopy(body, "cedar_ember").Exists() || ojson.GetBytesNoCopy(body, "extra_usage").Exists() {
		t.Fatalf("invented reset credits or spend limits: %s", body)
	}
}
