package helps

import (
	"net/http"
	"testing"
)

func TestPreserveClaudeSafeguardHeaders(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			dst := http.Header{"Anthropic-Beta": {"thinking-display-updates-2026-08-18,oauth-2025-04-20,extended-cache-ttl-2025-04-11"}, "Authorization": {"Bearer selected"}}
			src := http.Header{"anthropic-beta": {"future-one", "future-two"}, "anthropic-version": {"future-version"}, "authorization": {"Bearer caller"}, "x-api-key": {"caller-key"}}
			PreserveClaudeSafeguardHeaders(dst, src, []byte(`{"safeguards":[]}`))
			if got := dst.Get("Anthropic-Beta"); got != "future-one,future-two,thinking-display-updates-2026-08-18,oauth-2025-04-20,extended-cache-ttl-2025-04-11" {
				t.Fatal(got)
			}
			if dst.Get("Anthropic-Version") != "future-version" || dst.Get("Authorization") != "Bearer selected" || dst.Get("X-Api-Key") != "" {
				t.Fatal("feature headers or selected credential changed")
			}
		})
	}
	dst := http.Header{"Anthropic-Beta": {"baseline"}}
	PreserveClaudeSafeguardHeaders(dst, http.Header{"Anthropic-Beta": {"caller"}}, []byte(`{}`))
	if dst.Get("Anthropic-Beta") != "baseline" {
		t.Fatal("changed requests without safeguards")
	}
}
