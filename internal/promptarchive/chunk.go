// Package promptarchive records every proxied model request and its reply in a
// deduplicated, zstd-compressed on-disk archive that can be filtered later.
//
// Storage layout (inside the archive directory):
//   - index.db: bbolt index with record metadata, pack locations and the chunk hash index.
//   - packs-NNNNNN.bin: append-only segment files holding one zstd frame per record.
//
// Request and response bodies are split with content-defined chunking. A chunk
// that was already stored in the current segment is referenced instead of written
// again, so the ever-growing history that agent clients resend on every turn costs
// almost nothing. Only chunks a record introduces are compressed into its pack.
package promptarchive

import "crypto/sha256"

const (
	minChunkSize = 1 << 9
	maxChunkSize = 32 << 10
	// chunkMaskBits sets the average chunk size to about 1 KiB past the minimum.
	// Small chunks re-store less of the boundary each turn; measured on a 40-turn
	// agent conversation, ~1.5 KiB chunks stored 27% less than ~5 KiB chunks.
	chunkMaskBits = 10
)

// gearTable drives the rolling hash. Changing it only lowers dedup against
// existing data; stored records stay readable because they reference byte spans.
var gearTable = func() [256]uint64 {
	var table [256]uint64
	state := uint64(0x9E3779B97F4A7C15)
	for i := range table {
		// splitmix64
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		table[i] = z ^ (z >> 31)
	}
	return table
}()

// chunkHash identifies chunk content inside one archive segment. 96 bits keeps
// collisions out of reach for any realistic segment while trimming the index.
type chunkHash [12]byte

func hashChunk(data []byte) chunkHash {
	sum := sha256.Sum256(data)
	return chunkHash(sum[:len(chunkHash{})])
}

// splitChunks calls fn for each content-defined chunk of data, in order.
func splitChunks(data []byte, fn func(chunk []byte)) {
	for len(data) > 0 {
		n := chunkCut(data)
		fn(data[:n])
		data = data[n:]
	}
}

func chunkCut(data []byte) int {
	if len(data) <= minChunkSize {
		return len(data)
	}
	end := len(data)
	if end > maxChunkSize {
		end = maxChunkSize
	}
	const mask = uint64(1<<chunkMaskBits-1) << (64 - chunkMaskBits)
	var h uint64
	for i := minChunkSize; i < end; i++ {
		h = (h << 1) + gearTable[data[i]]
		if h&mask == 0 {
			return i + 1
		}
	}
	return end
}
