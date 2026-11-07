package helps

import (
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// PreserveClaudeSafeguardHeaders keeps the caller's feature negotiation paired
// with its safeguards body, including clients newer than our fingerprint.
// Authentication remains owned by the selected upstream credential.
func PreserveClaudeSafeguardHeaders(dst, src http.Header, body []byte) {
	if !ojson.GetBytesNoCopy(body, "safeguards").Exists() {
		return
	}
	generatedBetas := dst.Get("Anthropic-Beta")
	for name, values := range src {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "x-anthropic-") {
			dst[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	// Keep the feature betas required by the prepared body as well as every
	// caller beta. For example, display=updates requires its matching beta.
	if incoming := HeaderValuesCaseInsensitive(src, "Anthropic-Beta"); len(incoming) > 0 {
		betas := strings.Join(incoming, ",")
		seen := make(map[string]bool)
		for _, beta := range strings.Split(betas, ",") {
			seen[strings.TrimSpace(beta)] = true
		}
		for _, beta := range strings.Split(generatedBetas, ",") {
			beta = strings.TrimSpace(beta)
			if beta != "" && !seen[beta] {
				betas += "," + beta
				seen[beta] = true
			}
		}
		dst.Set("Anthropic-Beta", betas)
	}
}
