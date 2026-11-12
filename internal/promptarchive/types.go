package promptarchive

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Meta is the per-record metadata. Short JSON keys keep the index small.
type Meta struct {
	ID       uint64 `json:"-"`
	Time     int64  `json:"t"`
	DurMs    int64  `json:"d,omitempty"`
	Protocol string `json:"p,omitempty"`
	// RespProtocol is set only when the reply format differs from the request format.
	RespProtocol string `json:"rp,omitempty"`
	Endpoint     string `json:"ep,omitempty"`
	Model        string `json:"m,omitempty"`
	Requested    string `json:"rm,omitempty"`
	Key          string `json:"k,omitempty"`
	Auth         string `json:"a,omitempty"`
	Session      string `json:"s,omitempty"`
	Trace        string `json:"tr,omitempty"`
	Status       int    `json:"c,omitempty"`
	Outcome      string `json:"o,omitempty"`
	Error        string `json:"e,omitempty"`
	Stream       bool   `json:"st,omitempty"`
	Internal     bool   `json:"in,omitempty"`
	ClientIP     string `json:"ip,omitempty"`
	UserAgent    string `json:"ua,omitempty"`
	ReqBytes     int    `json:"rb,omitempty"`
	RespBytes    int    `json:"ob,omitempty"`
	InTokens     int64  `json:"it,omitempty"`
	OutTokens    int64  `json:"ot,omitempty"`
	Preview      string `json:"pv,omitempty"`
}

// Record is one archived model call before or after storage.
type Record struct {
	Meta     Meta
	Request  []byte
	Response []byte
}

// Summary is the API view of a record's metadata.
type Summary struct {
	ID             string    `json:"id"`
	Time           time.Time `json:"time"`
	DurationMs     int64     `json:"duration_ms,omitempty"`
	Protocol       string    `json:"protocol,omitempty"`
	Endpoint       string    `json:"endpoint,omitempty"`
	Model          string    `json:"model,omitempty"`
	RequestedModel string    `json:"requested_model,omitempty"`
	Key            string    `json:"key,omitempty"`
	Auth           string    `json:"auth,omitempty"`
	Session        string    `json:"session,omitempty"`
	Trace          string    `json:"trace,omitempty"`
	Status         int       `json:"status,omitempty"`
	Outcome        string    `json:"outcome,omitempty"`
	Error          string    `json:"error,omitempty"`
	Stream         bool      `json:"stream,omitempty"`
	Internal       bool      `json:"internal,omitempty"`
	ClientIP       string    `json:"client_ip,omitempty"`
	UserAgent      string    `json:"user_agent,omitempty"`
	RequestBytes   int       `json:"request_bytes"`
	ResponseBytes  int       `json:"response_bytes"`
	InputTokens    int64     `json:"input_tokens,omitempty"`
	OutputTokens   int64     `json:"output_tokens,omitempty"`
	Preview        string    `json:"preview,omitempty"`
}

// Detail is a full record with bodies and a protocol-neutral conversation view.
type Detail struct {
	Summary
	Request      json.RawMessage `json:"request,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
	Conversation []Message       `json:"conversation,omitempty"`
	Reply        *Message        `json:"reply,omitempty"`
}

// Message is a protocol-neutral conversation message.
type Message struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`
}

// Part is one piece of a message. Type is text, thinking, tool_call, tool_result, image or other.
type Part struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`
}

// Filter selects records. Empty fields match everything.
type Filter struct {
	Since     time.Time
	Until     time.Time
	Model     string
	Key       string
	Auth      string
	Protocol  string
	Session   string
	Endpoint  string
	Client    string
	Status    string // "ok", "error" or empty
	Query     string
	Deep      bool
	Internal  bool // include internal (proxy-originated) calls
	Limit     int
	Before    string // record ID cursor; returns records strictly older
	MaxDeepMB int    // cap on decompressed bytes scanned by a deep search
}

// Page is one page of list results.
type Page struct {
	Items     []Summary `json:"items"`
	Next      string    `json:"next,omitempty"`
	Scanned   int       `json:"scanned"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Stats summarizes the archive size.
type Stats struct {
	Records     int64     `json:"records"`
	RawBytes    int64     `json:"raw_bytes"`
	StoredBytes int64     `json:"stored_bytes"`
	DiskBytes   int64     `json:"disk_bytes"`
	Ratio       float64   `json:"ratio"`
	Segments    int       `json:"segments"`
	Oldest      time.Time `json:"oldest,omitempty"`
	Newest      time.Time `json:"newest,omitempty"`
	Dropped     int64     `json:"dropped,omitempty"`
	Dir         string    `json:"dir,omitempty"`
}

// FormatID renders a record ID for APIs and the CLI.
func FormatID(id uint64) string { return strconv.FormatUint(id, 36) }

// ParseID parses an ID produced by FormatID.
func ParseID(raw string) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimSpace(strings.ToLower(raw)), 36, 64)
	return id, err == nil && id > 0
}

func (m *Meta) replyProtocol() string {
	if m.RespProtocol != "" {
		return m.RespProtocol
	}
	return m.Protocol
}

func (m *Meta) summary() Summary {
	return Summary{
		ID:             FormatID(m.ID),
		Time:           time.UnixMilli(m.Time),
		DurationMs:     m.DurMs,
		Protocol:       m.Protocol,
		Endpoint:       m.Endpoint,
		Model:          m.Model,
		RequestedModel: m.Requested,
		Key:            m.Key,
		Auth:           m.Auth,
		Session:        m.Session,
		Trace:          m.Trace,
		Status:         m.Status,
		Outcome:        m.Outcome,
		Error:          m.Error,
		Stream:         m.Stream,
		Internal:       m.Internal,
		ClientIP:       m.ClientIP,
		UserAgent:      m.UserAgent,
		RequestBytes:   m.ReqBytes,
		ResponseBytes:  m.RespBytes,
		InputTokens:    m.InTokens,
		OutputTokens:   m.OutTokens,
		Preview:        m.Preview,
	}
}

func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// matchMeta applies every cheap (metadata-only) filter.
func (f *Filter) matchMeta(m *Meta) bool {
	t := time.UnixMilli(m.Time)
	if !f.Since.IsZero() && t.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && !t.Before(f.Until) {
		return false
	}
	if m.Internal && !f.Internal {
		return false
	}
	if f.Model != "" && !containsFold(m.Model, f.Model) && !containsFold(m.Requested, f.Model) {
		return false
	}
	if f.Key != "" && !containsFold(m.Key, f.Key) {
		return false
	}
	if f.Auth != "" && !containsFold(m.Auth, f.Auth) {
		return false
	}
	if f.Protocol != "" && !strings.EqualFold(m.Protocol, f.Protocol) {
		return false
	}
	if f.Session != "" && !containsFold(m.Session, f.Session) {
		return false
	}
	if f.Endpoint != "" && !containsFold(m.Endpoint, f.Endpoint) {
		return false
	}
	if f.Client != "" && !containsFold(m.UserAgent, f.Client) && !containsFold(m.ClientIP, f.Client) {
		return false
	}
	switch strings.ToLower(f.Status) {
	case "ok":
		if m.Outcome != "" && m.Outcome != "ok" {
			return false
		}
	case "error", "failed":
		if m.Outcome == "" || m.Outcome == "ok" {
			return false
		}
	}
	if f.Query != "" && !f.Deep {
		if !containsFold(m.Preview, f.Query) && !containsFold(m.Model, f.Query) && !containsFold(m.Error, f.Query) &&
			!containsFold(m.Session, f.Query) && !containsFold(m.Auth, f.Query) && !containsFold(m.Key, f.Query) &&
			!containsFold(m.UserAgent, f.Query) && FormatID(m.ID) != strings.ToLower(f.Query) {
			return false
		}
	}
	return true
}
