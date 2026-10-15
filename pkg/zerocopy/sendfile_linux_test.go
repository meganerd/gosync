//go:build linux

package zerocopy

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestSendFileN(t *testing.T) {
	payload := bytes.Repeat([]byte("zero-copy"), 4096)
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan []byte, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			received <- nil
			return
		}
		defer conn.Close()
		got, _ := io.ReadAll(conn)
		received <- got
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, used, err := SendFileN(conn, source, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if !used || n != int64(len(payload)) {
		t.Fatalf("n=%d used=%v", n, used)
	}
	conn.Close()
	if got := <-received; !bytes.Equal(got, payload) {
		t.Fatal("received payload differs")
	}
}

func TestSendFileNPortableFallback(t *testing.T) {
	file, err := os.Open(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		file.Close()
		t.Fatal("unexpected file")
	}
	var dst bytes.Buffer
	tmp, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	if n, used, err := SendFileN(&dst, tmp, 0); n != 0 || used || err != nil {
		t.Fatalf("n=%d used=%v err=%v", n, used, err)
	}
}
