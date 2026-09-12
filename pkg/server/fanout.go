package server

// Receiver side of QUIC multi-socket fan-out: FANOUT, SEND-RANGE(-HASH),
// COMMIT(-HASH) and ABORT. See docs/multi-connection-quic-design.md.
//
// Range validation, overlap rejection, coverage, token format and the offset
// writer all live in pkg/ranged; this file owns the wire framing, the staging
// file and the token registry shared by every connection and socket.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gbjohnso/gosync/pkg/checksum"
	"github.com/gbjohnso/gosync/pkg/ranged"
	"github.com/quic-go/quic-go"
)

const (
	// maxLiveTokens bounds uncommitted transfers so a peer cannot exhaust
	// receiver memory or file descriptors by opening transfers it never
	// commits or aborts. Fan-out uses one token per file in flight and the
	// sender caps concurrent sockets at ranged.MaxConnections, so this is far
	// above legitimate need.
	maxLiveTokens = 64

	// maxCommittedTokens bounds the committed tombstones kept only so a
	// retried COMMIT is idempotent. They hold no file handle and no staged
	// file; the oldest is evicted once the limit is reached. Tombstones are
	// counted separately from maxLiveTokens so a long multi-file sync is never
	// refused a new transfer by its own completed ones.
	maxCommittedTokens = 64

	// minTokenTimeout is the floor for idle token expiry, per the design.
	minTokenTimeout = 60 * time.Second

	// Sweep bounds keep expiry responsive without spinning on a long timeout.
	minSweepInterval = 100 * time.Millisecond
	maxSweepInterval = 15 * time.Second
)

// rangeTransfer is the receiver's state for one token: where the bytes are
// being staged, which ranges have been acknowledged, and whether it committed.
// Every field is guarded by Server.mu except the immutable path/total fields
// and the *os.File contents, which concurrent ranges reach through WriteAt.
type rangeTransfer struct {
	remotePath string // as sent, so later ranges must agree
	dest       string // filepath.Join(baseDir, remotePath)
	staged     string // <dest>.gosync-<token>.part
	total      int64
	file       *os.File
	coverage   ranged.Coverage
	spans      []ranged.Chunk // acknowledged plus in-flight, for pre-write refusal
	inFlight   int
	active     time.Time
	committing bool
	committed  bool
	digest     string // whole-file digest verified at COMMIT-HASH
}

// stagedPath keeps the partial file beside its destination so the commit rename
// is within one filesystem and therefore atomic.
func stagedPath(dest, token string) string {
	return dest + ".gosync-" + token + ".part"
}

// reserve refuses a range before a single byte is written, so a confused or
// hostile sender cannot overwrite bytes the receiver already acknowledged.
//
// pkg/ranged.Coverage is the authority for acknowledged ranges and is the only
// thing COMMIT trusts, but it deliberately offers no way to un-add a range,
// while a range whose connection dies must stay retryable at its original
// offset. In-flight reservations are therefore tracked here and both sets are
// checked with the same disjointness rule.
func (t *rangeTransfer) reserve(offset, length int64) error {
	if len(t.spans) >= ranged.MaxRangesPerToken {
		return fmt.Errorf("too many ranges for one transfer (limit %d)", ranged.MaxRangesPerToken)
	}
	end := offset + length
	for _, span := range t.spans {
		if offset < span.Offset+span.Length && span.Offset < end {
			return fmt.Errorf("range %d+%d overlaps already-reserved %d+%d", offset, length, span.Offset, span.Length)
		}
	}
	t.spans = append(t.spans, ranged.Chunk{Offset: offset, Length: length})
	t.inFlight++
	return nil
}

// release drops a reservation whose payload never arrived, leaving the offset
// free for the retry the sender is expected to make on another connection.
func (t *rangeTransfer) release(offset, length int64) {
	for i, span := range t.spans {
		if span.Offset == offset && span.Length == length {
			t.spans = append(t.spans[:i], t.spans[i+1:]...)
			return
		}
	}
}

// discard closes and removes the staged file. It must be called with Server.mu
// held. A committed transfer has no staged file left to remove.
func (t *rangeTransfer) discard() {
	if t.file != nil {
		t.file.Close()
		t.file = nil
	}
	t.spans = nil
	if t.staged == "" || t.committed {
		return
	}
	if err := os.Remove(t.staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Printf("gosync: remove staged file %s failed: %v\n", t.staged, err)
	}
}

// handleFanout answers the fan-out capability probe with the receiver's
// configured connection limit and the single UDP port it listens on.
func (s *Server) handleFanout(conn net.Conn, parts []string) {
	if len(parts) != 1 {
		s.sendError(conn, "FANOUT takes no arguments")
		return
	}
	s.mu.RLock()
	connections := s.maxConnections
	port := listenPort(s.quicListener, s.listener)
	s.mu.RUnlock()
	if connections < 1 {
		connections = 1
	}
	s.sendResponse(conn, fmt.Sprintf("FANOUT %d %d", connections, port))
}

// listenPort reports the port clients should dial. Callers already hold s.mu,
// so it takes the listeners rather than calling Addr.
func listenPort(quicListener *quic.Listener, listener net.Listener) int {
	var addr net.Addr
	switch {
	case quicListener != nil:
		addr = quicListener.Addr()
	case listener != nil:
		addr = listener.Addr()
	default:
		return 0
	}
	switch bound := addr.(type) {
	case *net.UDPAddr:
		return bound.Port
	case *net.TCPAddr:
		return bound.Port
	}
	if _, port, err := net.SplitHostPort(addr.String()); err == nil {
		if value, err := strconv.Atoi(port); err == nil {
			return value
		}
	}
	return 0
}

// handleSendRange stores one range of one file. Format:
//
//	SEND-RANGE      <token> <offset> <length> <total> <base64path>
//	SEND-RANGE-HASH <token> <offset> <length> <total> <base64path>
//
// followed by exactly length payload bytes. The reply is "OK RANGE <offset>
// <length>", with the receiver's digest of those bytes appended for the HASH
// variant.
func (s *Server) handleSendRange(reader io.Reader, conn net.Conn, line string, withChecksum bool) {
	// base64 never contains a space, so a plain split is unambiguous.
	fields := strings.Split(line, " ")
	command := fields[0]
	if len(fields) != 6 {
		s.sendError(conn, command+" requires token, offset, length, total and path")
		return
	}
	token := fields[1]
	if err := ranged.ValidateToken(token); err != nil {
		s.sendError(conn, err.Error())
		return
	}
	offset, offsetErr := parseDecimal(fields[2])
	length, lengthErr := parseDecimal(fields[3])
	total, totalErr := parseDecimal(fields[4])
	if offsetErr != nil || lengthErr != nil || totalErr != nil {
		s.sendError(conn, command+" requires decimal offset, length and total")
		return
	}
	if err := ranged.ValidateRange(offset, length, total); err != nil {
		s.sendError(conn, err.Error())
		return
	}
	remotePath, err := decodePath(fields[5])
	if err != nil {
		s.sendError(conn, command+" path not base64 encoded")
		return
	}
	transfer, staged, err := s.beginRange(token, remotePath, offset, length, total)
	if err != nil {
		s.sendError(conn, err.Error())
		return
	}
	// One OffsetWriter per range writes through WriteAt, so concurrent ranges
	// share the staged file with no seek position and no lock around the copy.
	// The handle came out of the registry under the lock; expiry or Stop closing
	// it concurrently fails this copy, which is exactly the intended outcome.
	writer := ranged.NewOffsetWriter(staged, offset, length)
	var digest [sha256.Size]byte
	if withChecksum {
		_, digest, err = checksum.CopyNWithSHA256Buffer(writer, reader, length, s.bufferSize)
	} else {
		_, err = checksum.CopyNBuffer(writer, reader, length, s.bufferSize)
	}
	if err := s.finishRange(token, transfer, offset, length, err); err != nil {
		s.sendError(conn, fmt.Sprintf("range %d+%d failed: %v", offset, length, err))
		return
	}
	if withChecksum {
		s.sendResponse(conn, fmt.Sprintf("RANGE %d %d %s", offset, length, hex.EncodeToString(digest[:])))
		return
	}
	s.sendResponse(conn, fmt.Sprintf("RANGE %d %d", offset, length))
}

// beginRange reserves a range, creating the staged file on the first range for
// a token. Ranged data is never written to the destination path itself, so an
// interrupted fan-out cannot leave a partial file where the real one belongs.
// It returns the staged file handle read under the lock, so callers never touch
// registry fields without it.
func (s *Server) beginRange(token, remotePath string, offset, length, total int64) (*rangeTransfer, *os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, nil, errors.New("server is stopping")
	}
	if s.tokens == nil {
		s.tokens = make(map[string]*rangeTransfer)
	}
	if transfer := s.tokens[token]; transfer != nil {
		switch {
		case transfer.committed:
			return nil, nil, fmt.Errorf("transfer %s already committed", token)
		case transfer.committing:
			return nil, nil, fmt.Errorf("transfer %s is committing", token)
		case transfer.total != total:
			return nil, nil, fmt.Errorf("total %d does not match %d for transfer %s", total, transfer.total, token)
		case transfer.remotePath != remotePath:
			return nil, nil, fmt.Errorf("path does not match the earlier range for transfer %s", token)
		case transfer.file == nil:
			return nil, nil, fmt.Errorf("transfer %s has no staged file", token)
		}
		if err := transfer.reserve(offset, length); err != nil {
			return nil, nil, err
		}
		transfer.active = time.Now()
		return transfer, transfer.file, nil
	}
	if s.liveTokens() >= maxLiveTokens {
		return nil, nil, fmt.Errorf("too many concurrent ranged transfers (limit %d)", maxLiveTokens)
	}
	s.evictCommittedTokens()
	dest := filepath.Join(s.baseDir, remotePath)
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return nil, nil, fmt.Errorf("create dir failed: %v", err)
	}
	staged := stagedPath(dest, token)
	file, err := os.OpenFile(staged, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return nil, nil, fmt.Errorf("create staging file failed: %v", err)
	}
	// Preallocation is an optimization only: tmpfs and some filesystems refuse
	// it, and WriteAt past the end still extends the file, so this is logged
	// and ignored rather than failing the transfer.
	if err := file.Truncate(total); err != nil {
		fmt.Printf("gosync: preallocating %s to %d bytes failed: %v\n", staged, total, err)
	}
	transfer := &rangeTransfer{
		remotePath: remotePath,
		dest:       dest,
		staged:     staged,
		total:      total,
		file:       file,
		active:     time.Now(),
	}
	if err := transfer.reserve(offset, length); err != nil {
		transfer.discard()
		return nil, nil, err
	}
	s.tokens[token] = transfer
	return transfer, file, nil
}

// finishRange acknowledges or releases a reservation. Only a fully copied range
// enters the coverage COMMIT checks, so a failed range neither counts towards
// completion nor blocks a retry at the same offset.
func (s *Server) finishRange(token string, transfer *rangeTransfer, offset, length int64, copyErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens[token] != transfer {
		// Expiry, ABORT or Stop discarded the transfer during the copy.
		if copyErr != nil {
			return copyErr
		}
		return fmt.Errorf("transfer %s no longer exists", token)
	}
	transfer.inFlight--
	transfer.active = time.Now()
	if copyErr != nil {
		transfer.release(offset, length)
		return copyErr
	}
	if err := transfer.coverage.Add(offset, length, transfer.total); err != nil {
		transfer.release(offset, length)
		return err
	}
	return nil
}

// handleCommit completes a ranged transfer:
//
//	COMMIT      <token> <total>
//	COMMIT-HASH <token> <total> <hexSHA256>
func (s *Server) handleCommit(conn net.Conn, line string, withChecksum bool) {
	fields := strings.Split(line, " ")
	command := fields[0]
	if (withChecksum && len(fields) != 4) || (!withChecksum && len(fields) != 3) {
		if withChecksum {
			s.sendError(conn, command+" requires token, total and checksum")
		} else {
			s.sendError(conn, command+" requires token and total")
		}
		return
	}
	token := fields[1]
	if err := ranged.ValidateToken(token); err != nil {
		s.sendError(conn, err.Error())
		return
	}
	total, err := parseDecimal(fields[2])
	if err != nil {
		s.sendError(conn, command+" requires a decimal total")
		return
	}
	expected := ""
	if withChecksum {
		expected = fields[3]
		if _, err := hex.DecodeString(expected); err != nil || len(expected) != hex.EncodedLen(sha256.Size) {
			s.sendError(conn, command+" requires a hex SHA-256 checksum")
			return
		}
	}
	digest, err := s.commitRange(token, total, expected, withChecksum)
	if err != nil {
		s.sendError(conn, err.Error())
		return
	}
	if withChecksum {
		s.sendResponse(conn, fmt.Sprintf("COMMIT %d %s", total, digest))
		return
	}
	s.sendResponse(conn, fmt.Sprintf("COMMIT %d", total))
}

// commitJob is everything finalizeCommit needs, copied out of the registry
// under the lock so the commit I/O touches no shared field.
type commitJob struct {
	file   *os.File
	staged string
	dest   string
	total  int64
}

func (s *Server) commitRange(token string, total int64, expected string, withChecksum bool) (string, error) {
	transfer, job, recorded, err := s.beginCommit(token, total, expected, withChecksum)
	if err != nil {
		return "", err
	}
	if job == nil {
		// Already committed: a lost acknowledgement must not become a failure.
		return recorded, nil
	}
	digest, err := s.finalizeCommit(*job, expected, withChecksum)
	if err != nil {
		s.discardToken(token, transfer)
		return "", err
	}
	s.completeCommit(transfer, digest)
	return digest, nil
}

// beginCommit runs the registry-side commit checks and claims the transfer.
// Coverage must tile [0, total) exactly once and no range may be in flight.
// A nil job means the token was already committed; the recorded digest is
// returned so the reply to the retry matches the original.
//
// A failed check discards the transfer, as the design requires: the sender is
// expected to commit only after every range was acknowledged, so a failure here
// is a protocol error and the staged file must not be left behind. The
// exceptions cannot discard anything safely: an unknown token has no state, a
// concurrent commit owns the file, and a committed token's data is already at
// the destination.
func (s *Server) beginCommit(token string, total int64, expected string, withChecksum bool) (*rangeTransfer, *commitJob, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	transfer := s.tokens[token]
	if transfer == nil {
		return nil, nil, "", fmt.Errorf("unknown transfer token %s", token)
	}
	transfer.active = time.Now()
	if transfer.committed {
		if transfer.total != total {
			return nil, nil, "", fmt.Errorf("commit total %d does not match committed %d for transfer %s", total, transfer.total, token)
		}
		if withChecksum {
			if transfer.digest == "" {
				return nil, nil, "", fmt.Errorf("transfer %s was committed without checksum verification", token)
			}
			if transfer.digest != expected {
				return nil, nil, "", fmt.Errorf("checksum mismatch: committed %s, sender sent %s", transfer.digest, expected)
			}
		}
		return transfer, nil, transfer.digest, nil
	}
	if transfer.committing {
		return nil, nil, "", fmt.Errorf("transfer %s is already committing", token)
	}
	discard := func(format string, args ...any) (*rangeTransfer, *commitJob, string, error) {
		delete(s.tokens, token)
		transfer.discard()
		return nil, nil, "", fmt.Errorf(format, args...)
	}
	if transfer.total != total {
		return discard("commit total %d does not match %d for transfer %s", total, transfer.total, token)
	}
	if transfer.inFlight > 0 {
		return discard("%d range(s) still in flight for transfer %s", transfer.inFlight, token)
	}
	if !transfer.coverage.Covers(total) {
		return discard("incomplete coverage for transfer %s: missing %s", token, transfer.coverage.Missing(total))
	}
	if transfer.file == nil {
		return discard("transfer %s has no staged file", token)
	}
	transfer.committing = true
	job := &commitJob{file: transfer.file, staged: transfer.staged, dest: transfer.dest, total: transfer.total}
	return transfer, job, "", nil
}

// finalizeCommit does the commit I/O without holding s.mu: the staged file may
// be large and reading it back must not block other transfers. The committing
// flag claimed in beginCommit keeps ranges, commits and expiry off this token.
func (s *Server) finalizeCommit(job commitJob, expected string, withChecksum bool) (string, error) {
	info, err := job.file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat staged file failed: %v", err)
	}
	if info.Size() != job.total {
		return "", fmt.Errorf("staged size %d does not match total %d", info.Size(), job.total)
	}
	if err := job.file.Sync(); err != nil {
		return "", fmt.Errorf("sync staged file failed: %v", err)
	}
	if err := job.file.Close(); err != nil {
		return "", fmt.Errorf("close staged file failed: %v", err)
	}
	digest := ""
	if withChecksum {
		// A whole-file SHA-256 cannot be composed from per-range digests, so
		// the receiver reads back what it actually persisted. That also proves
		// every range landed at the right offset.
		digest, err = s.stagedDigest(job.staged, job.total)
		if err != nil {
			return "", err
		}
		if digest != expected {
			return "", fmt.Errorf("checksum mismatch: computed %s, sender sent %s", digest, expected)
		}
	}
	if err := os.Rename(job.staged, job.dest); err != nil {
		return "", fmt.Errorf("rename staged file failed: %v", err)
	}
	return digest, nil
}

func (s *Server) stagedDigest(path string, total int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("reopen staged file failed: %v", err)
	}
	defer file.Close()
	_, digest, err := checksum.CopyNWithSHA256Buffer(io.Discard, file, total, s.bufferSize)
	if err != nil {
		return "", fmt.Errorf("read back staged file failed: %v", err)
	}
	return hex.EncodeToString(digest[:]), nil
}

// completeCommit records the commit. The staged file and its handle are gone
// and the destination is in place; the entry survives only as a tombstone so a
// retried COMMIT is idempotent, and idle expiry or eviction drops it.
func (s *Server) completeCommit(transfer *rangeTransfer, digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	transfer.committing = false
	transfer.committed = true
	transfer.digest = digest
	transfer.file = nil
	transfer.spans = nil
	transfer.active = time.Now()
}

// handleAbort removes the staged file and the token: ABORT <token>.
func (s *Server) handleAbort(conn net.Conn, line string) {
	fields := strings.Split(line, " ")
	if len(fields) != 2 {
		s.sendError(conn, "ABORT requires a token")
		return
	}
	if err := ranged.ValidateToken(fields[1]); err != nil {
		s.sendError(conn, err.Error())
		return
	}
	if err := s.abortToken(fields[1]); err != nil {
		s.sendError(conn, err.Error())
		return
	}
	s.sendResponse(conn, "ABORT")
}

// abortToken discards a transfer. An unknown token succeeds: it may already
// have expired or been aborted, and the sender's intent, no file at the
// destination path, holds either way.
func (s *Server) abortToken(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	transfer := s.tokens[token]
	if transfer == nil {
		return nil
	}
	if transfer.committed {
		return fmt.Errorf("transfer %s already committed", token)
	}
	if transfer.committing {
		return fmt.Errorf("transfer %s is committing", token)
	}
	delete(s.tokens, token)
	transfer.discard()
	return nil
}

// discardToken removes a token and its staged file after a failed commit.
func (s *Server) discardToken(token string, transfer *rangeTransfer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens[token] == transfer {
		delete(s.tokens, token)
	}
	transfer.committing = false
	transfer.discard()
}

// liveTokens counts uncommitted transfers; callers hold s.mu.
func (s *Server) liveTokens() int {
	live := 0
	for _, transfer := range s.tokens {
		if !transfer.committed {
			live++
		}
	}
	return live
}

// evictCommittedTokens keeps the idempotence tombstones bounded by dropping the
// oldest ones; callers hold s.mu.
func (s *Server) evictCommittedTokens() {
	for {
		count := 0
		oldest := ""
		var oldestAt time.Time
		for token, transfer := range s.tokens {
			if !transfer.committed {
				continue
			}
			count++
			if oldest == "" || transfer.active.Before(oldestAt) {
				oldest, oldestAt = token, transfer.active
			}
		}
		if count < maxCommittedTokens || oldest == "" {
			return
		}
		delete(s.tokens, oldest)
	}
}

// expireTokens drops idle tokens and deletes their staged files, so a crashed
// sender cannot accumulate garbage in the destination directory.
//
// A transfer with a range in flight or a commit in progress is kept: one
// multi-gigabyte range can legitimately take longer than the idle timeout, and
// a dead connection releases its range as soon as the copy fails.
func (s *Server) expireTokens(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	timeout := s.tokenTimeout
	if timeout <= 0 {
		timeout = minTokenTimeout
	}
	removed := 0
	for token, transfer := range s.tokens {
		if transfer.inFlight > 0 || transfer.committing {
			continue
		}
		if now.Sub(transfer.active) < timeout {
			continue
		}
		delete(s.tokens, token)
		transfer.discard()
		removed++
	}
	return removed
}

// cleanupTokens forgets every transfer and removes every staged file. Stop
// calls it after closing the listeners and before waiting for handlers, so it
// cannot block on a handler that is still copying.
func (s *Server) cleanupTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, transfer := range s.tokens {
		delete(s.tokens, token)
		transfer.discard()
	}
}

// startTokenSweeper runs idle token expiry for the lifetime of the server.
func (s *Server) startTokenSweeper() {
	s.mu.Lock()
	if s.stopped || s.sweeperDone != nil {
		s.mu.Unlock()
		return
	}
	timeout := s.tokenTimeout
	if timeout <= 0 {
		timeout = minTokenTimeout
	}
	done := make(chan struct{})
	s.sweeperDone = done
	s.workers.Add(1)
	s.mu.Unlock()
	go s.sweepTokens(done, timeout)
}

func (s *Server) sweepTokens(done <-chan struct{}, timeout time.Duration) {
	defer s.workers.Done()
	interval := timeout / 4
	if interval < minSweepInterval {
		interval = minSweepInterval
	}
	if interval > maxSweepInterval {
		interval = maxSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			s.expireTokens(now)
		}
	}
}

// parseDecimal accepts digits only, matching the sender's digit-only size
// parsing, so "+1", " 1", "0x10" and "1e3" are refused rather than coerced.
func parseDecimal(text string) (int64, error) {
	if text == "" {
		return 0, errors.New("empty number")
	}
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("invalid decimal %q", text)
		}
	}
	return strconv.ParseInt(text, 10, 64)
}
