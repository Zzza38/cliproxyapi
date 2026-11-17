package promptarchive

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

const cliUsage = `Usage: cli-proxy-api prompts [command] [flags]

Commands:
  list            List prompts, newest first (default)
  show <id>       Print one prompt as a readable conversation
  export          Write matching prompts as JSON lines (request + reply)
  stats           Show archive size and compression ratio

Filters (list, export):
  -q text         Search previews, models, sessions, keys and errors
  --deep          Search the full request and reply text instead
  --since 2h      Only newer than an age (90m, 2h, 3d, 1w) or a date (2026-11-01)
  --until date    Only older than an age or date
  --model x       Model name contains x
  --api-key x     Client API key label contains x (keys are stored masked)
  --auth x        Upstream credential ID contains x
  --session x     Session ID contains x
  --client x      User agent or client IP contains x
  --protocol p    openai, openai-response, claude or gemini
  --status s      ok or error
  --internal      Include proxy-originated calls
  -n 50           Number of results (export defaults to everything)
  --before id     Only prompts older than this ID (paging)

Source:
  --dir path      Read an archive directory directly (server must be stopped)
  --server url    Server base URL (default from config host and port)
  --mgmt-key k    Management key (default $MANAGEMENT_PASSWORD)

Output:
  --json          JSON output (list, show)
  --full          Do not shorten long parts (show)
  --raw           Print the stored request and reply JSON (show)
`

type archiveSource interface {
	List(Filter) (*Page, error)
	Detail(id string) (*Detail, error)
	Stats() (*Stats, error)
	Close()
}

type storeSource struct{ store *Store }

func (s storeSource) List(f Filter) (*Page, error) { return s.store.List(f) }
func (s storeSource) Stats() (*Stats, error)       { return s.store.Stats() }
func (s storeSource) Close()                       { s.store.Close() }
func (s storeSource) Detail(id string) (*Detail, error) {
	parsed, ok := ParseID(id)
	if !ok {
		return nil, fmt.Errorf("invalid prompt ID %q", id)
	}
	rec, errGet := s.store.Get(parsed)
	if errGet != nil {
		return nil, errGet
	}
	return BuildDetail(rec), nil
}

type httpSource struct {
	base   string
	key    string
	client *http.Client
}

func (h httpSource) Close() {}

func (h httpSource) get(path string, query url.Values, out any) error {
	target := h.base + "/v8/management/observability/prompts" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, errReq := http.NewRequest(http.MethodGet, target, nil)
	if errReq != nil {
		return errReq
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, errDo := h.client.Do(req)
	if errDo != nil {
		return errDo
	}
	defer func() { _ = resp.Body.Close() }()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return errRead
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error == "" {
			apiErr.Error = resp.Status
		}
		return fmt.Errorf("server: %s", apiErr.Error)
	}
	return json.Unmarshal(body, out)
}

func (h httpSource) List(f Filter) (*Page, error) {
	page := &Page{}
	return page, h.get("", f.Values(), page)
}

func (h httpSource) Detail(id string) (*Detail, error) {
	d := &Detail{}
	return d, h.get("/"+url.PathEscape(id), nil, d)
}

func (h httpSource) Stats() (*Stats, error) {
	st := &Stats{}
	return st, h.get("/stats", nil, st)
}

// cliConfig reads only what the CLI needs, from both the v8 and legacy layouts,
// without the rewrites a full config load may perform.
type cliConfig struct {
	Host          string                     `yaml:"host"`
	Port          int                        `yaml:"port"`
	AuthDir       string                     `yaml:"auth-dir"`
	PromptArchive config.PromptArchiveConfig `yaml:"prompt-archive"`
	TLS           struct {
		Enable bool `yaml:"enable"`
	} `yaml:"tls"`
	Server struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
		TLS  struct {
			Enable bool `yaml:"enable"`
		} `yaml:"tls"`
	} `yaml:"server"`
	OAuth struct {
		AuthDir string `yaml:"auth-dir"`
	} `yaml:"oauth"`
	Observability struct {
		PromptArchive config.PromptArchiveConfig `yaml:"prompt-archive"`
	} `yaml:"observability"`
}

func loadCLIConfig(path string) (dir, base string, errLoad error) {
	var raw cliConfig
	if path != "" {
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			return "", "", errRead
		}
		if errYAML := yaml.Unmarshal(data, &raw); errYAML != nil {
			return "", "", fmt.Errorf("parse %s: %w", path, errYAML)
		}
	}
	cfg := &config.Config{PromptArchive: raw.PromptArchive}
	cfg.AuthDir = raw.AuthDir
	if raw.OAuth.AuthDir != "" {
		cfg.AuthDir = raw.OAuth.AuthDir
	}
	if raw.Observability.PromptArchive.Dir != "" {
		cfg.PromptArchive.Dir = raw.Observability.PromptArchive.Dir
	}
	dir, errResolve := ResolveDir(cfg)
	if errResolve != nil {
		return "", "", errResolve
	}
	host, port, tls := raw.Host, raw.Port, raw.TLS.Enable
	if raw.Server.Host != "" {
		host = raw.Server.Host
	}
	if raw.Server.Port != 0 {
		port = raw.Server.Port
	}
	tls = tls || raw.Server.TLS.Enable
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 8317
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	return dir, scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// RunCLI implements "cli-proxy-api prompts". It returns a process exit code.
func RunCLI(args []string, configPath string, stdout, stderr io.Writer) int {
	cmd := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd == "help" {
		_, _ = fmt.Fprint(stdout, cliUsage)
		return 0
	}

	fs := flag.NewFlagSet("prompts", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		f                     Filter
		since, until          string
		dirFlag, server, mgmt string
		cfgFlag               string
		asJSON, full, raw     bool
	)
	fs.StringVar(&f.Query, "q", "", "")
	fs.BoolVar(&f.Deep, "deep", false, "")
	fs.StringVar(&since, "since", "", "")
	fs.StringVar(&until, "until", "", "")
	fs.StringVar(&f.Model, "model", "", "")
	fs.StringVar(&f.Key, "api-key", "", "")
	fs.StringVar(&f.Auth, "auth", "", "")
	fs.StringVar(&f.Session, "session", "", "")
	fs.StringVar(&f.Client, "client", "", "")
	fs.StringVar(&f.Protocol, "protocol", "", "")
	fs.StringVar(&f.Status, "status", "", "")
	fs.BoolVar(&f.Internal, "internal", false, "")
	fs.IntVar(&f.Limit, "n", 50, "")
	fs.StringVar(&f.Before, "before", "", "")
	fs.StringVar(&dirFlag, "dir", "", "")
	fs.StringVar(&server, "server", "", "")
	fs.StringVar(&mgmt, "mgmt-key", "", "")
	fs.StringVar(&cfgFlag, "config", configPath, "")
	fs.BoolVar(&asJSON, "json", false, "")
	fs.BoolVar(&full, "full", false, "")
	fs.BoolVar(&raw, "raw", false, "")

	var positional []string
	for {
		if errParse := fs.Parse(args); errParse != nil {
			if errors.Is(errParse, flag.ErrHelp) {
				_, _ = fmt.Fprint(stdout, cliUsage)
				return 0
			}
			_, _ = fmt.Fprintf(stderr, "prompts: %v\n\n%s", errParse, cliUsage)
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}

	limitSet := false
	fs.Visit(func(fl *flag.Flag) { limitSet = limitSet || fl.Name == "n" })
	if cmd == "export" && !limitSet {
		f.Limit = int(^uint(0) >> 1)
	}

	now := time.Now()
	var errTime error
	if f.Since, errTime = ParseTime(since, now); errTime == nil {
		f.Until, errTime = ParseTime(until, now)
	}
	if errTime != nil {
		_, _ = fmt.Fprintf(stderr, "prompts: %v\n", errTime)
		return 2
	}

	src, errSrc := openSource(cfgFlag, dirFlag, server, mgmt)
	if errSrc != nil {
		_, _ = fmt.Fprintf(stderr, "prompts: %v\n", errSrc)
		return 1
	}
	defer src.Close()

	var errRun error
	switch cmd {
	case "list", "ls":
		errRun = cliList(src, f, asJSON, stdout)
	case "show", "get":
		if len(positional) != 1 {
			_, _ = fmt.Fprintf(stderr, "prompts: show needs exactly one ID\n")
			return 2
		}
		errRun = cliShow(src, positional[0], asJSON, raw, full, stdout)
	case "export":
		errRun = cliExport(src, f, stdout)
	case "stats":
		errRun = cliStats(src, asJSON, stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "prompts: unknown command %q\n\n%s", cmd, cliUsage)
		return 2
	}
	if errRun != nil {
		_, _ = fmt.Fprintf(stderr, "prompts: %v\n", errRun)
		return 1
	}
	return 0
}

func openSource(configPath, dirFlag, server, mgmtKey string) (archiveSource, error) {
	if dirFlag != "" {
		store, errOpen := Open(dirFlag, true)
		if errOpen != nil {
			return nil, errOpen
		}
		return storeSource{store: store}, nil
	}
	if configPath == "" {
		if _, errStat := os.Stat("config.yaml"); errStat == nil {
			configPath = "config.yaml"
		}
	}
	dir, base, errCfg := loadCLIConfig(configPath)
	if errCfg != nil {
		return nil, errCfg
	}
	if server == "" {
		// Read the files directly when no server holds the index lock.
		if store, errOpen := Open(dir, true); errOpen == nil {
			return storeSource{store: store}, nil
		}
		server = base
	}
	if mgmtKey == "" {
		mgmtKey = strings.TrimSpace(os.Getenv("MANAGEMENT_PASSWORD"))
	}
	if mgmtKey == "" {
		return nil, fmt.Errorf("the archive at %s is in use by the server; pass --mgmt-key or set MANAGEMENT_PASSWORD to query %s", dir, server)
	}
	return httpSource{base: strings.TrimRight(server, "/"), key: mgmtKey, client: &http.Client{}}, nil
}

func cliList(src archiveSource, f Filter, asJSON bool, w io.Writer) error {
	page, errList := src.List(f)
	if errList != nil {
		return errList
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(page)
	}
	if len(page.Items) == 0 {
		_, _ = fmt.Fprintln(w, "No prompts match.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tTIME\tMODEL\tKEY\tTOKENS\tSTATUS\tPROMPT")
	for _, it := range page.Items {
		status := it.Outcome
		if it.Status != 0 && it.Outcome != "ok" {
			status = fmt.Sprintf("%s %d", it.Outcome, it.Status)
		}
		tokens := ""
		if it.InputTokens > 0 || it.OutputTokens > 0 {
			tokens = fmt.Sprintf("%s→%s", shortCount(it.InputTokens), shortCount(it.OutputTokens))
		}
		preview := it.Preview
		if preview == "" && it.Error != "" {
			preview = "error: " + it.Error
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", it.ID, it.Time.Local().Format("Jan 02 15:04:05"),
			truncateRunes(it.Model, 28), it.Key, tokens, status, truncateRunes(preview, 80))
	}
	if errFlush := tw.Flush(); errFlush != nil {
		return errFlush
	}
	if page.Next != "" {
		_, _ = fmt.Fprintf(w, "\nOlder results: add --before %s (or raise -n)\n", page.Next)
	}
	return nil
}

func shortCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}

func cliShow(src archiveSource, id string, asJSON, raw, full bool, w io.Writer) error {
	d, errDetail := src.Detail(id)
	if errDetail != nil {
		return errDetail
	}
	if asJSON || raw {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if raw {
			return enc.Encode(map[string]json.RawMessage{"request": d.Request, "response": d.Response})
		}
		return enc.Encode(d)
	}
	_, _ = fmt.Fprintf(w, "ID        %s\nTime      %s (%.1fs)\nModel     %s\nProtocol  %s %s\n",
		d.ID, d.Time.Local().Format(time.RFC1123), float64(d.DurationMs)/1000, d.Model, d.Protocol, d.Endpoint)
	_, _ = fmt.Fprintf(w, "Status    %s %d %s\n", d.Outcome, d.Status, d.Error)
	for _, kv := range [][2]string{{"Key", d.Key}, {"Auth", d.Auth}, {"Session", d.Session}, {"Client", d.UserAgent}} {
		if kv[1] != "" {
			_, _ = fmt.Fprintf(w, "%-9s %s\n", kv[0], kv[1])
		}
	}
	if d.InputTokens > 0 || d.OutputTokens > 0 {
		_, _ = fmt.Fprintf(w, "Tokens    %d in, %d out\n", d.InputTokens, d.OutputTokens)
	}
	limit := 1500
	if full {
		limit = 0
	}
	for _, m := range d.Conversation {
		printMessage(w, m.Role, m, limit)
	}
	if d.Reply != nil {
		printMessage(w, "reply", *d.Reply, limit)
	}
	return nil
}

func printMessage(w io.Writer, label string, m Message, limit int) {
	_, _ = fmt.Fprintf(w, "\n── %s %s\n", label, strings.Repeat("─", max(1, 60-len(label))))
	for _, p := range m.Parts {
		text := p.Text
		if limit > 0 && len(text) > limit && p.Type != "text" {
			text = text[:truncateUTF8(text, limit)] + fmt.Sprintf(" … (+%d chars, --full)", len(p.Text)-limit)
		}
		switch p.Type {
		case "text":
			_, _ = fmt.Fprintln(w, text)
		case "image":
			_, _ = fmt.Fprintln(w, "[image]")
		default:
			tag := p.Type
			if p.Name != "" {
				tag += " " + p.Name
			}
			_, _ = fmt.Fprintf(w, "[%s] %s\n", tag, text)
		}
	}
}

func truncateUTF8(s string, n int) int {
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return n
}

func cliExport(src archiveSource, f Filter, w io.Writer) error {
	enc := json.NewEncoder(w)
	remaining := f.Limit
	for {
		f.Limit = min(remaining, maxListLimit)
		page, errList := src.List(f)
		if errList != nil {
			return errList
		}
		for _, it := range page.Items {
			d, errDetail := src.Detail(it.ID)
			if errDetail != nil {
				return fmt.Errorf("%s: %w", it.ID, errDetail)
			}
			d.Conversation, d.Reply = nil, nil
			if errEncode := enc.Encode(d); errEncode != nil {
				return errEncode
			}
		}
		remaining -= len(page.Items)
		if page.Next == "" || remaining <= 0 {
			return nil
		}
		f.Before = page.Next
	}
}

func cliStats(src archiveSource, asJSON bool, w io.Writer) error {
	st, errStats := src.Stats()
	if errStats != nil {
		return errStats
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	_, _ = fmt.Fprintf(w, "Records   %d\nRaw       %s\nOn disk   %s (%.1fx smaller; %s of compressed data)\nSegments  %d\n",
		st.Records, humanBytes(st.RawBytes), humanBytes(st.DiskBytes), st.Ratio, humanBytes(st.StoredBytes), st.Segments)
	if !st.Oldest.IsZero() {
		_, _ = fmt.Fprintf(w, "Range     %s → %s\n", st.Oldest.Local().Format("2006-01-02 15:04"), st.Newest.Local().Format("2006-01-02 15:04"))
	}
	if st.Dir != "" {
		_, _ = fmt.Fprintf(w, "Dir       %s\n", st.Dir)
	}
	if st.Dropped > 0 {
		_, _ = fmt.Fprintf(w, "Dropped   %d\n", st.Dropped)
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
