package server

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gbjohnso/gosync/pkg/transport"
)

func TestTCPArchiveDirectoryIntegration(t *testing.T) {
	s, _ := startTestServer(t, "tcp", 4096)
	base := s.baseDir
	addr := s.Addr().(*net.TCPAddr)
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "nested", "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "file"), []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	tx := transport.NewTCPTransport(transport.Config{Timeout: 5, Checksum: true})
	if err := tx.Connect("127.0.0.1", addr.Port); err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	res, err := tx.SendDirectory(context.Background(), src, "archive", transport.ArchiveOptions{Policy: "portable", BufferSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 3 {
		t.Fatalf("entries=%d", res.Entries)
	}
	b, err := os.ReadFile(filepath.Join(base, "archive", "nested", "file"))
	if err != nil || string(b) != "payload" {
		t.Fatalf("content=%q err=%v", b, err)
	}
}
