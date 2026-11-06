package helps

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// PreserveClaudeSafeguardHeaders keeps the caller's feature negotiation paired
// with its safeguards body, including clients newer than our fingerprint.
// Authentication remains owned by the selected upstream credential.
func PreserveClaudeSafeguardHeaders(dst, src http.Header, body []byte) {
	if !gjson.GetBytes(body, "safeguards").Exists() {
		return
	}
	generatedBetas := dst.Get("Anthropic-Beta")
	for name, values := range src {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "x-anthropic-") {
			dst[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	// OAuth authentication and proxy-selected cache TTLs still require their
	// credential betas. Keep every caller beta, including unknown future ones.
	if incoming := HeaderValuesCaseInsensitive(src, "Anthropic-Beta"); len(incoming) > 0 {
		betas := strings.Join(incoming, ",")
		seen := make(map[string]bool)
		for _, beta := range strings.Split(betas, ",") {
			seen[strings.TrimSpace(beta)] = true
		}
		for _, beta := range strings.Split(generatedBetas, ",") {
			beta = strings.TrimSpace(beta)
			if (beta == "oauth-2025-04-20" || beta == "extended-cache-ttl-2025-04-11") && !seen[beta] {
				betas += "," + beta
				seen[beta] = true
			}
		}
		dst.Set("Anthropic-Beta", betas)
	}
}
