// Package ranged holds the offset, coverage and chunking logic shared by the
// QUIC fan-out sender and receiver. See docs/multi-connection-quic-design.md.
//
// Measurement showed that sender socket count carries QUIC throughput scaling,
// so fan-out means several sender sockets writing disjoint byte ranges of one
// file. This package owns the rules that keep those concurrent writes safe:
// every range is bounds-checked, ranges may never overlap, and a transfer
// commits only when the acknowledged ranges tile [0, total) exactly once.
package ranged

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// Wire commands. SEND/RECEIVE and the exact "OK CAPS NOHASH" reply are
// deliberately untouched so existing peers negotiate as before.
const (
	CmdFanout        = "FANOUT"
	CmdSendRange     = "SEND-RANGE"
	CmdSendRangeHash = "SEND-RANGE-HASH"
	CmdCommit        = "COMMIT"
	CmdCommitHash    = "COMMIT-HASH"
	CmdAbort         = "ABORT"
)

const (
	// DefaultConnections enables the measured QUIC fast path without requiring
	// callers to discover an opt-in flag. Four captured 92% of the eight-socket
	// gain while keeping socket and receiver-write concurrency modest.
	DefaultConnections = 4

	// MaxConnections bounds sender sockets per file. N=4 captured 92% of the
	// N=8 gain in benchmarks, so this is a generous ceiling, not a target.
	MaxConnections = 16

	// MinFanoutSize is the file size below which fan-out is pointless: setup
	// cost dominates and a single connection is already at full rate.
	MinFanoutSize = 64 << 20

	// MaxRangesPerToken bounds receiver bookkeeping per transfer. Fan-out
	// assigns one contiguous range per connection, so this is far above need
	// while still refusing an unbounded stream of tiny ranges.
	MaxRangesPerToken = 1024

	// TokenBytes is the token length in bytes before hex encoding.
	TokenBytes = 16
)

// Chunk is one contiguous span of a file assigned to a single connection.
type Chunk struct {
	Offset int64
	Length int64
}

// NewToken returns a hex-encoded random token scoping ranges to one transfer.
// It is not an authentication mechanism; it prevents accidental cross-transfer
// collisions and makes stale state attributable.
func NewToken() (string, error) {
	buf := make([]byte, TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate transfer token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ValidateToken accepts only a fixed-length lowercase hex token, so a token can
// never carry path separators or protocol framing into receiver state.
func ValidateToken(token string) error {
	if len(token) != hex.EncodedLen(TokenBytes) {
		return fmt.Errorf("invalid token length %d (want %d)", len(token), hex.EncodedLen(TokenBytes))
	}
	if strings.ToLower(token) != token {
		return fmt.Errorf("invalid token: must be lowercase hex")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return fmt.Errorf("invalid token: not hex")
	}
	return nil
}

// ValidateRange rejects out-of-bounds, empty and overflowing ranges. Overflow
// is checked explicitly because offset+length is attacker-influenced.
func ValidateRange(offset, length, total int64) error {
	if total < 0 {
		return fmt.Errorf("invalid total %d", total)
	}
	if offset < 0 {
		return fmt.Errorf("invalid offset %d", offset)
	}
	if length <= 0 {
		return fmt.Errorf("invalid length %d", length)
	}
	if offset > math.MaxInt64-length {
		return fmt.Errorf("range %d+%d overflows", offset, length)
	}
	if offset+length > total {
		return fmt.Errorf("range %d+%d exceeds total %d", offset, length, total)
	}
	return nil
}

// Plan splits total into one contiguous chunk per connection, each aligned up
// to align so receiver writes stay buffer-aligned. Contiguous spans also keep
// each socket's writes sequential, which matters on rotational destinations.
//
// It returns a single chunk when fan-out cannot help: total below
// MinFanoutSize, or connections <= 1.
func Plan(total int64, connections, align int) []Chunk {
	if total <= 0 {
		return nil
	}
	if connections > MaxConnections {
		connections = MaxConnections
	}
	if connections <= 1 || total < MinFanoutSize {
		return []Chunk{{Offset: 0, Length: total}}
	}
	if align <= 0 {
		align = 1
	}
	size := (total + int64(connections) - 1) / int64(connections)
	if r := size % int64(align); r != 0 {
		size += int64(align) - r
	}
	chunks := make([]Chunk, 0, connections)
	for offset := int64(0); offset < total; offset += size {
		length := size
		if remaining := total - offset; length > remaining {
			length = remaining
		}
		chunks = append(chunks, Chunk{Offset: offset, Length: length})
	}
	return chunks
}

// Coverage tracks which byte ranges a receiver has durably written. It refuses
// overlaps rather than allowing a confused or hostile sender to rewrite bytes
// it already acknowledged.
//
// Coverage is not safe for concurrent use; callers hold the token lock.
type Coverage struct {
	spans []Chunk // disjoint, sorted by offset, adjacent spans merged
	count int     // ranges accepted, before merging
}

// Add records a range, rejecting any overlap with an existing range.
func (c *Coverage) Add(offset, length, total int64) error {
	if err := ValidateRange(offset, length, total); err != nil {
		return err
	}
	if c.count >= MaxRangesPerToken {
		return fmt.Errorf("too many ranges for one transfer (limit %d)", MaxRangesPerToken)
	}
	end := offset + length
	for _, span := range c.spans {
		if offset < span.Offset+span.Length && span.Offset < end {
			return fmt.Errorf("range %d+%d overlaps already-written %d+%d", offset, length, span.Offset, span.Length)
		}
	}
	c.spans = append(c.spans, Chunk{Offset: offset, Length: length})
	c.count++
	sort.Slice(c.spans, func(i, j int) bool { return c.spans[i].Offset < c.spans[j].Offset })
	c.merge()
	return nil
}

func (c *Coverage) merge() {
	merged := c.spans[:0:cap(c.spans)]
	for _, span := range c.spans {
		if n := len(merged); n > 0 && merged[n-1].Offset+merged[n-1].Length == span.Offset {
			merged[n-1].Length += span.Length
			continue
		}
		merged = append(merged, span)
	}
	c.spans = merged
}

// Covers reports whether the accepted ranges tile [0, total) exactly once.
// Overlaps are impossible here because Add rejects them, so a single span of
// the full length is both necessary and sufficient.
func (c *Coverage) Covers(total int64) bool {
	if total == 0 {
		return len(c.spans) == 0
	}
	return len(c.spans) == 1 && c.spans[0].Offset == 0 && c.spans[0].Length == total
}

// Bytes returns the total number of bytes covered.
func (c *Coverage) Bytes() int64 {
	var sum int64
	for _, span := range c.spans {
		sum += span.Length
	}
	return sum
}

// Missing describes the gaps below total, for error messages only.
func (c *Coverage) Missing(total int64) string {
	var gaps []string
	cursor := int64(0)
	for _, span := range c.spans {
		if span.Offset > cursor {
			gaps = append(gaps, fmt.Sprintf("%d-%d", cursor, span.Offset))
		}
		cursor = span.Offset + span.Length
	}
	if cursor < total {
		gaps = append(gaps, fmt.Sprintf("%d-%d", cursor, total))
	}
	if len(gaps) == 0 {
		return "none"
	}
	return strings.Join(gaps, ",")
}

// OffsetWriter writes sequentially from a fixed starting offset through WriteAt.
// Each connection owns one OffsetWriter, so concurrent range writes to the same
// file need no shared seek position and no write lock.
type OffsetWriter struct {
	dst    io.WriterAt
	offset int64
	limit  int64 // bytes remaining; writes past the range are refused
}

// NewOffsetWriter returns a writer bounded to length bytes from offset.
func NewOffsetWriter(dst io.WriterAt, offset, length int64) *OffsetWriter {
	return &OffsetWriter{dst: dst, offset: offset, limit: length}
}

func (w *OffsetWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.limit {
		// Refuse rather than truncate: a sender exceeding its range is a
		// protocol violation, and silently dropping bytes would corrupt.
		return 0, fmt.Errorf("write of %d bytes exceeds remaining range %d", len(p), w.limit)
	}
	n, err := w.dst.WriteAt(p, w.offset)
	w.offset += int64(n)
	w.limit -= int64(n)
	return n, err
}

// Remaining reports unwritten bytes in this range.
func (w *OffsetWriter) Remaining() int64 { return w.limit }
