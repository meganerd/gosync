//go:build linux

package server

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Limit file writes in a subprocess so a real partial export fails without
// changing process-wide limits or signal handling for the rest of the suite.
func TestTLSIncompleteExportCleanup(t *testing.T) {
	const childEnv = "GOSYNC_TEST_PARTIAL_CERT_EXPORT"
	if os.Getenv(childEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestTLSIncompleteExportCleanup$")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("partial export subprocess: %v\n%s", err, output)
		}
		return
	}
	signal.Ignore(syscall.SIGXFSZ)
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = 32
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	s := newTLSServer(t)
	out := filepath.Join(t.TempDir(), "partial.pem")
	if err := s.SetCertificateOutput(out); err != nil {
		t.Fatal(err)
	}
	// Reuse a known free address to verify the failed startup closes its socket.
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.listenAddr = probe.LocalAddr().String()
	probe.Close()
	if err := s.Start(); err == nil {
		s.Stop()
		t.Fatal("partial export succeeded")
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete export not removed: %v", err)
	}
	if s.Addr() != nil || s.CertificatePEM() != nil {
		t.Fatal("failed export published readiness")
	}
	// quic-go releases its internally owned UDP socket asynchronously.
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, err = net.ListenPacket("udp", s.listenAddr)
		if err == nil {
			probe.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed export leaked listener: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}
