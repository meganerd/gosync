package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/checksum"
	"github.com/quic-go/quic-go"
)

func startTestServer(t *testing.T, protocol string, size int) (*Server, <-chan error) {
	t.Helper()
	s := NewServer("127.0.0.1:0", t.TempDir())
	if protocol != "" {
		if err := s.SetTransport(protocol); err != nil {
			t.Fatal(err)
		}
	}
	if size != 0 {
		if err := s.SetBufferSize(size); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- s.Start() }()
	t.Cleanup(func() {
		stopped := make(chan struct{})
		go func() { s.Stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("Stop deadlocked")
		}
	})
	until := time.Now().Add(5 * time.Second)
	for s.Addr() == nil {
		select {
		case err := <-done:
			t.Fatalf("Start: %v", err)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("listener not ready")
		}
		time.Sleep(time.Millisecond)
	}
	return s, done
}

func dialRawQUIC(t *testing.T, s *Server) *quic.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, s.Addr().String(), &tls.Config{
		InsecureSkipVerify: true, // Raw protocol tests; authentication is covered in tls_test.go.
		NextProtos:         []string{"gosync"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseWithError(0, "test complete") })
	if conn.ConnectionState().TLS.NegotiatedProtocol != "gosync" {
		t.Fatal("wrong ALPN")
	}
	return conn
}

func rawOperation(conn *quic.Conn, command string, payload []byte, fin bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.CancelRead(0)
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.Copy(stream, io.MultiReader(strings.NewReader(command+"\n"), bytes.NewReader(payload))); err != nil {
		return nil, err
	}
	if fin {
		if err := stream.Close(); err != nil {
			return nil, err
		}
	}
	// Reading through FIN also checks that server Close doesn't reset its output.
	return io.ReadAll(stream)
}

func rawTransfer(conn *quic.Conn, name string, payload []byte) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(name))
	digest := sha256.Sum256(payload)
	// Deliberately leave the write side open: SEND must not wait for a trailer or FIN.
	reply, err := rawOperation(conn, fmt.Sprintf("SEND %d %s", len(payload), encoded), payload, false)
	if err != nil {
		return err
	}
	if want := fmt.Sprintf("OK %x %d\n", digest, len(payload)); string(reply) != want {
		return fmt.Errorf("SEND: %q, want %q", reply, want)
	}
	reply, err = rawOperation(conn, "RECEIVE "+encoded, nil, true)
	if err != nil {
		return err
	}
	want := append([]byte(fmt.Sprintf("OK SIZE %d\n", len(payload))), payload...)
	want = append(want, []byte(fmt.Sprintf("CHECKSUM %x\n", digest))...)
	if !bytes.Equal(reply, want) {
		return fmt.Errorf("RECEIVE framing or payload mismatch: %d bytes, want %d", len(reply), len(want))
	}
	return nil
}

func TestQUICProtocolBuffers(t *testing.T) {
	for _, size := range []int{0, checksum.MinBufferSize, 256 * 1024, checksum.MaxBufferSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, _ := startTestServer(t, "quic", size)
			conn := dialRawQUIC(t, s)
			reply, err := rawOperation(conn, "PING BASE", nil, false)
			base, _ := filepath.Abs(s.baseDir)
			if want := "OK PONG " + base64.StdEncoding.EncodeToString([]byte(base)) + "\n"; err != nil || string(reply) != want {
				t.Fatalf("PING BASE: %q, %v", reply, err)
			}
			payload := make([]byte, 4*s.bufferSize+17)
			for i := range payload {
				payload[i] = byte(i*31 + i/251)
			}
			name := "nested folder/binary file \xff.dat"
			if err := rawTransfer(conn, name, payload); err != nil {
				t.Fatal(err)
			}
			stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
			if err != nil || !bytes.Equal(stored, payload) {
				t.Fatalf("stored payload differs: %v", err)
			}
			if err := rawTransfer(conn, "empty file", nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQUICErrorsAndTruncation(t *testing.T) {
	s, _ := startTestServer(t, "quic", 0)
	conn := dialRawQUIC(t, s)
	for _, command := range []string{"BOGUS", "SEND", "SEND nope eA==", "SEND -1 eA==", "SEND 1 !!!", "RECEIVE", "RECEIVE !!!", "RECEIVE bWlzc2luZw=="} {
		reply, err := rawOperation(conn, command, nil, false)
		if err != nil || !strings.HasPrefix(string(reply), "ERROR ") {
			t.Fatalf("%s: %q, %v", command, reply, err)
		}
	}
	reply, err := rawOperation(conn, "SEND 100 c2hvcnQ=", []byte{0, 1, 2}, true)
	if err != nil || !strings.HasPrefix(string(reply), "ERROR receive failed:") {
		t.Fatalf("truncated SEND: %q, %v", reply, err)
	}
	// A failed operation must not poison other streams on the session.
	reply, err = rawOperation(conn, "PING", nil, false)
	if err != nil || string(reply) != "OK PONG\n" {
		t.Fatalf("PING after errors: %q, %v", reply, err)
	}
}

func TestQUICParallelStreams(t *testing.T) {
	s, _ := startTestServer(t, "quic", checksum.MinBufferSize)
	conn := dialRawQUIC(t, s)
	// Hold multiple streams from the same address open to check identity tracking.
	var held []*quic.Stream
	for i := 0; i < 8; i++ {
		stream, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		stream.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := stream.Write([]byte("PI")); err != nil {
			t.Fatal(err)
		}
		held = append(held, stream)
	}
	until := time.Now().Add(5 * time.Second)
	for s.ClientCount() != len(held) {
		if time.Now().After(until) {
			t.Fatalf("tracked %d streams, want %d", s.ClientCount(), len(held))
		}
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			failures <- rawTransfer(conn, fmt.Sprintf("parallel/file %d", i), bytes.Repeat([]byte{byte(i), 0, 255, '\n'}, 32769))
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Error(err)
		}
	}
	for _, stream := range held {
		if _, err := stream.Write([]byte("NG\n")); err != nil {
			t.Fatal(err)
		}
		reply, err := io.ReadAll(stream)
		if err != nil || string(reply) != "OK PONG\n" {
			t.Fatalf("held stream: %q, %v", reply, err)
		}
		stream.Close()
	}
}

func TestServerTransportAndShutdown(t *testing.T) {
	for _, protocol := range []string{"", "tcp", "quic"} {
		t.Run("transport="+protocol, func(t *testing.T) {
			s, done := startTestServer(t, protocol, 0)
			if err := s.SetTransport("tcp"); err == nil {
				t.Fatal("changed running transport")
			}
			if err := s.SetBufferSize(checksum.MinBufferSize); err == nil {
				t.Fatal("changed running buffers")
			}
			if err := s.Start(); err == nil {
				t.Fatal("started twice")
			}
			var tcp net.Conn
			var qc *quic.Conn
			if protocol == "quic" {
				if _, ok := s.Addr().(*net.UDPAddr); !ok {
					t.Fatal("QUIC did not bind UDP")
				}
				qc = dialRawQUIC(t, s)
				// An idle session must also be closed, even without any streams.
				dialRawQUIC(t, s)
				if err := os.WriteFile(filepath.Join(s.baseDir, "large"), make([]byte, 8*1024*1024), 0600); err != nil {
					t.Fatal(err)
				}
				for _, command := range []string{"PI", "SEND 100000 c3RhbGxlZA==\n", "RECEIVE bGFyZ2U=\n"} {
					stream, err := qc.OpenStreamSync(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := stream.Write([]byte(command)); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				var err error
				tcp, err = net.DialTimeout("tcp", s.Addr().String(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer tcp.Close()
				tcp.SetDeadline(time.Now().Add(5 * time.Second))
				fmt.Fprint(tcp, "PING\n")
				line, err := bufio.NewReader(tcp).ReadString('\n')
				if err != nil || line != "OK PONG\n" {
					t.Fatalf("TCP default: %q %v", line, err)
				}
			}
			wantClients := 1
			if protocol == "quic" {
				wantClients = 3
			}
			until := time.Now().Add(5 * time.Second)
			for s.ClientCount() != wantClients {
				if time.Now().After(until) {
					t.Fatalf("active clients = %d, want %d", s.ClientCount(), wantClients)
				}
				time.Sleep(time.Millisecond)
			}
			// Exercise synchronized Addr and idempotent concurrent Stop.
			stopped := make(chan struct{})
			go func() {
				var wg sync.WaitGroup
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); s.Addr(); s.Stop() }()
				}
				wg.Wait()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("Stop deadlocked")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("accept loop did not exit")
			}
			if s.ClientCount() != 0 {
				t.Fatal("active streams remain")
			}
			s.mu.RLock()
			sessions := len(s.quicConns)
			s.mu.RUnlock()
			if sessions != 0 {
				t.Fatal("active QUIC sessions remain")
			}
			if tcp != nil {
				if _, err := tcp.Read(make([]byte, 1)); err == nil {
					t.Fatal("TCP connection still open")
				}
			}
			if qc != nil {
				select {
				case <-qc.Context().Done():
				case <-time.After(5 * time.Second):
					t.Fatal("QUIC connection still open")
				}
			}
		})
	}
	s := NewServer("127.0.0.1:0", t.TempDir())
	for _, value := range []string{"", "udp", "QUIC", " tcp"} {
		if s.SetTransport(value) == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if s.protocol != "tcp" {
		t.Fatal("invalid selection changed default")
	}
	s.Stop()
	if err := s.Start(); err == nil {
		t.Fatal("started stopped server")
	}
}

func TestQUICEphemeralTLS(t *testing.T) {
	first, err := ephemeralTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	second, err := ephemeralTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if first.MinVersion != tls.VersionTLS13 || len(first.NextProtos) != 1 || first.NextProtos[0] != "gosync" {
		t.Fatal("TLS configuration mismatch")
	}
	der := first.Certificates[0].Certificate[0]
	if bytes.Equal(der, second.Certificates[0].Certificate[0]) {
		t.Fatal("certificate not ephemeral")
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		t.Fatal(err)
	}
	if time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
		t.Fatal("certificate not valid now")
	}
}

func TestQUICStreamConnDeadlinesAndClose(t *testing.T) {
	config, err := ephemeralTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := listenQUIC("127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := quic.DialAddr(ctx, listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"gosync"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseWithError(0, "done")
	session, err := listener.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.CloseWithError(0, "done")
	stream, err := client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := stream.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	accepted, err := session.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &quicStreamConn{Stream: accepted, conn: session}
	defer adapter.Close()
	if adapter.LocalAddr().String() != listener.Addr().String() || adapter.RemoteAddr().(*net.UDPAddr).Port != client.LocalAddr().(*net.UDPAddr).Port {
		t.Fatal("adapter addresses mismatch")
	}
	if _, err := io.ReadFull(adapter, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	timeout := func(err error) {
		t.Helper()
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("expected deadline error, got %v", err)
		}
	}
	past := time.Now().Add(-time.Second)
	if err := adapter.SetReadDeadline(past); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Read(make([]byte, 1))
	timeout(err)
	if err := adapter.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SetWriteDeadline(past); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Write([]byte("not sent"))
	timeout(err)
	if err := adapter.SetDeadline(past); err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Read(make([]byte, 1))
	timeout(err)
	if err := adapter.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	const response = "OK final checksum 0\n"
	if _, err := io.WriteString(adapter, response); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(stream)
	if err != nil || string(got) != response {
		t.Fatalf("Close lost output: %q, %v", got, err)
	}
	if _, err := adapter.Read(make([]byte, 1)); err == nil {
		t.Fatal("Close did not cancel read")
	}
}

func TestClosedListenersExit(t *testing.T) {
	for _, protocol := range []string{"tcp", "quic"} {
		t.Run(protocol, func(t *testing.T) {
			s, done := startTestServer(t, protocol, 0)
			// Close without Stop to exercise unexpected permanent Accept errors too.
			s.mu.RLock()
			listener, qlistener := s.listener, s.quicListener
			s.mu.RUnlock()
			if listener != nil {
				listener.Close()
			} else {
				qlistener.Close()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected listener error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("closed listener accept loop did not exit")
			}
		})
	}
}

func TestServerConcurrentStartStop(t *testing.T) {
	for _, protocol := range []string{"tcp", "quic"} {
		for i := 0; i < 20; i++ {
			s := NewServer("127.0.0.1:0", t.TempDir())
			if err := s.SetTransport(protocol); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { s.Start(); close(done) }()
			s.Addr()
			s.Stop()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent Start/Stop deadlocked")
			}
		}
	}
}
