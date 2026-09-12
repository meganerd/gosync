package transport

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gbjohnso/gosync/pkg/checksum"
)

// Finite scripted input avoids listeners, startup races, and blocked socket I/O.
// Only Read and Write are used by the transfer methods under test.
type checksumIntegrationConn struct {
	net.Conn
	input           *bytes.Reader
	output          bytes.Buffer
	writes          int
	payloadLimit    int
	payloadErr      error
	maxRead         int
	maxPayloadWrite int
}

func (c *checksumIntegrationConn) Read(p []byte) (int, error) {
	c.maxRead = max(c.maxRead, len(p))
	return c.input.Read(p)
}

func (c *checksumIntegrationConn) Write(p []byte) (int, error) {
	c.writes++
	if c.writes > 1 {
		c.maxPayloadWrite = max(c.maxPayloadWrite, len(p))
	}
	if c.writes > 1 && c.payloadLimit >= 0 {
		n, _ := c.output.Write(p[:min(len(p), c.payloadLimit)])
		return n, c.payloadErr
	}
	return c.output.Write(p)
}

func newChecksumIntegrationTransport(kind, input string, report func(int64), configs ...Config) (Transport, *checksumIntegrationConn, *bufio.Reader) {
	config := Config{Checksum: true}
	if len(configs) > 0 {
		config = configs[0]
	}
	conn := &checksumIntegrationConn{input: bytes.NewReader([]byte(input)), payloadLimit: -1}
	reader := bufio.NewReader(conn)
	if kind == "server" {
		tr := NewServerTransport(config)
		tr.conn, tr.reader, tr.progressCallback = conn, reader, report
		return tr, conn, reader
	}
	tr := NewTCPTransport(config)
	tr.conn, tr.reader, tr.progressCallback = conn, reader, report
	return tr, conn, reader
}

func checksumIntegrationHex(payload []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(payload))
}

const checksumIntegrationPath = "nested/file with spaces.bin"

func checksumIntegrationCommand(verb string, size int) string {
	path := base64.StdEncoding.EncodeToString([]byte(checksumIntegrationPath))
	if verb == "SEND" {
		return fmt.Sprintf("SEND %d %s\n", size, path)
	}
	return "RECEIVE " + path + "\n"
}

func requireChecksumIntegrationTransferError(t *testing.T, err, cause error) {
	t.Helper()
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "transfer failed:") {
		t.Fatalf("want wrapped transfer error %v, got %v", cause, err)
	}
}

func TestChecksumIntegrationRoundTrip(t *testing.T) {
	for _, bufferSize := range []int{0, 256 * 1024} {
		t.Run(fmt.Sprintf("buffer=%d", bufferSize), func(t *testing.T) {
			checksumIntegrationRoundTrip(t, bufferSize)
		})
	}
}

func checksumIntegrationRoundTrip(t *testing.T, bufferSize int) {
	config := Config{Checksum: true, BufferSize: bufferSize}
	effectiveSize := bufferSize
	if effectiveSize == 0 {
		effectiveSize = checksum.DefaultBufferSize
	}
	for _, kind := range []string{"server", "tcp"} {
		for _, mode := range []string{"file", "stream"} {
			for _, size := range []int{0, 37, effectiveSize - 1, effectiveSize, effectiveSize + 1, 4*effectiveSize + 17} {
				t.Run(fmt.Sprintf("%s/%s/%d", kind, mode, size), func(t *testing.T) {
					payload := make([]byte, size)
					for i := range payload {
						payload[i] = byte(i*31 + i/251)
					}
					const sentinel = "NEXT FRAME\n"
					response := fmt.Sprintf("OK %s %d\n", checksumIntegrationHex(payload), size)
					var progress int64
					sender, sendConn, _ := newChecksumIntegrationTransport(kind, response, func(n int64) { progress += n }, config)
					var err error
					if mode == "file" {
						path := filepath.Join(t.TempDir(), "source")
						if err := os.WriteFile(path, payload, 0600); err != nil {
							t.Fatal(err)
						}
						err = sender.SendFile(path, checksumIntegrationPath)
					} else {
						source := strings.NewReader(string(payload) + sentinel)
						err = sender.SendStream(source, checksumIntegrationPath, int64(size))
						rest, readErr := io.ReadAll(source)
						if readErr != nil || string(rest) != sentinel {
							t.Fatalf("source sentinel consumed: %q, %v", rest, readErr)
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					if want := min(size, effectiveSize); sendConn.maxPayloadWrite != want {
						t.Fatalf("send buffer = %d, want %d", sendConn.maxPayloadWrite, want)
					}
					command := checksumIntegrationCommand("SEND", size)
					if !bytes.Equal(sendConn.output.Bytes(), append([]byte(command), payload...)) {
						t.Fatal("send command or payload differs")
					}
					if progress != int64(size) {
						t.Fatalf("progress = %d, want %d", progress, size)
					}

					// Feed the actual sent payload back with the receive framing.
					wirePayload := sendConn.output.Bytes()[len(command):]
					input := fmt.Sprintf("OK SIZE %d\n%sCHECKSUM %s\n%s", size, wirePayload, checksumIntegrationHex(wirePayload), sentinel)
					receiver, recvConn, reader := newChecksumIntegrationTransport(kind, input, nil, config)
					var received []byte
					if mode == "file" {
						path := filepath.Join(t.TempDir(), "received")
						err = receiver.ReceiveFile(checksumIntegrationPath, path)
						if err == nil {
							received, err = os.ReadFile(path)
						}
					} else {
						var dst bytes.Buffer
						err = receiver.ReceiveStream(checksumIntegrationPath, &dst)
						received = dst.Bytes()
					}
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(received, payload) {
						t.Fatal("received payload differs")
					}
					if size > 4*effectiveSize && recvConn.maxRead != effectiveSize {
						t.Fatalf("receive buffer = %d, want %d", recvConn.maxRead, effectiveSize)
					}
					if recvConn.output.String() != checksumIntegrationCommand("RECEIVE", size) {
						t.Fatalf("unexpected receive command: %q", recvConn.output.String())
					}
					rest, err := io.ReadAll(reader)
					if err != nil || string(rest) != sentinel {
						t.Fatalf("receive sentinel consumed: %q, %v", rest, err)
					}
				})
			}
		}
	}
}

func TestTCPBufferSizeInvalidConfigBeforeDial(t *testing.T) {
	for _, kind := range []string{"tcp", "server"} {
		for _, size := range []int{-1, checksum.MinBufferSize - 1, checksum.MaxBufferSize + 1} {
			t.Run(fmt.Sprintf("%s/%d", kind, size), func(t *testing.T) {
				config := Config{BufferSize: size}
				var tr Transport = NewTCPTransport(config)
				if kind == "server" {
					tr = NewServerTransport(config)
				}
				// A malformed address also prevents network I/O if validation regresses.
				// The returned error must be validation, not address parsing or dialing.
				err := tr.Connect("invalid:address", -1)
				if !errors.Is(err, checksum.ValidateBufferSize(size)) {
					t.Fatalf("Connect = %v, want buffer validation error", err)
				}
				if tr.IsConnected() {
					t.Fatal("invalid config established a connection")
				}
			})
		}
	}
}

func TestChecksumIntegrationTruncatedSource(t *testing.T) {
	for _, kind := range []string{"server", "tcp"} {
		t.Run(kind, func(t *testing.T) {
			payload := []byte("short")
			response := fmt.Sprintf("OK %s %d\n", checksumIntegrationHex(payload), len(payload))
			var progress int64
			tr, conn, _ := newChecksumIntegrationTransport(kind, response, func(n int64) { progress += n })
			err := tr.SendStream(bytes.NewReader(payload), checksumIntegrationPath, int64(len(payload)+1))
			requireChecksumIntegrationTransferError(t, err, io.EOF)
			if conn.input.Len() != len(response) {
				t.Fatal("read success response after failed transfer")
			}
			if progress != int64(len(payload)) {
				t.Fatalf("progress = %d, want %d", progress, len(payload))
			}
		})
	}
}

func TestChecksumIntegrationReceiveFailures(t *testing.T) {
	for _, kind := range []string{"server", "tcp"} {
		for _, mode := range []string{"file", "stream"} {
			for _, failure := range []string{"truncated", "corrupted"} {
				t.Run(kind+"/"+mode+"/"+failure, func(t *testing.T) {
					input := "OK SIZE 6\nshort"
					if failure == "corrupted" {
						input = "OK SIZE 5\nshortCHECKSUM " + checksumIntegrationHex([]byte("other")) + "\n"
					}
					tr, _, _ := newChecksumIntegrationTransport(kind, input, nil)
					var err error
					if mode == "file" {
						err = tr.ReceiveFile(checksumIntegrationPath, filepath.Join(t.TempDir(), "received"))
					} else {
						err = tr.ReceiveStream(checksumIntegrationPath, io.Discard)
					}
					if failure == "truncated" {
						requireChecksumIntegrationTransferError(t, err, io.EOF)
					} else if err == nil || !strings.Contains(err.Error(), "checksum mismatch:") {
						t.Fatalf("want checksum mismatch, got %v", err)
					}
				})
			}
		}
	}
}

func TestChecksumIntegrationSendFailures(t *testing.T) {
	writeErr := errors.New("payload write failed")
	for _, kind := range []string{"server", "tcp"} {
		for _, mode := range []string{"file", "stream"} {
			for _, failure := range []string{"partial", "short-write", "corrupted"} {
				t.Run(kind+"/"+mode+"/"+failure, func(t *testing.T) {
					payload := []byte("payload")
					response := "OK " + checksumIntegrationHex([]byte("corrupt")) + " 7\n"
					var progress int64
					tr, conn, _ := newChecksumIntegrationTransport(kind, response, func(n int64) { progress += n })
					if failure != "corrupted" {
						conn.payloadLimit = 3
						if failure == "partial" {
							conn.payloadErr = writeErr
						}
					}
					var err error
					if mode == "file" {
						path := filepath.Join(t.TempDir(), "source")
						if err := os.WriteFile(path, payload, 0600); err != nil {
							t.Fatal(err)
						}
						err = tr.SendFile(path, checksumIntegrationPath)
					} else {
						err = tr.SendStream(bytes.NewReader(payload), checksumIntegrationPath, int64(len(payload)))
					}
					if failure == "corrupted" {
						if err == nil || !strings.Contains(err.Error(), "checksum mismatch:") {
							t.Fatalf("want checksum mismatch, got %v", err)
						}
					} else {
						cause := writeErr
						if failure == "short-write" {
							cause = io.ErrShortWrite
						}
						requireChecksumIntegrationTransferError(t, err, cause)
						if conn.input.Len() != len(response) {
							t.Fatal("read response after failed transfer")
						}
					}
					written := conn.output.Len() - len(checksumIntegrationCommand("SEND", len(payload)))
					if progress != int64(written) {
						t.Fatalf("progress = %d, successfully written = %d", progress, written)
					}
				})
			}
		}
	}
}
