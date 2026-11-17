package openai

import (
	"net/http"
	"strconv"

	"github.com/MoeForge/moego/ojson"
)

// pooledCodexQuotaEvent adapts HTTP quota headers to the Codex websocket event.
func pooledCodexQuotaEvent(headers http.Header) []byte {
	doc := ojson.NewDoc([]byte(`{"type":"codex.rate_limits","rate_limits":{}}`))
	found := false
	for _, window := range []string{"primary", "secondary"} {
		prefix := "X-Codex-" + window + "-"
		used, err := strconv.ParseFloat(headers.Get(prefix+"Used-Percent"), 64)
		if err != nil {
			continue
		}
		reset, errReset := strconv.ParseInt(headers.Get(prefix+"Reset-At"), 10, 64)
		minutes, errMinutes := strconv.ParseInt(headers.Get(prefix+"Window-Minutes"), 10, 64)
		if errReset != nil || errMinutes != nil {
			continue
		}
		if errSet := doc.Set("rate_limits."+window, map[string]any{"used_percent": used, "reset_at": reset, "window_minutes": minutes}); errSet != nil {
			return nil
		}
		found = true
	}
	if !found {
		return nil
	}
	return doc.Bytes()
}
