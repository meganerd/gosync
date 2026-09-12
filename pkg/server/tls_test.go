package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func tlsFixture(t *testing.T, expired bool) (string, string, []byte) {
	t.Helper()
	config, err := ephemeralTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	cert := config.Certificates[0]
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	if expired {
		leaf.NotBefore = time.Now().Add(-48 * time.Hour)
		leaf.NotAfter = time.Now().Add(-24 * time.Hour)
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, leaf.PublicKey, cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	// Include a second chain entry to ensure export contains only the leaf.
	if err := os.WriteFile(certPath, append(append([]byte(nil), public...), public...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, public
}

func newTLSServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer("127.0.0.1:0", t.TempDir())
	if err := s.SetTransport("quic"); err != nil {
		t.Fatal(err)
	}
	return s
}

func runTLSServer(t *testing.T, s *Server) {
	t.Helper()
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
			t.Error("Start did not exit")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for s.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("server not ready")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTLSConfiguration(t *testing.T) {
	s := newTLSServer(t)
	for _, pair := range [][2]string{{"cert", ""}, {"", "key"}} {
		if s.SetTLSFiles(pair[0], pair[1]) == nil {
			t.Fatal("accepted incomplete pair")
		}
	}
	if s.SetTLSFiles("", "") != nil {
		t.Fatal("rejected empty pair")
	}
	if s.CertificatePEM() != nil {
		t.Fatal("certificate before startup")
	}
	runTLSServer(t, s)
	if s.SetTLSFiles("", "") == nil || s.SetCertificateOutput("") == nil {
		t.Fatal("configuration allowed after Start")
	}
	s.Stop()
	if s.SetTLSFiles("", "") == nil || s.SetCertificateOutput("") == nil {
		t.Fatal("configuration allowed after Stop")
	}
}

func TestTLSInvalidFiles(t *testing.T) {
	cert, key, _ := tlsFixture(t, false)
	_, otherKey, _ := tlsFixture(t, false)
	bad := filepath.Join(t.TempDir(), "malformed")
	if err := os.WriteFile(bad, []byte("not PEM"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"mismatch": {cert, otherKey}, "bad cert": {bad, key}, "bad key": {cert, bad},
		"missing cert": {bad + "missing", key}, "missing key": {cert, bad + "missing"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newTLSServer(t)
			out := filepath.Join(t.TempDir(), "out.pem")
			if err := s.SetTLSFiles(pair[0], pair[1]); err != nil {
				t.Fatal(err)
			}
			if err := s.SetCertificateOutput(out); err != nil {
				t.Fatal(err)
			}
			if err := s.Start(); err == nil {
				t.Fatal("invalid TLS accepted")
			}
			if s.Addr() != nil || s.CertificatePEM() != nil {
				t.Fatal("failed server published readiness")
			}
			if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("export on failure: %v", err)
			}
		})
	}
}

func TestTLSCertificateExportAndPresentation(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated", true: "custom"}[custom], func(t *testing.T) {
			s := newTLSServer(t)
			var want, originalCert, originalKey []byte
			var certPath, keyPath string
			if custom {
				certPath, keyPath, want = tlsFixture(t, false)
				originalCert, _ = os.ReadFile(certPath)
				originalKey, _ = os.ReadFile(keyPath)
				if err := s.SetTLSFiles(certPath, keyPath); err != nil {
					t.Fatal(err)
				}
			}
			out := filepath.Join(t.TempDir(), "public.pem")
			if err := s.SetCertificateOutput(out); err != nil {
				t.Fatal(err)
			}
			runTLSServer(t, s)
			public := s.CertificatePEM()
			if len(public) == 0 {
				t.Fatal("Addr ready without certificate")
			}
			exported, err := os.ReadFile(out)
			if err != nil || !bytes.Equal(exported, public) {
				t.Fatalf("export differs: %v", err)
			}
			info, err := os.Stat(out)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("export permissions: %v %v", info, err)
			}
			block, rest := pem.Decode(public)
			if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
				t.Fatal("export is not a single public certificate")
			}
			public[0] ^= 1
			if !bytes.Equal(s.CertificatePEM(), exported) {
				t.Fatal("CertificatePEM aliases server state")
			}
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(exported)
			config := &tls.Config{NextProtos: []string{"gosync"}, RootCAs: roots}
			if !custom {
				// A generated identity has no hostname; verify the explicitly trusted
				// certificate and its validity instead of DNS identity.
				config.InsecureSkipVerify = true
				config.VerifyConnection = func(state tls.ConnectionState) error {
					_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots})
					return err
				}
			}
			for i := 0; i < 2; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				conn, err := quic.DialAddr(ctx, s.Addr().String(), config, nil)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(conn.ConnectionState().TLS.PeerCertificates[0].Raw, block.Bytes) {
					t.Fatal("presented certificate differs")
				}
				conn.CloseWithError(0, "done")
			}
			if custom {
				if !bytes.Equal(want, exported) {
					t.Fatal("custom leaf not selected")
				}
				gotCert, _ := os.ReadFile(certPath)
				gotKey, _ := os.ReadFile(keyPath)
				if !bytes.Equal(gotCert, originalCert) || !bytes.Equal(gotKey, originalKey) {
					t.Fatal("supplied files changed")
				}
			}
		})
	}
}

func TestTLSExportRefusesExisting(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "dangling", "certificate", "key", "directory"} {
		t.Run(kind, func(t *testing.T) {
			s := newTLSServer(t)
			cert, key, _ := tlsFixture(t, false)
			if err := s.SetTLSFiles(cert, key); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "out")
			target := filepath.Join(t.TempDir(), "target")
			if kind != "dangling" {
				if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "file":
				if err := os.WriteFile(out, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink", "dangling":
				if err := os.Symlink(target, out); err != nil {
					t.Fatal(err)
				}
			case "certificate":
				out = cert
			case "key":
				out = key
			case "directory":
				if err := os.Mkdir(out, 0700); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(out)
			if err := s.SetCertificateOutput(out); err != nil {
				t.Fatal(err)
			}
			if err := s.Start(); err == nil {
				t.Fatal("overwrote existing output")
			}
			if s.Addr() != nil || s.CertificatePEM() != nil {
				t.Fatal("published failed listener")
			}
			after, _ := os.ReadFile(out)
			if !bytes.Equal(before, after) {
				t.Fatal("existing contents changed")
			}
			if _, err := os.Lstat(out); err != nil {
				t.Fatal("removed existing output")
			}
			if kind == "dangling" {
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("created symlink target")
				}
			}
		})
	}
}

func TestTLSBindFailureDoesNotExport(t *testing.T) {
	occupied, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	s := newTLSServer(t)
	s.listenAddr = occupied.LocalAddr().String()
	out := filepath.Join(t.TempDir(), "cert.pem")
	if err := s.SetCertificateOutput(out); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("bound occupied address")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exported before bind")
	}
}

func TestTLSTCPRejectsConfiguration(t *testing.T) {
	for _, files := range []bool{false, true} {
		s := NewServer("127.0.0.1:0", t.TempDir())
		if files {
			if err := s.SetTLSFiles("not-read-cert", "not-read-key"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := s.SetCertificateOutput(filepath.Join(t.TempDir(), "out")); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Start(); err == nil {
			s.Stop()
			t.Fatal("TCP accepted TLS configuration")
		}
		if s.Addr() != nil || s.CertificatePEM() != nil {
			t.Fatal("TCP TLS readiness published")
		}
	}
	s, _ := startTestServer(t, "tcp", 0)
	if s.CertificatePEM() != nil {
		t.Fatal("TCP has certificate")
	}
}

func TestTLSExpiredCertificateRejectedByClient(t *testing.T) {
	cert, key, public := tlsFixture(t, true)
	s := newTLSServer(t)
	if err := s.SetTLSFiles(cert, key); err != nil {
		t.Fatal(err)
	}
	runTLSServer(t, s)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(public)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, s.Addr().String(), &tls.Config{RootCAs: roots, NextProtos: []string{"gosync"}}, nil)
	if err == nil {
		conn.CloseWithError(0, "done")
		t.Fatal("client accepted expired certificate")
	}
	var invalid x509.CertificateInvalidError
	if !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		t.Fatalf("expected expiry failure, got %v", err)
	}
}
