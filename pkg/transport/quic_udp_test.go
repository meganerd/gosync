package transport_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/checksum"
	"github.com/gbjohnso/gosync/pkg/server"
	"github.com/gbjohnso/gosync/pkg/transport"
	"github.com/quic-go/quic-go"
)

const quicTestPath = "nested folder/tab\tnewline\n雪 invalid\xff.bin"

func encodedPath(path string) string { return base64.StdEncoding.EncodeToString([]byte(path)) }
func digestHex(p []byte) string      { return fmt.Sprintf("%x", sha256.Sum256(p)) }

// A real UDP peer for injecting replies the production server must never emit.
// Every accepted stream runs independently, and cleanup joins all peer goroutines.
func quicTestPeer(t *testing.T, config transport.Config, pong string, handle func(*quic.Stream) error) (*transport.QUICTransport, error) {
	t.Helper()
	cert := quicUDPTestCertificate(t)
	config.CertificatePEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	listener, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"gosync"}, MinVersion: tls.VersionTLS13,
	}, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if listener.Addr().Network() != "udp" {
		t.Fatal("QUIC listener is not UDP")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		defer conn.CloseWithError(0, "test complete")
		if conn.ConnectionState().TLS.NegotiatedProtocol != "gosync" {
			t.Error("wrong ALPN")
		}
		stop := context.AfterFunc(ctx, func() { conn.CloseWithError(0, "test canceled") })
		defer stop()
		ping, err := conn.AcceptStream(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		ping.SetDeadline(time.Now().Add(5 * time.Second))
		command, err := io.ReadAll(ping)
		if err != nil || string(command) != "PING BASE\n" {
			t.Errorf("PING = %q, %v", command, err)
			return
		}
		if _, err := io.WriteString(ping, pong); err != nil {
			t.Error(err)
			return
		}
		ping.Close()
		var workers sync.WaitGroup
		defer workers.Wait()
		for {
			stream, err := conn.AcceptStream(ctx)
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer stream.CancelRead(0)
				defer stream.Close()
				stream.SetDeadline(time.Now().Add(10 * time.Second))
				if err := handle(stream); err != nil {
					t.Error(err)
				}
			}()
		}
	}()
	client := transport.NewQUICTransport(config)
	t.Cleanup(func() {
		client.Close()
		cancel()
		listener.Close()
		select {
		case <-done:
		case <-time.After(12 * time.Second):
			t.Error("peer cleanup timed out")
		}
	})
	addr := listener.Addr().(*net.UDPAddr)
	err = client.Connect(addr.IP.String(), addr.Port)
	return client, err
}

func connectedQUICPeer(t *testing.T, config transport.Config, handle func(*quic.Stream) error) *transport.QUICTransport {
	t.Helper()
	client, err := quicTestPeer(t, config, "OK PONG "+encodedPath("/receiver base")+"\n", handle)
	if err != nil {
		t.Fatal(err)
	}
	if client.RemoteBase() != "/receiver base" || !client.IsConnected() {
		t.Fatal("missing connection/base discovery")
	}
	return client
}

func TestQUICSendAcknowledgement(t *testing.T) {
	payload := []byte("payload")
	good := "OK " + digestHex(payload) + " 7\n"
	for _, reply := range []string{good, "", "OK\n", "ERROR disk full\n", "OKAY " + digestHex(payload) + " 7\n", "OK ab 7\n", "OK " + digestHex(nil) + " 7\n", "OK " + digestHex(payload) + " 8\n", "OK " + digestHex(payload) + " nope\n", strings.TrimSuffix(good, "\n"), good[:len(good)-1] + " extra\n", strings.Repeat("x", 5000) + "\n"} {
		t.Run(fmt.Sprintf("reply=%q", reply[:min(90, len(reply))]), func(t *testing.T) {
			client := connectedQUICPeer(t, transport.Config{Checksum: true, Timeout: 3}, func(s *quic.Stream) error {
				wire, err := io.ReadAll(s)
				want := fmt.Sprintf("SEND 7 %s\npayload", encodedPath(quicTestPath))
				if err != nil || string(wire) != want {
					return fmt.Errorf("SEND wire (must have no trailer): %q, %v", wire, err)
				}
				_, err = io.WriteString(s, reply)
				return err
			})
			var progress int64
			client.SetProgressCallback(func(n int64) { progress += n })
			source := strings.NewReader("payloadDO NOT CONSUME")
			err := client.SendStream(source, quicTestPath, 7)
			if (err == nil) != (reply == good) {
				t.Fatalf("SendStream = %v", err)
			}
			if progress != 7 || source.Len() != len("DO NOT CONSUME") {
				t.Fatalf("progress=%d remaining=%d", progress, source.Len())
			}
		})
	}
}

func TestQUICReceiveReplies(t *testing.T) {
	good := "OK SIZE 7\npayloadCHECKSUM " + digestHex([]byte("payload")) + "\n"
	for _, reply := range []string{good, "", "ERROR missing file\n", "OK SIZE -1\n", "OK SIZE nope\n", "OK SIZE 7 extra\n", "OK SIZES 7\n", "OK SIZE 8\nshort", "OK SIZE 7\npayload", "OK SIZE 7\npayloadBOGUS hash\n", "OK SIZE 7\npayloadCHECKSUM ab\n", "OK SIZE 7\npayloadCHECKSUM " + digestHex(nil) + "\n", strings.TrimSuffix(good, "\n")} {
		t.Run(fmt.Sprintf("reply=%q", reply), func(t *testing.T) {
			client := connectedQUICPeer(t, transport.Config{Checksum: true, Timeout: 3}, func(s *quic.Stream) error {
				wire, err := io.ReadAll(s)
				if err != nil || string(wire) != "RECEIVE "+encodedPath(quicTestPath)+"\n" {
					return fmt.Errorf("RECEIVE wire: %q, %v", wire, err)
				}
				_, err = io.WriteString(s, reply)
				return err
			})
			var dst bytes.Buffer
			err := client.ReceiveStream(quicTestPath, &dst)
			if (err == nil) != (reply == good) {
				t.Fatalf("ReceiveStream = %v", err)
			}
			if err == nil && dst.String() != "payload" {
				t.Fatalf("payload = %q", dst.String())
			}
		})
	}
}

func TestQUICMalformedPong(t *testing.T) {
	for _, pong := range []string{"", "OK PONG\n", "OK PONG !!!\n", "OK PONG " + encodedPath("relative") + "\n", "OK PONG " + encodedPath("/base") + " extra\n"} {
		t.Run(pong, func(t *testing.T) {
			client, err := quicTestPeer(t, transport.Config{Checksum: true, Timeout: 2}, pong, func(*quic.Stream) error { return nil })
			if err == nil || client.IsConnected() || client.RemoteBase() != "" {
				t.Fatalf("malformed PONG accepted: %v", err)
			}
		})
	}
}

func TestQUICTruncatedSourceResetsStream(t *testing.T) {
	reset := make(chan error, 1)
	client := connectedQUICPeer(t, transport.Config{Checksum: true, Timeout: 3}, func(s *quic.Stream) error {
		_, err := io.ReadAll(s)
		reset <- err
		return nil
	})
	if err := client.SendStream(strings.NewReader("short"), "file", 100); !errors.Is(err, io.EOF) {
		t.Fatalf("SendStream = %v, want EOF", err)
	}
	select {
	case err := <-reset:
		var streamErr *quic.StreamError
		if !errors.As(err, &streamErr) || !streamErr.Remote {
			t.Fatalf("peer read = %v, want remote reset", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("truncated source left peer blocked")
	}
}

type failingQUICWriter struct{}

func (failingQUICWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestQUICWriterFailureResetsStream(t *testing.T) {
	reset := make(chan error, 1)
	client := connectedQUICPeer(t, transport.Config{Checksum: true, Timeout: 3}, func(s *quic.Stream) error {
		if _, err := io.ReadAll(s); err != nil {
			return err
		}
		if _, err := io.WriteString(s, "OK SIZE 100000000\n"); err != nil {
			return err
		}
		_, err := io.CopyN(s, &zeroQUICReader{}, 100000000)
		reset <- err
		return nil
	})
	if err := client.ReceiveStream("file", failingQUICWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("ReceiveStream = %v", err)
	}
	select {
	case err := <-reset:
		var streamErr *quic.StreamError
		if !errors.As(err, &streamErr) || !streamErr.Remote {
			t.Fatalf("peer write = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failed writer left peer blocked")
	}
}

type zeroQUICReader struct{}

func (*zeroQUICReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestQUICConcurrentStreams(t *testing.T) {
	const workers = 8
	arrived := make(chan struct{}, workers)
	release := make(chan struct{})
	defer close(release)
	client := connectedQUICPeer(t, transport.Config{Checksum: true, Timeout: 5}, func(s *quic.Stream) error {
		wire, err := io.ReadAll(s)
		if err != nil {
			return err
		}
		arrived <- struct{}{}
		select {
		case <-release:
		case <-time.After(4 * time.Second):
			return fmt.Errorf("operations were serialized")
		}
		_, err = fmt.Fprintf(s, "OK %s 1\n", digestHex(wire[len(wire)-1:]))
		return err
	})
	var progress atomic.Int64
	client.SetProgressCallback(func(n int64) { progress.Add(n) })
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) { results <- client.SendStream(strings.NewReader("x"), fmt.Sprint(i), 1) }(i)
	}
	for i := 0; i < workers; i++ {
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent operations did not reach peer independently")
		}
	}
	// A separate release channel per test avoids sleeps for concurrency assertions.
	for i := 0; i < workers; i++ {
		release <- struct{}{}
	}
	for i := 0; i < workers; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if progress.Load() != workers {
		t.Fatalf("progress = %d", progress.Load())
	}
}

func TestQUICIdleDeadlineAndClose(t *testing.T) {
	for _, closeClient := range []bool{false, true} {
		t.Run(fmt.Sprint(closeClient), func(t *testing.T) {
			ready := make(chan struct{})
			client := connectedQUICPeer(t, transport.Config{Checksum: true, Timeout: 1}, func(s *quic.Stream) error {
				_, err := io.ReadAll(s)
				close(ready)
				if err != nil {
					return err
				}
				<-s.Context().Done()
				return nil
			})
			done := make(chan error, 1)
			go func() { done <- client.SendStream(strings.NewReader("x"), "file", 1) }()
			<-ready
			if closeClient {
				client.Close()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("missing acknowledgement succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("blocked operation was not canceled")
			}
		})
	}
}

func TestQUICActiveTransferExceedsTimeout(t *testing.T) {
	payload := []byte("123456")
	client := connectedQUICPeer(t, transport.Config{Checksum: true, Timeout: 1}, func(s *quic.Stream) error {
		if _, err := io.ReadAll(s); err != nil {
			return err
		}
		if _, err := io.WriteString(s, "OK SIZE 6\n"); err != nil {
			return err
		}
		for _, b := range payload {
			time.Sleep(250 * time.Millisecond)
			if _, err := s.Write([]byte{b}); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(s, "CHECKSUM %s\n", digestHex(payload))
		return err
	})
	var dst bytes.Buffer
	if err := client.ReceiveStream("slow file", &dst); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Fatal("slow transfer payload mismatch")
	}
}

func TestQUICInvalidBuffer(t *testing.T) {
	for _, size := range []int{-1, checksum.MinBufferSize - 1, checksum.MaxBufferSize + 1} {
		client := transport.NewQUICTransport(transport.Config{BufferSize: size})
		if err := client.Connect("invalid:address", -1); !errors.Is(err, checksum.ValidateBufferSize(size)) {
			t.Fatalf("buffer=%d: %v", size, err)
		}
	}
}

type measuredQUICReader struct {
	io.Reader
	maxRead int
}

func (r *measuredQUICReader) Read(p []byte) (int, error) {
	r.maxRead = max(r.maxRead, len(p))
	return r.Reader.Read(p)
}

type measuredQUICWriter struct {
	bytes.Buffer
	maxWrite int
}

func (w *measuredQUICWriter) Write(p []byte) (int, error) {
	w.maxWrite = max(w.maxWrite, len(p))
	return w.Buffer.Write(p)
}

func TestQUICProductionReceiverIntegration(t *testing.T) {
	for _, bufferSize := range []int{0, checksum.MinBufferSize, 256 * 1024} {
		t.Run(fmt.Sprintf("buffer=%d", bufferSize), func(t *testing.T) {
			base := t.TempDir()
			s := server.NewServer("127.0.0.1:0", base)
			if err := s.SetTransport("quic"); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- s.Start() }()
			t.Cleanup(func() {
				s.Stop()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("server did not stop")
				}
			})
			deadline := time.Now().Add(5 * time.Second)
			for s.Addr() == nil {
				select {
				case err := <-done:
					t.Fatalf("server startup: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("server startup timed out")
				}
				time.Sleep(time.Millisecond)
			}
			addr := s.Addr().(*net.UDPAddr)
			client := transport.NewQUICTransport(transport.Config{Checksum: true, Timeout: 5, BufferSize: bufferSize, CertificatePEM: s.CertificatePEM()})
			t.Cleanup(func() { client.Close() })
			if err := client.Connect(addr.IP.String(), addr.Port); err != nil {
				t.Fatal(err)
			}
			if client.RemoteBase() != base {
				t.Fatalf("RemoteBase = %q", client.RemoteBase())
			}
			effective := bufferSize
			if effective == 0 {
				effective = checksum.DefaultBufferSize
			}
			payload := bytes.Repeat([]byte{0, 255, '\n', 42}, effective+17)
			source := &measuredQUICReader{Reader: bytes.NewReader(payload)}
			if err := client.SendStream(source, quicTestPath, int64(len(payload))); err != nil {
				t.Fatal(err)
			}
			if source.maxRead != effective {
				t.Fatalf("send buffer=%d want=%d", source.maxRead, effective)
			}
			var dst measuredQUICWriter
			if err := client.ReceiveStream(quicTestPath, &dst); err != nil {
				t.Fatal(err)
			}
			if dst.maxWrite > effective || !bytes.Equal(dst.Bytes(), payload) {
				t.Fatal("receive buffer/payload mismatch")
			}
			stored, err := os.ReadFile(filepath.Join(base, quicTestPath))
			if err != nil || !bytes.Equal(stored, payload) {
				t.Fatalf("stored payload: %v", err)
			}
			local := filepath.Join(t.TempDir(), "local file")
			if err := os.WriteFile(local, payload, 0600); err != nil {
				t.Fatal(err)
			}
			if err := client.SendFile(local, "file copy"); err != nil {
				t.Fatal(err)
			}
			if err := client.ReceiveFile("file copy", local); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(local)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("file round trip: %v", err)
			}
			if err := client.SendStream(strings.NewReader(""), "empty", 0); err != nil {
				t.Fatal(err)
			}
			if err := client.ReceiveFile("empty", local); err != nil {
				t.Fatal(err)
			}
			stat, err := os.Stat(local)
			if err != nil || stat.Size() != 0 || stat.Mode().Perm() != 0600 {
				t.Fatalf("receive file truncation/mode: %v, %v", stat, err)
			}
			if err := client.ReceiveStream("missing", io.Discard); err == nil {
				t.Fatal("missing file succeeded")
			}
			if err := client.SendStream(strings.NewReader("x"), "nested folder", 1); err == nil {
				t.Fatal("sending over an existing directory succeeded")
			}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					path := fmt.Sprintf("worker/%d", i)
					if err := client.SendStream(bytes.NewReader(payload), path, int64(len(payload))); err != nil {
						t.Error(err)
						return
					}
					var out bytes.Buffer
					if err := client.ReceiveStream(path, &out); err != nil {
						t.Error(err)
						return
					}
					if !bytes.Equal(out.Bytes(), payload) {
						t.Error("worker payload mismatch")
					}
				}(i)
			}
			wg.Wait()
			t.Log("production receiver integration succeeded: UDP QUIC, discovery, file/stream round trips, concurrent workers")
		})
	}
}

func quicUDPTestCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
