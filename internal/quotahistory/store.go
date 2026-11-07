// Package quotahistory stores quota observations and token totals without credentials or prompts.
package quotahistory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const Retention = 28 * 24 * time.Hour

type Point struct {
	Credential string  `json:"credential"`
	Provider   string  `json:"provider"`
	At         int64   `json:"at"`
	Used       float64 `json:"used"`
	Reset      int64   `json:"reset"`
}
type TokenPoint struct {
	ID         string `json:"id"`
	Provider   string `json:"provider"`
	At         int64  `json:"at"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cacheRead"`
	CacheWrite int64  `json:"cacheWrite"`
	Total      int64  `json:"total"`
}
type Reading [3]float64
type Series struct {
	WindowMinutes int       `json:"windowMinutes"`
	Readings      []Reading `json:"readings"`
}
type Credential struct {
	Provider string             `json:"provider"`
	Series   map[string]*Series `json:"series"`
}
type History struct {
	Enabled     bool                   `json:"enabled"`
	Version     int                    `json:"version"`
	StartedAt   *int64                 `json:"startedAtMs"`
	Credentials map[string]*Credential `json:"credentials"`
}
type TokenTotals struct {
	Requests   int64 `json:"requests"`
	Input      int64 `json:"inputTokens"`
	Output     int64 `json:"outputTokens"`
	CacheRead  int64 `json:"cacheReadTokens"`
	CacheWrite int64 `json:"cacheWriteTokens"`
	Total      int64 `json:"totalTokens"`
}
type event struct {
	Quota  *Point      `json:"quota,omitempty"`
	Tokens *TokenPoint `json:"tokens,omitempty"`
}
type Store struct {
	mu     sync.Mutex
	dir    string
	events []event
	seen   map[string]bool
	day    string
}

func Open(dir string, now time.Time) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, seen: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		day, errDay := time.Parse("2006-01-02.jsonl", entry.Name())
		if errDay != nil || entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if day.Add(24 * time.Hour).Before(now.Add(-Retention)) {
			if errRemove := os.Remove(path); errRemove != nil {
				return nil, errRemove
			}
			continue
		}
		data, errRead := os.ReadFile(path)
		if errRead != nil {
			return nil, errRead
		}
		// A crash may leave a partial final record. Remove only that incomplete tail.
		if len(data) > 0 && data[len(data)-1] != '\n' {
			end := bytes.LastIndexByte(data, '\n') + 1
			if errTruncate := os.Truncate(path, int64(end)); errTruncate != nil {
				return nil, errTruncate
			}
			data = data[:end]
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 1024), 1<<20)
		for scanner.Scan() {
			var e event
			if errJSON := json.Unmarshal(scanner.Bytes(), &e); errJSON != nil {
				return nil, fmt.Errorf("read quota history: %w", errJSON)
			}
			if valid(e, now) && !s.seen[key(e)] {
				s.events = append(s.events, e)
				s.seen[key(e)] = true
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			return nil, errScan
		}
	}
	return s, nil
}
func at(e event) int64 {
	if e.Quota != nil {
		return e.Quota.At
	}
	if e.Tokens != nil {
		return e.Tokens.At
	}
	return 0
}
func key(e event) string {
	if e.Quota != nil {
		return fmt.Sprintf("q:%s:%s:%d", e.Quota.Provider, e.Quota.Credential, e.Quota.At)
	}
	return "t:" + e.Tokens.ID
}
func valid(e event, now time.Time) bool {
	stamp := time.UnixMilli(at(e))
	if stamp.Before(now.Add(-Retention)) || stamp.After(now.Add(time.Minute)) {
		return false
	}
	if q := e.Quota; q != nil {
		return e.Tokens == nil && (q.Provider == "claude" || q.Provider == "codex") && q.Credential != "" && len(q.Credential) < 512 && !math.IsNaN(q.Used) && !math.IsInf(q.Used, 0) && q.Used >= 0 && q.Used <= 100 && q.Reset > q.At
	}
	t := e.Tokens
	return t != nil && t.ID != "" && len(t.ID) <= 256 && len(t.Provider) <= 64 && t.Input >= 0 && t.Output >= 0 && t.CacheRead >= 0 && t.CacheWrite >= 0 && t.Total >= 0
}
func (s *Store) append(events []event, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	groups := map[string][]event{}
	pending := map[string]bool{}
	for _, e := range events {
		if !valid(e, now) || s.seen[key(e)] || pending[key(e)] {
			continue
		}
		pending[key(e)] = true
		day := time.UnixMilli(at(e)).UTC().Format("2006-01-02")
		groups[day] = append(groups[day], e)
	}
	count := 0
	for day, batch := range groups {
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		for _, e := range batch {
			if err := encoder.Encode(e); err != nil {
				return count, err
			}
		}
		f, err := os.OpenFile(filepath.Join(s.dir, day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return count, err
		}
		stat, errStat := f.Stat()
		if errStat != nil {
			return count, errors.Join(errStat, f.Close())
		}
		n, errWrite := f.Write(buffer.Bytes())
		if n != buffer.Len() && errWrite == nil {
			errWrite = io.ErrShortWrite
		}
		errSync := f.Sync()
		if errWrite != nil || errSync != nil {
			errTruncate := f.Truncate(stat.Size())
			errRollback := f.Sync()
			return count, errors.Join(errWrite, errSync, errTruncate, errRollback, f.Close())
		}
		errClose := f.Close()
		if errWrite != nil {
			return count, errWrite
		}
		if errSync != nil {
			return count, errSync
		}
		if errClose != nil {
			return count, errClose
		}
		for _, e := range batch {
			s.events = append(s.events, e)
			s.seen[key(e)] = true
			count++
		}
	}
	today := now.UTC().Format("2006-01-02")
	if s.day != today {
		s.day = today
		kept := s.events[:0]
		s.seen = map[string]bool{}
		for _, e := range s.events {
			if valid(e, now) {
				kept = append(kept, e)
				s.seen[key(e)] = true
			}
		}
		s.events = kept
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return count, err
		}
		for _, entry := range entries {
			day, errParse := time.Parse("2006-01-02.jsonl", entry.Name())
			if errParse == nil && day.Add(24*time.Hour).Before(now.Add(-Retention)) {
				if errRemove := os.Remove(filepath.Join(s.dir, entry.Name())); errRemove != nil {
					return count, errRemove
				}
			}
		}
	}
	return count, nil
}
func (s *Store) Record(points []Point, now time.Time) (int, error) {
	events := make([]event, len(points))
	for i := range points {
		point := points[i]
		events[i].Quota = &point
	}
	return s.append(events, now)
}
func (s *Store) RecordTokens(points []TokenPoint, now time.Time) (int, error) {
	events := make([]event, len(points))
	for i := range points {
		point := points[i]
		events[i].Tokens = &point
	}
	return s.append(events, now)
}
func (s *Store) History(live map[string]bool, now time.Time) History {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := History{Enabled: true, Version: 1, Credentials: map[string]*Credential{}}
	for _, e := range s.events {
		q := e.Quota
		if q == nil || !valid(e, now) {
			continue
		}
		id := q.Provider + ":" + q.Credential
		if !live[id] {
			continue
		}
		c := h.Credentials[id]
		if c == nil {
			c = &Credential{Provider: q.Provider, Series: map[string]*Series{}}
			h.Credentials[id] = c
		}
		window := "weekly"
		if q.Provider == "claude" {
			window = "seven-day"
		}
		series := c.Series[window]
		if series == nil {
			series = &Series{WindowMinutes: 10080, Readings: []Reading{}}
			c.Series[window] = series
		}
		series.Readings = append(series.Readings, Reading{float64(q.At), q.Used, float64(q.Reset)})
		if h.StartedAt == nil || q.At < *h.StartedAt {
			a := q.At
			h.StartedAt = &a
		}
	}
	for _, c := range h.Credentials {
		for _, series := range c.Series {
			sort.Slice(series.Readings, func(i, j int) bool { return series.Readings[i][0] < series.Readings[j][0] })
		}
	}
	return h
}
func (s *Store) Import(h History, live map[string]bool, now time.Time) (int, error) {
	points := []Point{}
	for id, c := range h.Credentials {
		if c == nil || !live[id] || !strings.HasPrefix(id, c.Provider+":") {
			continue
		}
		window := "weekly"
		if c.Provider == "claude" {
			window = "seven-day"
		}
		series := c.Series[window]
		if series == nil || series.WindowMinutes != 10080 {
			continue
		}
		for _, r := range series.Readings {
			points = append(points, Point{Credential: strings.TrimPrefix(id, c.Provider+":"), Provider: c.Provider, At: int64(r[0]), Used: r[1], Reset: int64(r[2])})
			if len(points) > 100000 {
				return 0, fmt.Errorf("too many readings")
			}
		}
	}
	return s.Record(points, now)
}
func (s *Store) Tokens(from, to int64) map[string]*TokenTotals {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[string]*TokenTotals{}
	for _, e := range s.events {
		t := e.Tokens
		if t == nil || t.At < from || t.At >= to {
			continue
		}
		v := result[t.Provider]
		if v == nil {
			v = &TokenTotals{}
			result[t.Provider] = v
		}
		v.Requests++
		v.Input += t.Input
		v.Output += t.Output
		v.CacheRead += t.CacheRead
		v.CacheWrite += t.CacheWrite
		v.Total += t.Total
	}
	return result
}
