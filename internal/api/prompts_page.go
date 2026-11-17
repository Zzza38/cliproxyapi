package api

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

// promptsPageHTML is the prompt archive viewer. It holds no data itself and
// reads everything from the management API with the key the user enters.
//
//go:embed prompts_page.html
var promptsPageHTML []byte

func servePromptsPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "text/html; charset=utf-8", promptsPageHTML)
}
