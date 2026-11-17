package management

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/promptarchive"
)

func promptArchiveStore(c *gin.Context) *promptarchive.Store {
	store := promptarchive.CurrentStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "prompt archive is disabled (set observability.prompt-archive.enable: true)"})
	}
	return store
}

// ListPrompts returns archived prompts, newest first, filtered by query parameters.
func (h *Handler) ListPrompts(c *gin.Context) {
	store := promptArchiveStore(c)
	if store == nil {
		return
	}
	filter, errFilter := promptarchive.ParseFilter(c.Request.URL.Query(), time.Now())
	if errFilter != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errFilter.Error()})
		return
	}
	page, errList := store.List(filter)
	if errList != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errList.Error()})
		return
	}
	c.JSON(http.StatusOK, page)
}

// GetPrompt returns one archived prompt with its request, reply and conversation view.
func (h *Handler) GetPrompt(c *gin.Context) {
	store := promptArchiveStore(c)
	if store == nil {
		return
	}
	id, ok := promptarchive.ParseID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid prompt ID"})
		return
	}
	rec, errGet := store.Get(id)
	if errors.Is(errGet, promptarchive.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "prompt not found"})
		return
	}
	if errGet != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errGet.Error()})
		return
	}
	c.JSON(http.StatusOK, promptarchive.BuildDetail(rec))
}

// GetPromptStats returns archive totals and the compression ratio.
func (h *Handler) GetPromptStats(c *gin.Context) {
	store := promptArchiveStore(c)
	if store == nil {
		return
	}
	stats, errStats := store.Stats()
	if errStats != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errStats.Error()})
		return
	}
	stats.Dropped = promptarchive.Dropped()
	c.JSON(http.StatusOK, stats)
}
