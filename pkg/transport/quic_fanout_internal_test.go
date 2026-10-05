package transport

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/ranged"
)

var _ ConnectionsConfigurer = (*QUICTransport)(nil)

func TestParseFanoutReply(t *testing.T) {
	for _, tc := range []struct {
		line  string
		count int
		port  int
	}{
		{"OK FANOUT 1 49152", 1, 49152},
		{"OK FANOUT 4 9444", 4, 9444},
		{fmt.Sprintf("OK FANOUT %d 65535", ranged.MaxConnections), ranged.MaxConnections, 65535},
	} {
		count, port, err := parseFanoutReply(tc.line)
		if err != nil || count != tc.count || port != tc.port {
			t.Fatalf("parseFanoutReply(%q) = %d, %d, %v", tc.line, count, port, err)
		}
	}

	for _, line := range []string{
		"",
		"OK",
		"OK FANOUT",
		"OK FANOUT 4",
		"OK FANOUT 4 9444 extra",
		"OKAY FANOUT 4 9444",
		"OK CAPS NOHASH",
		"ERROR unknown command: FANOUT",
		"ERROR FANOUT 4 9444",
		"OK FANOUT four 9444",
		"OK FANOUT 4 port",
		"OK FANOUT 4 94.4",
		"OK FANOUT +4 9444",
		"OK FANOUT -4 9444",
		"OK FANOUT 4 -9444",
		"OK FANOUT  4 9444",
		"OK FANOUT 0 9444", // implausible count
		fmt.Sprintf("OK FANOUT %d 9444", ranged.MaxConnections+1), // above the shared ceiling
		"OK FANOUT 999999 9444",                                   // absurd count
		"OK FANOUT 4 0",                                           // implausible port
		"OK FANOUT 4 65536",                                       // above the port range
		"OK FANOUT 4 99999999999999999999",                        // overflows int64
		"OK FANOUT 4 " + strings.Repeat("9", 40),                  // digit-only but absurd
	} {
		if count, port, err := parseFanoutReply(line); err == nil {
			t.Fatalf("parseFanoutReply(%q) accepted: %d, %d", line, count, port)
		}
	}
}

func TestValidateRangeReply(t *testing.T) {
	payload := []byte("range payload")
	digest := sha256.Sum256(payload)
	hex := fmt.Sprintf("%x", digest)

	if err := validateRangeReply("OK RANGE 4096 13", 4096, 13, [sha256.Size]byte{}, false); err != nil {
		t.Fatalf("plain acknowledgement rejected: %v", err)
	}
	if err := validateRangeReply("OK RANGE 4096 13 "+hex, 4096, 13, digest, true); err != nil {
		t.Fatalf("digest acknowledgement rejected: %v", err)
	}

	for _, tc := range []struct {
		name    string
		line    string
		enabled bool
	}{
		{"empty", "", false},
		{"error reply", "ERROR range overlaps", false},
		{"wrong keyword", "OK RANGES 4096 13", false},
		{"wrong status", "OKAY RANGE 4096 13", false},
		{"wrong offset", "OK RANGE 0 13", false},
		{"wrong length", "OK RANGE 4096 12", false},
		{"non-numeric offset", "OK RANGE start 13", false},
		{"missing digest", "OK RANGE 4096 13", true},
		{"unexpected digest", "OK RANGE 4096 13 " + hex, false},
		{"malformed digest", "OK RANGE 4096 13 nothex", true},
		{"short digest", "OK RANGE 4096 13 abcd", true},
		{"other digest", "OK RANGE 4096 13 " + fmt.Sprintf("%x", sha256.Sum256(nil)), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateRangeReply(tc.line, 4096, 13, digest, tc.enabled); err == nil {
				t.Fatalf("accepted %q", tc.line)
			}
		})
	}
}

func TestValidateCommitReply(t *testing.T) {
	digest := sha256.Sum256([]byte("whole file"))
	hex := fmt.Sprintf("%x", digest)

	if err := validateCommitReply("OK COMMIT 100", 100, [sha256.Size]byte{}, false); err != nil {
		t.Fatalf("plain commit rejected: %v", err)
	}
	if err := validateCommitReply("OK COMMIT 100 "+hex, 100, digest, true); err != nil {
		t.Fatalf("digest commit rejected: %v", err)
	}
	for _, tc := range []struct {
		line    string
		enabled bool
	}{
		{"", false},
		{"ERROR coverage gap 0-4096", false},
		{"OK COMMIT", false},
		{"OK COMMIT 99", false},
		{"OK COMMITTED 100", false},
		{"OK COMMIT 100 " + hex, false},
		{"OK COMMIT 100", true},
		{"OK COMMIT 100 " + fmt.Sprintf("%x", sha256.Sum256(nil)), true},
		{"OK COMMIT 100 zz", true},
	} {
		if err := validateCommitReply(tc.line, 100, digest, tc.enabled); err == nil {
			t.Fatalf("accepted %q (checksum=%v)", tc.line, tc.enabled)
		}
	}
}

func TestCommitTimeoutScalesForLargeFinalization(t *testing.T) {
	q := NewQUICTransport(Config{Timeout: 30})
	if got := q.commitTimeout(1 << 40); got < 24*time.Hour {
		t.Fatalf("1 TiB commit timeout = %s, want at least 24h", got)
	}
	if got := q.commitTimeout(1); got < 5*time.Minute {
		t.Fatalf("small commit timeout = %s, want at least 5m", got)
	}
}

func TestQUICSetConnectionsValidation(t *testing.T) {
	client := NewQUICTransport(Config{})
	for _, invalid := range []int{-1, 0, ranged.MaxConnections + 1, 1000} {
		if err := client.SetConnections(invalid); err == nil {
			t.Fatalf("connections=%d accepted", invalid)
		}
		if client.config.Connections != 0 {
			t.Fatalf("invalid connections changed configuration: %d", client.config.Connections)
		}
	}
	for _, valid := range []int{1, 2, ranged.MaxConnections} {
		if err := client.SetConnections(valid); err != nil {
			t.Fatalf("connections=%d rejected: %v", valid, err)
		}
		if client.config.Connections != valid {
			t.Fatalf("connections = %d, want %d", client.config.Connections, valid)
		}
	}
	// A single connection must never take the fan-out path.
	if err := client.SetConnections(1); err != nil {
		t.Fatal(err)
	}
	if client.fanoutRequested() {
		t.Fatal("one connection requested fan-out")
	}
}

// The reserve/release bookkeeping caps total data sockets, so -workers times
// -connections cannot exhaust file descriptors.
func TestQUICReserveDataSockets(t *testing.T) {
	client := NewQUICTransport(Config{})
	if got := client.reserveDataSockets(ranged.MaxConnections - 1); got != ranged.MaxConnections-1 {
		t.Fatalf("first reservation = %d", got)
	}
	if got := client.reserveDataSockets(4); got != 1 {
		t.Fatalf("capped reservation = %d, want 1", got)
	}
	if got := client.reserveDataSockets(4); got != 0 {
		t.Fatalf("exhausted reservation = %d, want 0", got)
	}
	client.releaseDataSockets(ranged.MaxConnections)
	if client.dataSockets != 0 {
		t.Fatalf("sockets outstanding after release: %d", client.dataSockets)
	}
	client.releaseDataSockets(5)
	if client.dataSockets != 0 {
		t.Fatalf("over-release went negative: %d", client.dataSockets)
	}
	if got := client.reserveDataSockets(ranged.MaxConnections); got != ranged.MaxConnections {
		t.Fatalf("reservation after release = %d", got)
	}
}

// Fan-out requires an established control connection; without one the sender
// reports a connection error rather than dialing data sockets.
func TestQUICFanoutRequiresConnection(t *testing.T) {
	client := NewQUICTransport(Config{Connections: 4})
	err := client.sendFanout(strings.NewReader("payload"), "file", 7)
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("sendFanout = %v", err)
	}
}
