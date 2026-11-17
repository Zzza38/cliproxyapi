package promptarchive

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
	"go.etcd.io/bbolt"
)

const (
	indexFileName       = "index.db"
	recordFormatVersion = 1
	maxSegmentBytes     = 256 << 20
	minSegmentBytes     = 4 << 20
	packCacheMaxBytes   = 64 << 20
	defaultListLimit    = 50
	maxListLimit        = 1000
	maxListScan         = 200000
	defaultDeepScanMB   = 4096
)

var (
	bucketMeta = []byte("meta")
	bucketRec  = []byte("rec")
	bucketPack = []byte("pack")
	bucketHash = []byte("hash")
	bucketSeg  = []byte("seg")

	keyCurrentSeg = []byte("current-seg")

	// ErrNotFound is returned when a record ID does not exist.
	ErrNotFound = errors.New("prompt archive: record not found")
	errReadOnly = errors.New("prompt archive: store is read-only")
)

type span struct {
	pack uint64
	off  uint64
	n    uint64
}

type segInfo struct {
	FirstPack uint64 `json:"p"`
	MaxRecID  uint64 `json:"r,omitempty"`
	Records   int64  `json:"n,omitempty"`
	RawBytes  int64  `json:"raw,omitempty"`
	Stored    int64  `json:"st,omitempty"`
	Created   int64  `json:"c"`
	Oldest    int64  `json:"o,omitempty"`
	Newest    int64  `json:"t,omitempty"`
}

type packLoc struct {
	seg    uint32
	off    int64
	clen   int
	rawLen int
	recID  uint64
}

// Store is the on-disk archive. Append must only be called from one goroutine;
// read methods are safe for concurrent use.
type Store struct {
	dir      string
	db       *bbolt.DB
	readOnly bool
	enc      *zstd.Encoder
	dec      *zstd.Decoder
	now      func() time.Time

	filesMu sync.Mutex
	files   map[uint32]*os.File

	cacheMu    sync.Mutex
	cacheLRU   *list.List
	cacheIndex map[uint64]*list.Element
	cacheBytes int

	limitsMu sync.Mutex
	maxBytes int64
	maxAge   time.Duration
	// segmentBytes overrides the computed segment size (tests only).
	segmentBytes int64

	// Writer-only state.
	wFile *os.File
	wSeg  uint32
	wSize int64
}

type cacheEntry struct {
	id   uint64
	data []byte
}

// Open opens or creates an archive in dir. A read-only open fails fast when a
// running server holds the index lock.
func Open(dir string, readOnly bool) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("prompt archive: empty directory")
	}
	opts := &bbolt.Options{Timeout: 2 * time.Second}
	if readOnly {
		if _, errStat := os.Stat(filepath.Join(dir, indexFileName)); errStat != nil {
			return nil, fmt.Errorf("prompt archive: open %s: %w", dir, errStat)
		}
		opts = &bbolt.Options{ReadOnly: true, Timeout: 300 * time.Millisecond}
	} else if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return nil, fmt.Errorf("prompt archive: create %s: %w", dir, errMkdir)
	}
	db, errOpen := bbolt.Open(filepath.Join(dir, indexFileName), 0o600, opts)
	if errOpen != nil {
		return nil, fmt.Errorf("prompt archive: open index: %w", errOpen)
	}
	dict := []byte(packDictV1)
	enc, errEnc := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderDictRaw(packDictID, dict))
	if errEnc != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prompt archive: zstd encoder: %w", errEnc)
	}
	dec, errDec := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(0),
		zstd.WithDecoderMaxMemory(1<<30),
		zstd.WithDecoderDictRaw(packDictID, dict))
	if errDec != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prompt archive: zstd decoder: %w", errDec)
	}
	s := &Store{
		dir:        dir,
		db:         db,
		readOnly:   readOnly,
		enc:        enc,
		dec:        dec,
		now:        time.Now,
		files:      make(map[uint32]*os.File),
		cacheLRU:   list.New(),
		cacheIndex: make(map[uint64]*list.Element),
	}
	if !readOnly {
		if errInit := s.initWriter(); errInit != nil {
			s.Close()
			return nil, errInit
		}
	}
	return s, nil
}

// Dir returns the archive directory.
func (s *Store) Dir() string { return s.dir }

// SetLimits updates pruning limits. Zero disables a limit.
func (s *Store) SetLimits(maxBytes int64, maxAge time.Duration) {
	s.limitsMu.Lock()
	s.maxBytes, s.maxAge = maxBytes, maxAge
	s.limitsMu.Unlock()
}

func (s *Store) limits() (int64, time.Duration) {
	s.limitsMu.Lock()
	defer s.limitsMu.Unlock()
	return s.maxBytes, s.maxAge
}

// Close releases all resources.
func (s *Store) Close() {
	if s == nil {
		return
	}
	if s.wFile != nil {
		if errClose := s.wFile.Close(); errClose != nil {
			log.Warnf("prompt archive: close segment: %v", errClose)
		}
		s.wFile = nil
	}
	s.filesMu.Lock()
	for seg, f := range s.files {
		_ = f.Close()
		delete(s.files, seg)
	}
	s.filesMu.Unlock()
	if s.enc != nil {
		_ = s.enc.Close()
	}
	if s.dec != nil {
		s.dec.Close()
	}
	if s.db != nil {
		if errClose := s.db.Close(); errClose != nil {
			log.Warnf("prompt archive: close index: %v", errClose)
		}
	}
}

func segmentPath(dir string, seg uint32) string {
	return filepath.Join(dir, fmt.Sprintf("packs-%06d.bin", seg))
}

func u64Key(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

func u32Key(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

func (s *Store) initWriter() error {
	errUpdate := s.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketMeta, bucketRec, bucketPack, bucketHash, bucketSeg} {
			if _, errCreate := tx.CreateBucketIfNotExists(name); errCreate != nil {
				return errCreate
			}
		}
		meta := tx.Bucket(bucketMeta)
		if raw := meta.Get(keyCurrentSeg); len(raw) == 4 {
			s.wSeg = binary.BigEndian.Uint32(raw)
			return nil
		}
		s.wSeg = 1
		if errPut := putSeg(tx, s.wSeg, &segInfo{FirstPack: 1, Created: s.now().UnixMilli()}); errPut != nil {
			return errPut
		}
		return meta.Put(keyCurrentSeg, u32Key(s.wSeg))
	})
	if errUpdate != nil {
		return fmt.Errorf("prompt archive: init index: %w", errUpdate)
	}
	return s.openSegmentWriter(s.wSeg)
}

func (s *Store) openSegmentWriter(seg uint32) error {
	f, errOpen := os.OpenFile(segmentPath(s.dir, seg), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		return fmt.Errorf("prompt archive: open segment: %w", errOpen)
	}
	info, errStat := f.Stat()
	if errStat != nil {
		_ = f.Close()
		return fmt.Errorf("prompt archive: stat segment: %w", errStat)
	}
	if s.wFile != nil {
		_ = s.wFile.Close()
	}
	s.wFile, s.wSeg, s.wSize = f, seg, info.Size()
	return nil
}

func getSeg(tx *bbolt.Tx, seg uint32) (*segInfo, error) {
	raw := tx.Bucket(bucketSeg).Get(u32Key(seg))
	if raw == nil {
		return nil, fmt.Errorf("prompt archive: segment %d missing", seg)
	}
	info := &segInfo{}
	if errUnmarshal := json.Unmarshal(raw, info); errUnmarshal != nil {
		return nil, fmt.Errorf("prompt archive: segment %d: %w", seg, errUnmarshal)
	}
	return info, nil
}

func putSeg(tx *bbolt.Tx, seg uint32, info *segInfo) error {
	raw, errMarshal := json.Marshal(info)
	if errMarshal != nil {
		return errMarshal
	}
	return tx.Bucket(bucketSeg).Put(u32Key(seg), raw)
}

func (s *Store) segmentLimit() int64 {
	if s.segmentBytes > 0 {
		return s.segmentBytes
	}
	maxBytes, _ := s.limits()
	limit := int64(maxSegmentBytes)
	if maxBytes > 0 {
		limit = maxBytes / 4
		if limit < minSegmentBytes {
			limit = minSegmentBytes
		}
		if limit > maxSegmentBytes {
			limit = maxSegmentBytes
		}
	}
	return limit
}

// Append stores a batch of records in one index transaction.
func (s *Store) Append(batch []*Record) error {
	if s.readOnly {
		return errReadOnly
	}
	if len(batch) == 0 {
		return nil
	}
	if errRotate := s.maybeRotate(); errRotate != nil {
		log.Warnf("prompt archive: rotate segment: %v", errRotate)
	}
	if errPrune := s.prune(); errPrune != nil {
		log.Warnf("prompt archive: prune: %v", errPrune)
	}
	errUpdate := s.db.Update(func(tx *bbolt.Tx) error {
		info, errSeg := getSeg(tx, s.wSeg)
		if errSeg != nil {
			return errSeg
		}
		for _, rec := range batch {
			if errAppend := s.appendOne(tx, info, rec); errAppend != nil {
				return errAppend
			}
		}
		if errPut := putSeg(tx, s.wSeg, info); errPut != nil {
			return errPut
		}
		// Make pack bytes durable before the index that points at them.
		return s.wFile.Sync()
	})
	if errUpdate != nil {
		s.resyncWriterSize()
	}
	return errUpdate
}

func (s *Store) resyncWriterSize() {
	if s.wFile == nil {
		return
	}
	if info, errStat := s.wFile.Stat(); errStat == nil {
		s.wSize = info.Size()
	}
}

func (s *Store) appendOne(tx *bbolt.Tx, info *segInfo, rec *Record) error {
	recB, packB, hashB := tx.Bucket(bucketRec), tx.Bucket(bucketPack), tx.Bucket(bucketHash)

	if rec.Meta.Time == 0 {
		rec.Meta.Time = s.now().UnixMilli()
	}
	id := uint64(rec.Meta.Time) * 1000
	if id == 0 {
		id = 1
	}
	for recB.Get(u64Key(id)) != nil {
		id++
	}
	rec.Meta.ID = id
	rec.Meta.ReqBytes, rec.Meta.RespBytes = len(rec.Request), len(rec.Response)

	packID := packB.Sequence() + 1
	var fresh []byte
	type newHash struct {
		h  chunkHash
		sp span
	}
	var added []newHash
	local := make(map[chunkHash]span)
	encodeBlob := func(blob []byte) []span {
		var spans []span
		splitChunks(blob, func(chunk []byte) {
			h := hashChunk(chunk)
			sp, ok := local[h]
			if !ok {
				if raw := hashB.Get(h[:]); raw != nil {
					sp, ok = decodeHashValue(raw)
				}
			}
			if !ok || sp.n != uint64(len(chunk)) {
				sp = span{pack: packID, off: uint64(len(fresh)), n: uint64(len(chunk))}
				fresh = append(fresh, chunk...)
				added = append(added, newHash{h: h, sp: sp})
			}
			local[h] = sp
			spans = appendSpan(spans, sp)
		})
		return spans
	}
	reqSpans := encodeBlob(rec.Request)
	respSpans := encodeBlob(rec.Response)

	stored := 0
	if len(fresh) > 0 {
		compressed := s.enc.EncodeAll(fresh, nil)
		offset := s.wSize
		if _, errWrite := s.wFile.Write(compressed); errWrite != nil {
			return fmt.Errorf("prompt archive: write pack: %w", errWrite)
		}
		s.wSize += int64(len(compressed))
		if errSeq := packB.SetSequence(packID); errSeq != nil {
			return errSeq
		}
		loc := packLoc{seg: s.wSeg, off: offset, clen: len(compressed), rawLen: len(fresh), recID: id}
		if errPut := packB.Put(u64Key(packID), encodePackLoc(loc)); errPut != nil {
			return errPut
		}
		for _, item := range added {
			if errPut := hashB.Put(item.h[:], encodeHashValue(item.sp)); errPut != nil {
				return errPut
			}
		}
		stored += len(compressed)
	}

	metaJSON, errMarshal := json.Marshal(&rec.Meta)
	if errMarshal != nil {
		return errMarshal
	}
	value := encodeRecord(s.wSeg, metaJSON, reqSpans, respSpans)
	if errPut := recB.Put(u64Key(id), value); errPut != nil {
		return errPut
	}
	stored += len(value)

	info.Records++
	info.RawBytes += int64(len(rec.Request) + len(rec.Response))
	info.Stored += int64(stored)
	if id > info.MaxRecID {
		info.MaxRecID = id
	}
	if info.Oldest == 0 || rec.Meta.Time < info.Oldest {
		info.Oldest = rec.Meta.Time
	}
	if rec.Meta.Time > info.Newest {
		info.Newest = rec.Meta.Time
	}
	return nil
}

func appendSpan(spans []span, sp span) []span {
	if n := len(spans); n > 0 {
		last := &spans[n-1]
		if last.pack == sp.pack && last.off+last.n == sp.off {
			last.n += sp.n
			return spans
		}
	}
	return append(spans, sp)
}

func encodeHashValue(sp span) []byte {
	buf := make([]byte, 0, 3*binary.MaxVarintLen64)
	buf = binary.AppendUvarint(buf, sp.pack)
	buf = binary.AppendUvarint(buf, sp.off)
	return binary.AppendUvarint(buf, sp.n)
}

func decodeHashValue(raw []byte) (span, bool) {
	r := varintReader{buf: raw}
	sp := span{pack: r.uvarint(), off: r.uvarint(), n: r.uvarint()}
	return sp, r.err == nil
}

func encodePackLoc(loc packLoc) []byte {
	buf := make([]byte, 0, 5*binary.MaxVarintLen64)
	buf = binary.AppendUvarint(buf, uint64(loc.seg))
	buf = binary.AppendUvarint(buf, uint64(loc.off))
	buf = binary.AppendUvarint(buf, uint64(loc.clen))
	buf = binary.AppendUvarint(buf, uint64(loc.rawLen))
	return binary.AppendUvarint(buf, loc.recID)
}

func decodePackLoc(raw []byte) (packLoc, error) {
	r := varintReader{buf: raw}
	loc := packLoc{
		seg:    uint32(r.uvarint()),
		off:    int64(r.uvarint()),
		clen:   int(r.uvarint()),
		rawLen: int(r.uvarint()),
		recID:  r.uvarint(),
	}
	return loc, r.err
}

func encodeRecord(seg uint32, metaJSON []byte, req, resp []span) []byte {
	buf := make([]byte, 0, 16+len(metaJSON)+(len(req)+len(resp))*8)
	buf = append(buf, recordFormatVersion)
	buf = binary.AppendUvarint(buf, uint64(seg))
	buf = binary.AppendUvarint(buf, uint64(len(metaJSON)))
	buf = append(buf, metaJSON...)
	var prev uint64
	for _, spans := range [][]span{req, resp} {
		buf = binary.AppendUvarint(buf, uint64(len(spans)))
		for _, sp := range spans {
			buf = binary.AppendVarint(buf, int64(sp.pack-prev))
			buf = binary.AppendUvarint(buf, sp.off)
			buf = binary.AppendUvarint(buf, sp.n)
			prev = sp.pack
		}
	}
	return buf
}

type decodedRecord struct {
	seg       uint32
	meta      Meta
	req, resp []span
}

func decodeRecordHeader(raw []byte) (seg uint32, metaJSON []byte, rest []byte, err error) {
	if len(raw) == 0 || raw[0] != recordFormatVersion {
		return 0, nil, nil, fmt.Errorf("prompt archive: unsupported record format")
	}
	r := varintReader{buf: raw[1:]}
	seg = uint32(r.uvarint())
	n := r.uvarint()
	if r.err != nil || n > uint64(len(r.buf)) {
		return 0, nil, nil, fmt.Errorf("prompt archive: corrupt record header")
	}
	return seg, r.buf[:n], r.buf[n:], nil
}

func decodeRecord(id uint64, raw []byte, withSpans bool) (*decodedRecord, error) {
	seg, metaJSON, rest, errHeader := decodeRecordHeader(raw)
	if errHeader != nil {
		return nil, errHeader
	}
	out := &decodedRecord{seg: seg}
	if errUnmarshal := json.Unmarshal(metaJSON, &out.meta); errUnmarshal != nil {
		return nil, fmt.Errorf("prompt archive: record meta: %w", errUnmarshal)
	}
	out.meta.ID = id
	if !withSpans {
		return out, nil
	}
	r := varintReader{buf: rest}
	var prev uint64
	readSpans := func() []span {
		count := r.uvarint()
		if r.err != nil || count > uint64(len(r.buf)) {
			r.err = fmt.Errorf("prompt archive: corrupt span list")
			return nil
		}
		spans := make([]span, 0, count)
		for i := uint64(0); i < count && r.err == nil; i++ {
			pack := prev + uint64(r.varint())
			sp := span{pack: pack, off: r.uvarint(), n: r.uvarint()}
			prev = pack
			spans = append(spans, sp)
		}
		return spans
	}
	out.req = readSpans()
	out.resp = readSpans()
	if r.err != nil {
		return nil, r.err
	}
	return out, nil
}

type varintReader struct {
	buf []byte
	err error
}

func (r *varintReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.buf)
	if n <= 0 {
		r.err = fmt.Errorf("prompt archive: bad varint")
		return 0
	}
	r.buf = r.buf[n:]
	return v
}

func (r *varintReader) varint() int64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Varint(r.buf)
	if n <= 0 {
		r.err = fmt.Errorf("prompt archive: bad varint")
		return 0
	}
	r.buf = r.buf[n:]
	return v
}

// maybeRotate starts a new segment when the current one is full or, with an
// age limit, old enough that pruning would otherwise never reach it.
func (s *Store) maybeRotate() error {
	_, maxAge := s.limits()
	rotate := s.wSize >= s.segmentLimit()
	var nextFirstPack uint64
	errView := s.db.View(func(tx *bbolt.Tx) error {
		info, errSeg := getSeg(tx, s.wSeg)
		if errSeg != nil {
			return errSeg
		}
		if maxAge > 0 && info.Records > 0 && s.now().Sub(time.UnixMilli(info.Created)) > maxAge/4 {
			rotate = true
		}
		nextFirstPack = tx.Bucket(bucketPack).Sequence() + 1
		return nil
	})
	if errView != nil || !rotate {
		return errView
	}
	next := s.wSeg + 1
	errUpdate := s.db.Update(func(tx *bbolt.Tx) error {
		if errDelete := tx.DeleteBucket(bucketHash); errDelete != nil && !errors.Is(errDelete, bbolt.ErrBucketNotFound) {
			return errDelete
		}
		if _, errCreate := tx.CreateBucket(bucketHash); errCreate != nil {
			return errCreate
		}
		if errPut := putSeg(tx, next, &segInfo{FirstPack: nextFirstPack, Created: s.now().UnixMilli()}); errPut != nil {
			return errPut
		}
		return tx.Bucket(bucketMeta).Put(keyCurrentSeg, u32Key(next))
	})
	if errUpdate != nil {
		return errUpdate
	}
	return s.openSegmentWriter(next)
}

// prune drops whole segments, oldest first, to honor the size and age limits.
func (s *Store) prune() error {
	maxBytes, maxAge := s.limits()
	if maxBytes <= 0 && maxAge <= 0 {
		return nil
	}
	type segEntry struct {
		seg  uint32
		info segInfo
	}
	var segs []segEntry
	errView := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketSeg).ForEach(func(k, v []byte) error {
			var info segInfo
			if errUnmarshal := json.Unmarshal(v, &info); errUnmarshal != nil {
				return errUnmarshal
			}
			segs = append(segs, segEntry{seg: binary.BigEndian.Uint32(k), info: info})
			return nil
		})
	})
	if errView != nil {
		return errView
	}
	var total int64
	for _, e := range segs {
		total += e.info.Stored
	}
	cutoff := s.now().Add(-maxAge).UnixMilli()
	for _, e := range segs {
		if e.seg >= s.wSeg {
			break
		}
		tooBig := maxBytes > 0 && total > maxBytes
		tooOld := maxAge > 0 && e.info.Newest < cutoff
		if !tooBig && !tooOld {
			break
		}
		if errDrop := s.dropSegment(e.seg, e.info); errDrop != nil {
			return errDrop
		}
		total -= e.info.Stored
		log.Infof("prompt archive: pruned segment %d (%d records)", e.seg, e.info.Records)
	}
	return nil
}

func (s *Store) dropSegment(seg uint32, info segInfo) error {
	errUpdate := s.db.Update(func(tx *bbolt.Tx) error {
		var recKeys, packKeys [][]byte
		c := tx.Bucket(bucketRec).Cursor()
		for k, v := c.First(); k != nil && binary.BigEndian.Uint64(k) <= info.MaxRecID; k, v = c.Next() {
			if recSeg, _, _, errHeader := decodeRecordHeader(v); errHeader != nil || recSeg <= seg {
				recKeys = append(recKeys, append([]byte(nil), k...))
			}
		}
		pc := tx.Bucket(bucketPack).Cursor()
		for k, v := pc.First(); k != nil; k, v = pc.Next() {
			loc, errLoc := decodePackLoc(v)
			if errLoc == nil && loc.seg > seg {
				break
			}
			packKeys = append(packKeys, append([]byte(nil), k...))
		}
		for _, k := range recKeys {
			if errDelete := tx.Bucket(bucketRec).Delete(k); errDelete != nil {
				return errDelete
			}
		}
		for _, k := range packKeys {
			if errDelete := tx.Bucket(bucketPack).Delete(k); errDelete != nil {
				return errDelete
			}
		}
		return tx.Bucket(bucketSeg).Delete(u32Key(seg))
	})
	if errUpdate != nil {
		return errUpdate
	}
	s.filesMu.Lock()
	if f := s.files[seg]; f != nil {
		_ = f.Close()
		delete(s.files, seg)
	}
	s.filesMu.Unlock()
	if errRemove := os.Remove(segmentPath(s.dir, seg)); errRemove != nil && !os.IsNotExist(errRemove) {
		return errRemove
	}
	return nil
}

func (s *Store) segmentReader(seg uint32) (*os.File, error) {
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if f := s.files[seg]; f != nil {
		return f, nil
	}
	f, errOpen := os.Open(segmentPath(s.dir, seg))
	if errOpen != nil {
		return nil, errOpen
	}
	s.files[seg] = f
	return f, nil
}

func (s *Store) loadPack(id uint64, loc packLoc) ([]byte, error) {
	s.cacheMu.Lock()
	if el := s.cacheIndex[id]; el != nil {
		s.cacheLRU.MoveToFront(el)
		data := el.Value.(*cacheEntry).data
		s.cacheMu.Unlock()
		return data, nil
	}
	s.cacheMu.Unlock()

	f, errOpen := s.segmentReader(loc.seg)
	if errOpen != nil {
		return nil, fmt.Errorf("prompt archive: open segment %d: %w", loc.seg, errOpen)
	}
	compressed := make([]byte, loc.clen)
	if _, errRead := f.ReadAt(compressed, loc.off); errRead != nil {
		return nil, fmt.Errorf("prompt archive: read pack %d: %w", id, errRead)
	}
	data, errDecode := s.dec.DecodeAll(compressed, make([]byte, 0, loc.rawLen))
	if errDecode != nil {
		return nil, fmt.Errorf("prompt archive: decode pack %d: %w", id, errDecode)
	}

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if el := s.cacheIndex[id]; el != nil {
		return el.Value.(*cacheEntry).data, nil
	}
	s.cacheIndex[id] = s.cacheLRU.PushFront(&cacheEntry{id: id, data: data})
	s.cacheBytes += len(data)
	for s.cacheBytes > packCacheMaxBytes && s.cacheLRU.Len() > 1 {
		oldest := s.cacheLRU.Back()
		entry := oldest.Value.(*cacheEntry)
		s.cacheLRU.Remove(oldest)
		delete(s.cacheIndex, entry.id)
		s.cacheBytes -= len(entry.data)
	}
	return data, nil
}

func (s *Store) assemble(spans []span, locs map[uint64]packLoc) ([]byte, error) {
	var total uint64
	for _, sp := range spans {
		total += sp.n
	}
	out := make([]byte, 0, total)
	for _, sp := range spans {
		loc, ok := locs[sp.pack]
		if !ok {
			return nil, fmt.Errorf("prompt archive: pack %d missing", sp.pack)
		}
		data, errLoad := s.loadPack(sp.pack, loc)
		if errLoad != nil {
			return nil, errLoad
		}
		if sp.off+sp.n > uint64(len(data)) {
			return nil, fmt.Errorf("prompt archive: span outside pack %d", sp.pack)
		}
		out = append(out, data[sp.off:sp.off+sp.n]...)
	}
	return out, nil
}

// Get loads one full record.
func (s *Store) Get(id uint64) (*Record, error) {
	var rec *decodedRecord
	locs := make(map[uint64]packLoc)
	errView := s.db.View(func(tx *bbolt.Tx) error {
		recB := tx.Bucket(bucketRec)
		if recB == nil {
			return ErrNotFound
		}
		raw := recB.Get(u64Key(id))
		if raw == nil {
			return ErrNotFound
		}
		decoded, errDecode := decodeRecord(id, raw, true)
		if errDecode != nil {
			return errDecode
		}
		rec = decoded
		packB := tx.Bucket(bucketPack)
		for _, spans := range [][]span{rec.req, rec.resp} {
			for _, sp := range spans {
				if _, seen := locs[sp.pack]; seen {
					continue
				}
				rawLoc := packB.Get(u64Key(sp.pack))
				if rawLoc == nil {
					return fmt.Errorf("prompt archive: pack %d missing", sp.pack)
				}
				loc, errLoc := decodePackLoc(rawLoc)
				if errLoc != nil {
					return errLoc
				}
				locs[sp.pack] = loc
			}
		}
		return nil
	})
	if errView != nil {
		return nil, errView
	}
	req, errReq := s.assemble(rec.req, locs)
	if errReq != nil {
		return nil, errReq
	}
	resp, errResp := s.assemble(rec.resp, locs)
	if errResp != nil {
		return nil, errResp
	}
	return &Record{Meta: rec.meta, Request: req, Response: resp}, nil
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
}

// List returns matching records, newest first.
func (s *Store) List(f Filter) (*Page, error) {
	f.Limit = normalizeLimit(f.Limit)
	if f.Deep && f.Query != "" {
		return s.deepList(f)
	}
	page := &Page{Items: []Summary{}}
	errView := s.db.View(func(tx *bbolt.Tx) error {
		recB := tx.Bucket(bucketRec)
		if recB == nil {
			return nil
		}
		c := recB.Cursor()
		k, v := s.seekStart(c, f)
		for ; k != nil; k, v = c.Prev() {
			id := binary.BigEndian.Uint64(k)
			if !f.Since.IsZero() && id < uint64(f.Since.UnixMilli())*1000 {
				break
			}
			page.Scanned++
			if page.Scanned > maxListScan {
				page.Truncated = true
				page.Next = FormatID(id + 1)
				break
			}
			decoded, errDecode := decodeRecord(id, v, false)
			if errDecode != nil {
				continue
			}
			if !f.matchMeta(&decoded.meta) {
				continue
			}
			page.Items = append(page.Items, decoded.meta.summary())
			if len(page.Items) >= f.Limit {
				page.Next = FormatID(id)
				break
			}
		}
		return nil
	})
	return page, errView
}

func (s *Store) seekStart(c *bbolt.Cursor, f Filter) ([]byte, []byte) {
	start := uint64(0)
	if before, ok := ParseID(f.Before); ok {
		start = before
	}
	if !f.Until.IsZero() {
		until := uint64(f.Until.UnixMilli()) * 1000
		if start == 0 || until < start {
			start = until
		}
	}
	if start == 0 {
		return c.Last()
	}
	k, _ := c.Seek(u64Key(start))
	if k == nil {
		return c.Last()
	}
	return c.Prev()
}

// deepList searches the content each record introduced. Because history is
// deduplicated, a hit is the record where the text first appeared, which
// keeps a long conversation from flooding the results with copies.
func (s *Store) deepList(f Filter) (*Page, error) {
	page := &Page{Items: []Summary{}}
	needle := bytes.ToLower([]byte(f.Query))
	budget := int64(f.MaxDeepMB)
	if budget <= 0 {
		budget = defaultDeepScanMB
	}
	budget <<= 20
	before, hasBefore := ParseID(f.Before)

	type packRef struct {
		id  uint64
		loc packLoc
	}
	cursor := uint64(0)
	seen := make(map[uint64]bool)
	for len(page.Items) < f.Limit {
		var batch []packRef
		errView := s.db.View(func(tx *bbolt.Tx) error {
			packB := tx.Bucket(bucketPack)
			if packB == nil {
				return nil
			}
			c := packB.Cursor()
			var k, v []byte
			if cursor == 0 {
				k, v = c.Last()
			} else {
				c.Seek(u64Key(cursor))
				k, v = c.Prev()
			}
			for ; k != nil && len(batch) < 256; k, v = c.Prev() {
				loc, errLoc := decodePackLoc(v)
				if errLoc != nil {
					continue
				}
				batch = append(batch, packRef{id: binary.BigEndian.Uint64(k), loc: loc})
			}
			return nil
		})
		if errView != nil {
			return nil, errView
		}
		if len(batch) == 0 {
			break
		}
		cursor = batch[len(batch)-1].id
		var hits []uint64
		for _, ref := range batch {
			if hasBefore && ref.loc.recID >= before {
				continue
			}
			if !f.Until.IsZero() && ref.loc.recID >= uint64(f.Until.UnixMilli())*1000 {
				continue
			}
			if !f.Since.IsZero() && ref.loc.recID < uint64(f.Since.UnixMilli())*1000 {
				continue
			}
			if seen[ref.loc.recID] {
				continue
			}
			if budget <= 0 {
				page.Truncated = true
				break
			}
			data, errLoad := s.loadPack(ref.id, ref.loc)
			if errLoad != nil {
				continue
			}
			budget -= int64(len(data))
			page.Scanned++
			if bytes.Contains(bytes.ToLower(data), needle) {
				seen[ref.loc.recID] = true
				hits = append(hits, ref.loc.recID)
			}
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i] > hits[j] })
		errView = s.db.View(func(tx *bbolt.Tx) error {
			recB := tx.Bucket(bucketRec)
			for _, id := range hits {
				raw := recB.Get(u64Key(id))
				if raw == nil {
					continue
				}
				decoded, errDecode := decodeRecord(id, raw, false)
				if errDecode != nil || !f.matchMeta(&decoded.meta) {
					continue
				}
				page.Items = append(page.Items, decoded.meta.summary())
				if len(page.Items) >= f.Limit {
					page.Next = FormatID(id)
					return nil
				}
			}
			return nil
		})
		if errView != nil {
			return nil, errView
		}
		if page.Truncated {
			break
		}
	}
	return page, nil
}

// Stats reports archive totals.
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{Dir: s.dir}
	errView := s.db.View(func(tx *bbolt.Tx) error {
		segB := tx.Bucket(bucketSeg)
		if segB == nil {
			return nil
		}
		return segB.ForEach(func(_, v []byte) error {
			var info segInfo
			if errUnmarshal := json.Unmarshal(v, &info); errUnmarshal != nil {
				return errUnmarshal
			}
			st.Segments++
			st.Records += info.Records
			st.RawBytes += info.RawBytes
			st.StoredBytes += info.Stored
			if info.Oldest > 0 && (st.Oldest.IsZero() || time.UnixMilli(info.Oldest).Before(st.Oldest)) {
				st.Oldest = time.UnixMilli(info.Oldest)
			}
			if info.Newest > 0 && time.UnixMilli(info.Newest).After(st.Newest) {
				st.Newest = time.UnixMilli(info.Newest)
			}
			return nil
		})
	})
	if errView != nil {
		return st, errView
	}
	// Ratio uses real disk usage: segment files plus the index with its free pages.
	if entries, errRead := os.ReadDir(s.dir); errRead == nil {
		for _, entry := range entries {
			if info, errInfo := entry.Info(); errInfo == nil && info.Mode().IsRegular() {
				st.DiskBytes += info.Size()
			}
		}
	}
	if st.DiskBytes > 0 {
		st.Ratio = float64(st.RawBytes) / float64(st.DiskBytes)
	}
	return st, nil
}
