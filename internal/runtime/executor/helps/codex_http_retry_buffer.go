package helps

import (
	"errors"
	"net/http"
	"strings"
)

// IsCodexHTTPRetryBufferError identifies a rejection from the upstream HTTP
// retry buffer. It does not match generic storage errors or failures after a
// response has started, which must not trigger a transport replay.
func IsCodexHTTPRetryBufferError(err error) bool {
	var status interface{ StatusCode() int }
	return err != nil && errors.As(err, &status) && status.StatusCode() == http.StatusInsufficientStorage &&
		strings.TrimSpace(err.Error()) == "exceeded request buffer limit while retrying upstream"
}
