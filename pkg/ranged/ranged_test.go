package ranged

import (
	"math"
	"strings"
	"testing"
)

func TestValidateRange(t *testing.T) {
	total := int64(1000)
	if err := ValidateRange(0, 1000, total); err != nil {
		t.Fatalf("exact range rejected: %v", err)
	}
	if err := ValidateRange(999, 1, total); err != nil {
		t.Fatalf("final byte rejected: %v", err)
	}
	for name, c := range map[string]struct{ offset, length int64 }{
		"zero length":     {0, 0},
		"negative length": {0, -1},
		"negative offset": {-1, 10},
		"past end":        {995, 10},
		"offset past end": {1000, 1},
		"overflow":        {math.MaxInt64, 2},
	} {
		if err := ValidateRange(c.offset, c.length, total); err == nil {
			t.Errorf("%s: expected rejection for %d+%d", name, c.offset, c.length)
		}
	}
}

func TestCoverageRejectsOverlapAndTracksCompletion(t *testing.T) {
	total := int64(300)
	var c Coverage
	if err := c.Add(0, 100, total); err != nil {
		t.Fatalf("first range: %v", err)
	}
	if err := c.Add(200, 100, total); err != nil {
		t.Fatalf("third range: %v", err)
	}
	if c.Covers(total) {
		t.Fatal("reported complete with a gap at 100-200")
	}
	if got := c.Missing(total); got != "100-200" {
		t.Fatalf("Missing() = %q, want 100-200", got)
	}
	for name, c2 := range map[string]struct{ offset, length int64 }{
		"exact duplicate":    {0, 100},
		"partial overlap":    {50, 100},
		"containing overlap": {0, 300},
		"tail overlap":       {250, 50},
	} {
		if err := c.Add(c2.offset, c2.length, total); err == nil {
			t.Errorf("%s: overlap %d+%d accepted", name, c2.offset, c2.length)
		}
	}
	if err := c.Add(100, 100, total); err != nil {
		t.Fatalf("gap-filling range: %v", err)
	}
	if !c.Covers(total) {
		t.Fatalf("not complete after tiling; missing %s", c.Missing(total))
	}
	if c.Bytes() != total {
		t.Fatalf("Bytes() = %d, want %d", c.Bytes(), total)
	}
}

func TestCoverageRangeLimit(t *testing.T) {
	total := int64(MaxRangesPerToken + 10)
	var c Coverage
	for i := int64(0); i < MaxRangesPerToken; i++ {
		if err := c.Add(i, 1, total); err != nil {
			t.Fatalf("range %d: %v", i, err)
		}
	}
	if err := c.Add(MaxRangesPerToken, 1, total); err == nil {
		t.Fatal("expected rejection past MaxRangesPerToken")
	}
}

func TestPlanTilesExactlyOnce(t *testing.T) {
	for _, total := range []int64{MinFanoutSize, MinFanoutSize + 1, 6140975104, MinFanoutSize*3 + 7} {
		for _, connections := range []int{2, 3, 4, 8, 16} {
			chunks := Plan(total, connections, 32*1024)
			if len(chunks) == 0 {
				t.Fatalf("total=%d n=%d: no chunks", total, connections)
			}
			var c Coverage
			for _, chunk := range chunks {
				if err := c.Add(chunk.Offset, chunk.Length, total); err != nil {
					t.Fatalf("total=%d n=%d: %v", total, connections, err)
				}
			}
			if !c.Covers(total) {
				t.Fatalf("total=%d n=%d: missing %s", total, connections, c.Missing(total))
			}
			if len(chunks) > connections {
				t.Fatalf("total=%d n=%d: %d chunks exceeds connections", total, connections, len(chunks))
			}
		}
	}
}

func TestPlanSkipsFanoutWhenPointless(t *testing.T) {
	if got := Plan(MinFanoutSize-1, 8, 32*1024); len(got) != 1 {
		t.Fatalf("small file split into %d chunks, want 1", len(got))
	}
	if got := Plan(MinFanoutSize*4, 1, 32*1024); len(got) != 1 {
		t.Fatalf("single connection split into %d chunks, want 1", len(got))
	}
	if got := Plan(0, 8, 32*1024); got != nil {
		t.Fatalf("empty file produced %v, want nil", got)
	}
	if got := Plan(MinFanoutSize*4, 1000, 32*1024); len(got) > MaxConnections {
		t.Fatalf("produced %d chunks, want at most %d", len(got), MaxConnections)
	}
}

func TestPlanAlignsChunkStarts(t *testing.T) {
	align := int64(32 * 1024)
	for _, chunk := range Plan(MinFanoutSize*4+12345, 4, int(align)) {
		if chunk.Offset%align != 0 {
			t.Fatalf("offset %d not aligned to %d", chunk.Offset, align)
		}
	}
}

type writerAtFunc func(p []byte, off int64) (int, error)

func (f writerAtFunc) WriteAt(p []byte, off int64) (int, error) { return f(p, off) }

func TestOffsetWriterWritesAtOffsetAndBounds(t *testing.T) {
	dst := make([]byte, 20)
	w := NewOffsetWriter(writerAtFunc(func(p []byte, off int64) (int, error) {
		return copy(dst[off:], p), nil
	}), 5, 6)

	if n, err := w.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("Write = (%d, %v)", n, err)
	}
	if w.Remaining() != 3 {
		t.Fatalf("Remaining = %d, want 3", w.Remaining())
	}
	if n, err := w.Write([]byte("def")); err != nil || n != 3 {
		t.Fatalf("second Write = (%d, %v)", n, err)
	}
	if string(dst[5:11]) != "abcdef" {
		t.Fatalf("destination = %q, want abcdef at offset 5", string(dst[5:11]))
	}
	if string(dst[:5]) != "\x00\x00\x00\x00\x00" {
		t.Fatal("wrote before the range start")
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("expected rejection past the range end")
	}
}

func TestTokens(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := ValidateToken(token); err != nil {
		t.Fatalf("generated token rejected: %v", err)
	}
	other, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if other == token {
		t.Fatal("tokens are not unique")
	}
	for name, bad := range map[string]string{
		"empty":      "",
		"short":      token[:len(token)-1],
		"long":       token + "a",
		"uppercase":  strings.ToUpper(token),
		"non hex":    strings.Repeat("z", len(token)),
		"path chars": strings.Repeat("/", len(token)),
	} {
		if err := ValidateToken(bad); err == nil {
			t.Errorf("%s: accepted invalid token %q", name, bad)
		}
	}
}
