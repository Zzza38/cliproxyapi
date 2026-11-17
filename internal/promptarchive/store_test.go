package promptarchive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
)

// agentTurns builds the requests an agent client sends over a conversation:
// every turn resends the full history plus a large, stable tool list.
func agentTurns(turns int, seed int64) [][]byte {
	rng := rand.New(rand.NewSource(seed))
	words := strings.Fields("the quick brown fox jumps over a lazy dog while reading files and running tests in the repo before fixing bugs")
	sentence := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = words[rng.Intn(len(words))]
		}
		return strings.Join(parts, " ")
	}
	tools := ojson.NewDocString(`{"tools":[]}`)
	for i := 0; i < 40; i++ {
		_ = tools.SetRaw("tools.-1", fmt.Sprintf(`{"name":"tool_%d","description":%q,"input_schema":{"type":"object"}}`, i, sentence(60)))
	}
	toolsRaw := tools.Get("tools").Raw

	var out [][]byte
	history := `[]`
	for turn := 0; turn < turns; turn++ {
		h := ojson.NewDocString(history)
		_ = h.SetRaw("-1", fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q}]}`, sentence(30)))
		_ = h.SetRaw("-1", fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_%d","name":"Bash","input":{"command":%q}}]}`, turn, sentence(8)))
		_ = h.SetRaw("-1", fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":%q}]}`, turn, sentence(200)))
		history = h.String()
		req := ojson.NewDocString(`{"model":"claude-test","system":"You are a test agent.","stream":true}`)
		_ = req.SetRaw("messages", history)
		_ = req.SetRaw("tools", toolsRaw)
		out = append(out, req.Bytes())
	}
	return out
}

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, errOpen := Open(dir, false)
	if errOpen != nil {
		t.Fatalf("open: %v", errOpen)
	}
	t.Cleanup(s.Close)
	return s, dir
}

func TestStoreRoundTripAndDedup(t *testing.T) {
	s, _ := openTestStore(t)
	turns := agentTurns(40, 1)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ids := make([]uint64, len(turns))
	for i, req := range turns {
		rec := &Record{
			Meta:     Meta{Time: base.Add(time.Duration(i) * time.Minute).UnixMilli(), Protocol: "claude", Model: "claude-test"},
			Request:  req,
			Response: []byte(fmt.Sprintf(`{"type":"message","content":[{"type":"text","text":"turn %d done"}]}`, i)),
		}
		if errAppend := s.Append([]*Record{rec}); errAppend != nil {
			t.Fatalf("append %d: %v", i, errAppend)
		}
		ids[i] = rec.Meta.ID
	}
	for i, id := range ids {
		got, errGet := s.Get(id)
		if errGet != nil {
			t.Fatalf("get %d: %v", i, errGet)
		}
		if !bytes.Equal(got.Request, turns[i]) {
			t.Fatalf("turn %d request mismatch", i)
		}
		if !strings.Contains(string(got.Response), fmt.Sprintf("turn %d done", i)) {
			t.Fatalf("turn %d response mismatch: %s", i, got.Response)
		}
	}
	st, errStats := s.Stats()
	if errStats != nil {
		t.Fatal(errStats)
	}
	if st.Records != int64(len(turns)) {
		t.Fatalf("records = %d", st.Records)
	}
	// A 40-turn agent conversation resends its history every turn; dedup plus
	// zstd should shrink it far beyond what per-record compression could do.
	dataRatio := float64(st.RawBytes) / float64(st.StoredBytes)
	if dataRatio < 30 {
		t.Fatalf("compression ratio %.1f too low (raw %d, stored %d)", dataRatio, st.RawBytes, st.StoredBytes)
	}
	t.Logf("raw %d bytes -> stored %d bytes (%.1fx), %d bytes on disk", st.RawBytes, st.StoredBytes, dataRatio, st.DiskBytes)
}

func TestStoreReopenAndReadOnly(t *testing.T) {
	s, dir := openTestStore(t)
	turns := agentTurns(5, 2)
	for i, req := range turns[:3] {
		if errAppend := s.Append([]*Record{{Meta: Meta{Time: int64(1_700_000_000_000 + i)}, Request: req}}); errAppend != nil {
			t.Fatal(errAppend)
		}
	}
	s.Close()

	s2, errOpen := Open(dir, false)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	for i, req := range turns[3:] {
		if errAppend := s2.Append([]*Record{{Meta: Meta{Time: int64(1_700_000_001_000 + i)}, Request: req}}); errAppend != nil {
			t.Fatal(errAppend)
		}
	}
	if _, errLocked := Open(dir, true); errLocked == nil {
		t.Fatal("read-only open should fail while a writer holds the index")
	}
	s2.Close()

	ro, errRO := Open(dir, true)
	if errRO != nil {
		t.Fatal(errRO)
	}
	defer ro.Close()
	page, errList := ro.List(Filter{Limit: 10})
	if errList != nil {
		t.Fatal(errList)
	}
	if len(page.Items) != 5 {
		t.Fatalf("items = %d", len(page.Items))
	}
	for i, item := range page.Items {
		id, _ := ParseID(item.ID)
		rec, errGet := ro.Get(id)
		if errGet != nil {
			t.Fatal(errGet)
		}
		if !bytes.Equal(rec.Request, turns[len(turns)-1-i]) {
			t.Fatalf("item %d request mismatch after reopen", i)
		}
	}
	if errAppend := ro.Append([]*Record{{Request: []byte("x")}}); errAppend == nil {
		t.Fatal("append on read-only store should fail")
	}
}

func TestStoreListFiltersAndPaging(t *testing.T) {
	s, _ := openTestStore(t)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var batch []*Record
	for i := 0; i < 30; i++ {
		meta := Meta{Time: base.Add(time.Duration(i) * time.Hour).UnixMilli(), Model: "gpt-5", Key: "sk-…aaaa", Outcome: "ok", Status: 200, Preview: fmt.Sprintf("question %d", i)}
		if i%3 == 0 {
			meta.Model = "claude-opus"
			meta.Key = "sk-…bbbb"
		}
		if i%10 == 0 {
			meta.Outcome, meta.Status = "failed", 500
		}
		if i == 29 {
			meta.Internal = true
		}
		batch = append(batch, &Record{Meta: meta, Request: []byte(fmt.Sprintf(`{"n":%d}`, i))})
	}
	if errAppend := s.Append(batch); errAppend != nil {
		t.Fatal(errAppend)
	}

	count := func(f Filter) int {
		t.Helper()
		page, errList := s.List(f)
		if errList != nil {
			t.Fatal(errList)
		}
		return len(page.Items)
	}
	if got := count(Filter{Limit: 100}); got != 29 {
		t.Fatalf("default excludes internal: got %d", got)
	}
	if got := count(Filter{Limit: 100, Internal: true}); got != 30 {
		t.Fatalf("with internal: got %d", got)
	}
	if got := count(Filter{Limit: 100, Model: "CLAUDE"}); got != 10 {
		t.Fatalf("model filter: got %d", got)
	}
	if got := count(Filter{Limit: 100, Key: "bbbb"}); got != 10 {
		t.Fatalf("key filter: got %d", got)
	}
	if got := count(Filter{Limit: 100, Status: "error"}); got != 3 {
		t.Fatalf("status filter: got %d", got)
	}
	if got := count(Filter{Limit: 100, Query: "question 1"}); got != 11 {
		t.Fatalf("query filter: got %d", got)
	}
	if got := count(Filter{Limit: 100, Since: base.Add(20 * time.Hour), Until: base.Add(25 * time.Hour)}); got != 5 {
		t.Fatalf("time range: got %d", got)
	}

	// Paging walks every record exactly once, newest first.
	seen := map[string]bool{}
	f := Filter{Limit: 7, Internal: true}
	var last time.Time
	for {
		page, errList := s.List(f)
		if errList != nil {
			t.Fatal(errList)
		}
		for _, it := range page.Items {
			if seen[it.ID] {
				t.Fatalf("duplicate %s", it.ID)
			}
			if !last.IsZero() && !it.Time.Before(last) {
				t.Fatalf("not newest first")
			}
			last = it.Time
			seen[it.ID] = true
		}
		if page.Next == "" {
			break
		}
		f.Before = page.Next
	}
	if len(seen) != 30 {
		t.Fatalf("paged %d records", len(seen))
	}
}

func TestStoreDeepSearchReturnsFirstAppearance(t *testing.T) {
	s, _ := openTestStore(t)
	turns := agentTurns(10, 3)
	marker := "ZEBRA-UNIQUE-MARKER"
	var ids []uint64
	for i, req := range turns {
		if i >= 4 {
			// From turn 4 on, the marker is part of the resent history.
			doc := ojson.NewDoc(req)
			_ = doc.Set("metadata.note", marker)
			req = doc.Bytes()
		}
		rec := &Record{Meta: Meta{Time: int64(1_700_000_000_000 + i*1000), Model: "m"}, Request: req}
		if errAppend := s.Append([]*Record{rec}); errAppend != nil {
			t.Fatal(errAppend)
		}
		ids = append(ids, rec.Meta.ID)
	}
	page, errList := s.List(Filter{Query: strings.ToLower(marker), Deep: true, Limit: 10})
	if errList != nil {
		t.Fatal(errList)
	}
	if len(page.Items) != 1 || page.Items[0].ID != FormatID(ids[4]) {
		t.Fatalf("deep search = %+v, want only %s", page.Items, FormatID(ids[4]))
	}
	page, _ = s.List(Filter{Query: "no-such-text-anywhere", Deep: true})
	if len(page.Items) != 0 {
		t.Fatalf("unexpected deep hits: %+v", page.Items)
	}
}

func TestStoreRotationAndPrune(t *testing.T) {
	s, dir := openTestStore(t)
	s.segmentBytes = 2048
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	rng := rand.New(rand.NewSource(4))
	noise := func() []byte {
		b := make([]byte, 4096)
		for i := range b {
			b[i] = byte('a' + rng.Intn(26))
		}
		return b
	}
	for i := 0; i < 6; i++ {
		rec := &Record{Meta: Meta{Time: now.Add(time.Duration(i-10) * 24 * time.Hour).UnixMilli()}, Request: noise()}
		if errAppend := s.Append([]*Record{rec}); errAppend != nil {
			t.Fatal(errAppend)
		}
	}
	st, _ := s.Stats()
	if st.Segments < 3 {
		t.Fatalf("expected rotation, got %d segments", st.Segments)
	}

	// Keep roughly two segments' worth.
	s.SetLimits(st.StoredBytes/3, 0)
	if errAppend := s.Append([]*Record{{Meta: Meta{Time: now.UnixMilli()}, Request: noise()}}); errAppend != nil {
		t.Fatal(errAppend)
	}
	after, _ := s.Stats()
	if after.Segments >= st.Segments+1 || after.Records >= 7 {
		t.Fatalf("prune did not drop segments: before %+v after %+v", st, after)
	}
	page, errList := s.List(Filter{Limit: 100})
	if errList != nil {
		t.Fatal(errList)
	}
	for _, it := range page.Items {
		id, _ := ParseID(it.ID)
		if _, errGet := s.Get(id); errGet != nil {
			t.Fatalf("surviving record %s unreadable: %v", it.ID, errGet)
		}
	}
	files, _ := os.ReadDir(dir)
	packs := 0
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "packs-") {
			packs++
		}
	}
	if packs != after.Segments {
		t.Fatalf("%d segment files for %d segments", packs, after.Segments)
	}

	// Age limit drops everything but the current segment.
	s.SetLimits(0, 3*24*time.Hour)
	if errAppend := s.Append([]*Record{{Meta: Meta{Time: now.UnixMilli()}, Request: []byte("{}")}}); errAppend != nil {
		t.Fatal(errAppend)
	}
	page, _ = s.List(Filter{Limit: 100})
	for _, it := range page.Items {
		if it.Time.Before(now.Add(-3 * 24 * time.Hour)) {
			t.Fatalf("record %s older than max age survived", it.ID)
		}
	}
}

// The dictionary is part of the on-disk format; editing it breaks old packs.
func TestPackDictionaryIsFrozen(t *testing.T) {
	sum := sha256.Sum256([]byte(packDictV1))
	const want = "fd9d403fb87f713586899bac874bcc7c74b374151d9d79fa5ae1217fa9a277cd"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("packDictV1 changed: sha256 %s, want %s. Add a new dictionary ID instead.", got, want)
	}
}

func TestSplitChunksIsLosslessAndBounded(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	data := make([]byte, 300_000)
	rng.Read(data)
	var joined []byte
	splitChunks(data, func(c []byte) {
		if len(c) > maxChunkSize {
			t.Fatalf("chunk %d > max", len(c))
		}
		joined = append(joined, c...)
	})
	if !bytes.Equal(joined, data) {
		t.Fatal("chunks do not reassemble")
	}
	// An insertion near the front must not change chunks far behind it.
	shifted := append(append([]byte("INSERTED"), data[:100]...), data[100:]...)
	set := map[chunkHash]bool{}
	splitChunks(data, func(c []byte) { set[hashChunk(c)] = true })
	shared := 0
	total := 0
	splitChunks(shifted, func(c []byte) {
		total++
		if set[hashChunk(c)] {
			shared++
		}
	})
	if shared < total-3 {
		t.Fatalf("content-defined chunking did not resync: %d/%d shared", shared, total)
	}
}
