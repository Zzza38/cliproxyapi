package quotahistory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistsQuotaAndTokensAcrossRestart(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s, err := Open(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	points := []Point{{Credential: "a.json", Provider: "codex", At: now.Add(-time.Minute).UnixMilli(), Used: 20, Reset: now.Add(time.Hour).UnixMilli()}, {Credential: "a.json", Provider: "codex", At: now.UnixMilli(), Used: 21, Reset: now.Add(time.Hour).UnixMilli()}}
	if n, err := s.Record(points, now); err != nil || n != 2 {
		t.Fatalf("record=%d %v", n, err)
	}
	points[0].Used = 99
	tokens := []TokenPoint{{ID: "request-a", Provider: "codex", At: now.UnixMilli(), Input: 100, Output: 10, CacheRead: 60, Total: 110}}
	if n, err := s.RecordTokens(tokens, now); err != nil || n != 1 {
		t.Fatalf("tokens=%d %v", n, err)
	}
	if n, err := s.RecordTokens(tokens, now); err != nil || n != 0 {
		t.Fatalf("duplicate=%d %v", n, err)
	}
	restarted, err := Open(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	h := restarted.History(map[string]bool{"codex:a.json": true}, now)
	readings := h.Credentials["codex:a.json"].Series["weekly"].Readings
	if len(readings) != 2 || readings[0][1] != 20 {
		t.Fatalf("readings=%v", readings)
	}
	counts := restarted.Tokens(now.Add(-time.Hour).UnixMilli(), now.Add(time.Second).UnixMilli())["codex"]
	if counts.Total != 110 || counts.Input != 100 || counts.CacheRead != 60 || counts.Requests != 1 {
		t.Fatalf("counts=%+v", counts)
	}
	if len(restarted.History(map[string]bool{}, now).Credentials) != 0 {
		t.Fatal("deleted credentials were exposed")
	}
}
func TestStoreImportsOnlyKnownWeeklyHistoryAndDeduplicates(t *testing.T) {
	now := time.Now()
	s, err := Open(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	h := History{Version: 1, Credentials: map[string]*Credential{"claude:a.json": {Provider: "claude", Series: map[string]*Series{"seven-day": {WindowMinutes: 10080, Readings: []Reading{{float64(now.UnixMilli()), 35, float64(now.Add(time.Hour).UnixMilli())}}}}}, "codex:unknown.json": {Provider: "codex", Series: map[string]*Series{}}}}
	live := map[string]bool{"claude:a.json": true}
	if n, err := s.Import(h, live, now); err != nil || n != 1 {
		t.Fatalf("import=%d %v", n, err)
	}
	if n, err := s.Import(h, live, now); err != nil || n != 0 {
		t.Fatalf("duplicate=%d %v", n, err)
	}
	if got := s.History(live, now); len(got.Credentials) != 1 {
		t.Fatalf("history=%+v", got)
	}
}
func TestStoreRecoversPartialWriteAndPrunesOldDays(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s, err := Open(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	point := Point{Credential: "a", Provider: "codex", At: now.UnixMilli(), Used: 1, Reset: now.Add(time.Hour).UnixMilli()}
	if _, err := s.Record([]Point{point}, now); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "2026-11-06.jsonl")
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"quota":`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	point.At = now.Add(time.Minute).UnixMilli()
	if _, err := s.Record([]Point{point}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.History(map[string]bool{"codex:a": true}, now.Add(time.Minute)).Credentials["codex:a"].Series["weekly"].Readings); got != 2 {
		t.Fatalf("got %d records", got)
	}
	s, err = Open(dir, now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.History(map[string]bool{"codex:a": true}, now.Add(30*24*time.Hour)).Credentials) != 0 {
		t.Fatal("expired data survived")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("old file not pruned: %v", err)
	}
}
