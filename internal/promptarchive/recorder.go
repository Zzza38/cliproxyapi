package promptarchive

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	log "github.com/sirupsen/logrus"
)

const (
	queueSize           = 1024
	maxBatch            = 64
	maxCapturedRespSize = 64 << 20
	maxPreviewRunes     = 200
	maxErrorRunes       = 500
)

// recorder owns the writer goroutine for one open store.
type recorder struct {
	store   *Store
	queue   chan *pending
	done    chan struct{}
	dropped atomic.Int64

	closeMu sync.RWMutex
	closed  bool
}

type pending struct {
	rec    Record
	chunks [][]byte
	stream bool
}

var (
	configMu sync.Mutex
	active   atomic.Pointer[recorder]
)

// Configure opens, reconfigures or closes the archive to match cfg.
func Configure(cfg *config.Config) {
	configMu.Lock()
	defer configMu.Unlock()

	current := active.Load()
	if cfg == nil || !cfg.PromptArchive.Enable {
		if current != nil {
			active.Store(nil)
			current.close()
			log.Info("prompt archive: disabled")
		}
		return
	}
	dir, errResolve := ResolveDir(cfg)
	if errResolve != nil {
		log.Errorf("prompt archive: %v", errResolve)
		return
	}
	maxBytes := int64(cfg.PromptArchive.MaxSizeMB) << 20
	maxAge := time.Duration(cfg.PromptArchive.MaxAgeDays) * 24 * time.Hour
	if current != nil && current.store.Dir() == dir {
		current.store.SetLimits(maxBytes, maxAge)
		return
	}
	if current != nil {
		active.Store(nil)
		current.close()
	}
	store, errOpen := Open(dir, false)
	if errOpen != nil {
		log.Errorf("prompt archive: disabled, %v", errOpen)
		return
	}
	store.SetLimits(maxBytes, maxAge)
	r := &recorder{store: store, queue: make(chan *pending, queueSize), done: make(chan struct{})}
	go r.run()
	active.Store(r)
	log.Infof("prompt archive: recording to %s", dir)
}

// ResolveDir returns the absolute archive directory for cfg.
func ResolveDir(cfg *config.Config) (string, error) {
	return kvstore.ResolvePath(filepath.Clean(cfg.EffectivePromptArchiveDir()))
}

// Shutdown flushes pending records and closes the archive.
func Shutdown() {
	configMu.Lock()
	defer configMu.Unlock()
	if current := active.Swap(nil); current != nil {
		current.close()
	}
}

// CurrentStore returns the open store, or nil when recording is disabled.
func CurrentStore() *Store {
	if r := active.Load(); r != nil {
		return r.store
	}
	return nil
}

// Dropped reports records lost because the write queue was full.
func Dropped() int64 {
	if r := active.Load(); r != nil {
		return r.dropped.Load()
	}
	return 0
}

func (r *recorder) close() {
	r.closeMu.Lock()
	r.closed = true
	close(r.queue)
	r.closeMu.Unlock()
	<-r.done
	r.store.Close()
}

func (r *recorder) submit(p *pending) {
	r.closeMu.RLock()
	defer r.closeMu.RUnlock()
	if r.closed {
		// The archive was closed or moved by a config reload mid-request.
		r.dropped.Add(1)
		return
	}
	select {
	case r.queue <- p:
	default:
		if r.dropped.Add(1)%100 == 1 {
			log.Warn("prompt archive: write queue full, dropping records")
		}
	}
}

func (r *recorder) run() {
	defer close(r.done)
	batch := make([]*Record, 0, maxBatch)
	for p := range r.queue {
		batch = append(batch[:0], p.finalize())
	drain:
		for len(batch) < maxBatch {
			select {
			case next, ok := <-r.queue:
				if !ok {
					break drain
				}
				batch = append(batch, next.finalize())
			default:
				break drain
			}
		}
		if errAppend := r.store.Append(batch); errAppend != nil {
			r.dropped.Add(int64(len(batch)))
			log.Errorf("prompt archive: write %d records: %v", len(batch), errAppend)
		}
	}
}

// finalize runs on the writer goroutine so assembly and parsing stay off request paths.
func (p *pending) finalize() *Record {
	rec := &p.rec
	if p.stream {
		rec.Response = AssembleStream(rec.Meta.replyProtocol(), p.chunks)
		p.chunks = nil
	} else {
		rec.Response = CompactResponse(rec.Meta.replyProtocol(), rec.Response)
	}
	conv := ParseConversation(rec.Meta.Protocol, rec.Request)
	rec.Meta.Preview = previewOf(conv)
	rec.Meta.InTokens, rec.Meta.OutTokens = usageOf(rec.Response)
	return rec
}

// Capture collects one model call. A nil *Capture is valid and records nothing.
type Capture struct {
	mu        sync.Mutex
	r         *recorder
	p         *pending
	started   time.Time
	respBytes int
	finished  bool
}

// CaptureInfo describes a model call at its start.
type CaptureInfo struct {
	Protocol         string
	ResponseProtocol string
	Endpoint         string
	Model            string
	Requested        string
	Key              string
	Session          string
	Trace            string
	ClientIP         string
	UserAgent        string
	Stream           bool
	Internal         bool
}

// Begin starts capturing a call. It returns nil when the archive is disabled.
func Begin(info CaptureInfo, request []byte) *Capture {
	r := active.Load()
	if r == nil {
		return nil
	}
	now := time.Now()
	meta := Meta{
		Time:      now.UnixMilli(),
		Protocol:  info.Protocol,
		Endpoint:  info.Endpoint,
		Model:     info.Model,
		Key:       MaskKey(info.Key),
		Session:   info.Session,
		Trace:     info.Trace,
		ClientIP:  info.ClientIP,
		UserAgent: truncateRunes(info.UserAgent, 120),
		Stream:    info.Stream,
		Internal:  info.Internal,
	}
	if info.ResponseProtocol != "" && info.ResponseProtocol != info.Protocol {
		meta.RespProtocol = info.ResponseProtocol
	}
	if info.Requested != "" && info.Requested != info.Model {
		meta.Requested = info.Requested
	}
	return &Capture{
		r:       r,
		started: now,
		p: &pending{
			rec:    Record{Meta: meta, Request: append([]byte(nil), request...)},
			stream: info.Stream,
		},
	}
}

// SetSession records the session ID once it is known.
func (c *Capture) SetSession(session string) {
	if c == nil || strings.TrimSpace(session) == "" {
		return
	}
	c.mu.Lock()
	c.p.rec.Meta.Session = strings.TrimSpace(session)
	c.mu.Unlock()
}

// AddChunk records one streamed response chunk as delivered to the client.
func (c *Capture) AddChunk(chunk []byte) {
	if c == nil || len(chunk) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished || c.respBytes+len(chunk) > maxCapturedRespSize {
		return
	}
	c.respBytes += len(chunk)
	c.p.chunks = append(c.p.chunks, append([]byte(nil), chunk...))
}

// SetResponse records a complete non-streamed response body.
func (c *Capture) SetResponse(body []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	c.p.stream = false
	c.p.rec.Response = append([]byte(nil), body...)
}

// SetAuth records the upstream credential serving the call. Retries call it
// again, so the last credential tried wins.
func (c *Capture) SetAuth(auth string) {
	if c == nil || strings.TrimSpace(auth) == "" {
		return
	}
	c.mu.Lock()
	c.p.rec.Meta.Auth = strings.TrimSpace(auth)
	c.mu.Unlock()
}

// Finish completes the capture and queues it for writing.
func (c *Capture) Finish(outcome string, status int, errText string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	c.finished = true
	meta := &c.p.rec.Meta
	meta.DurMs = time.Since(c.started).Milliseconds()
	meta.Outcome = outcome
	meta.Status = status
	meta.Error = truncateRunes(errText, maxErrorRunes)
	p := c.p
	c.mu.Unlock()
	c.r.submit(p)
}

// MaskKey keeps enough of a client API key to tell keys apart without storing it.
func MaskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "…" + key[len(key)/2:]
	}
	return key[:3] + "…" + key[len(key)-4:]
}

func truncateRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// Enabled reports whether the archive is currently recording.
func Enabled() bool { return active.Load() != nil }
