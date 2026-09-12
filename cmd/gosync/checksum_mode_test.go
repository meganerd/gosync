package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/server"
)

func TestChecksumModeCLIHelper(t *testing.T) {
	if os.Getenv("GOSYNC_CHECKSUM_HELPER") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("GOSYNC_CHECKSUM_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"gosync"}, args...)
	main()
	if got := flag.Lookup("checksum").DefValue; got != "false" {
		t.Fatalf("checksum default = %s, want false", got)
	}
	os.Exit(0)
}

func checksumModeCLI(t *testing.T, args []string, env ...string) ([]byte, error) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestChecksumModeCLIHelper$")
	cmd.Env = append(os.Environ(), "GOSYNC_CHECKSUM_HELPER=1", "GOSYNC_CHECKSUM_ARGS="+string(encoded))
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("CLI timed out: %v\n%s", ctx.Err(), out)
	}
	return out, err
}

func checksumModeReceiver(t *testing.T, protocol string) (*server.Server, string) {
	t.Helper()
	base := t.TempDir()
	receiver := server.NewServer("127.0.0.1:0", base)
	if err := receiver.SetTransport(protocol); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- receiver.Start() }()
	t.Cleanup(func() {
		receiver.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("receiver: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("receiver shutdown timed out")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for receiver.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("receiver startup timed out")
		}
		time.Sleep(time.Millisecond)
	}
	return receiver, base
}

type checksumModeWire struct {
	request, response string
	err               error
}

// Record both directions without replacing the real receiver's protocol handling.
func checksumModeTCPProxy(t *testing.T, upstream string) (string, <-chan checksumModeWire) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	done := make(chan checksumModeWire, 1)
	go func() {
		client, err := listener.Accept()
		if err != nil {
			done <- checksumModeWire{err: err}
			return
		}
		defer client.Close()
		remote, err := net.DialTimeout("tcp", upstream, 5*time.Second)
		if err != nil {
			done <- checksumModeWire{err: err}
			return
		}
		defer remote.Close()
		client.SetDeadline(time.Now().Add(15 * time.Second))
		remote.SetDeadline(time.Now().Add(15 * time.Second))
		var request, response bytes.Buffer
		requestDone := make(chan error, 1)
		go func() {
			_, err := io.Copy(remote, io.TeeReader(client, &request))
			remote.(*net.TCPConn).CloseWrite()
			requestDone <- err
		}()
		_, responseErr := io.Copy(client, io.TeeReader(remote, &response))
		requestErr := <-requestDone
		if responseErr == nil {
			responseErr = requestErr
		}
		done <- checksumModeWire{request.String(), response.String(), responseErr}
	}()
	return listener.Addr().String(), done
}

func assertChecksumModeWire(t *testing.T, done <-chan checksumModeWire, enabled bool, name string, payload []byte) {
	t.Helper()
	var wire checksumModeWire
	select {
	case wire = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not finish")
	}
	if wire.err != nil {
		t.Fatal(wire.err)
	}
	command, acknowledgement := "SEND-NOHASH", fmt.Sprintf("OK NONE %d\n", len(payload))
	handshake := "PING BASE\nCAPS\n"
	if enabled {
		command = "SEND"
		acknowledgement = fmt.Sprintf("OK %x %d\n", sha256.Sum256(payload), len(payload))
		handshake = "PING BASE\n"
	}
	want := handshake + fmt.Sprintf("%s %d %s\n", command, len(payload), base64.StdEncoding.EncodeToString([]byte(name))) + string(payload)
	// Close may send QUIT, but no other commands or payload bytes are allowed.
	if wire.request != want && wire.request != want+"QUIT\n" {
		t.Fatalf("checksum=%t: unexpected request (got %d bytes, want %d); prefix: %.100q", enabled, len(wire.request), len(want), wire.request)
	}
	if !strings.HasSuffix(wire.response, acknowledgement) {
		t.Fatalf("checksum=%t: receiver response = %q, want suffix %q", enabled, wire.response, acknowledgement)
	}
	if got := strings.Contains(wire.response, "OK CAPS NOHASH\n"); got != !enabled {
		t.Fatalf("checksum=%t: capability acknowledgement present=%t", enabled, got)
	}
}

// Like the deployment package's harness, intercept SSH/SCP through PATH. Never
// execute the remote startup or pkill commands: the test owns a local receiver.
func checksumModeDeploymentEnv(t *testing.T) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "commands")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	for _, tool := range []string{"ssh", "scp"} {
		script := "#!/bin/sh\nprintf '%s\\n' " + quote(tool) + " \"$@\" >> " + quote(log) + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}, log
}

func TestChecksumModeCLIEndpoints(t *testing.T) {
	for _, protocol := range []string{"tcp", "server", "quic"} {
		for _, deployment := range []bool{false, true} {
			// QUIC deployment bootstrap has its own certificate/SSH harness; this
			// suite covers pinned QUIC directly and deploy data wiring over TCP.
			if protocol == "quic" && deployment {
				continue
			}
			for _, mode := range []struct {
				name    string
				flags   []string
				enabled bool
			}{
				{"default", nil, false},
				{"true", []string{"-checksum=true"}, true},
				{"false", []string{"-checksum=false"}, false},
			} {
				t.Run(fmt.Sprintf("%s/deploy=%t/%s", protocol, deployment, mode.name), func(t *testing.T) {
					receiverProtocol := protocol
					if protocol == "server" {
						receiverProtocol = "tcp"
					}
					receiver, base := checksumModeReceiver(t, receiverProtocol)
					address := receiver.Addr().String()
					var wire <-chan checksumModeWire
					if receiverProtocol == "tcp" {
						address, wire = checksumModeTCPProxy(t, address)
					}
					payload := bytes.Repeat([]byte("checksum CLI payload\x00\xff\n"), 4097)
					source := filepath.Join(t.TempDir(), "source file.bin")
					if err := os.WriteFile(source, payload, 0600); err != nil {
						t.Fatal(err)
					}
					args := []string{"-transport", protocol, "-quiet", "-workers", "1"}
					args = append(args, mode.flags...)
					if protocol == "quic" {
						cert := filepath.Join(t.TempDir(), "receiver.pem")
						if err := os.WriteFile(cert, receiver.CertificatePEM(), 0600); err != nil {
							t.Fatal(err)
						}
						args = append(args, "-cert", cert)
					}
					name := "destination file.bin"
					destination := address + ":/" + name
					var env []string
					var log string
					if deployment {
						env, log = checksumModeDeploymentEnv(t)
						args = append(args, "-deploy", "-deploy-listen", address)
						destination = "tester@127.0.0.1:" + base
						name = filepath.Base(source)
					}
					args = append(args, source, destination)
					out, err := checksumModeCLI(t, args, env...)
					if err != nil {
						t.Fatalf("CLI: %v\n%s", err, out)
					}
					stored, err := os.ReadFile(filepath.Join(base, name))
					if err != nil || !bytes.Equal(stored, payload) {
						t.Fatalf("receiver payload differs: %v", err)
					}
					if wire != nil {
						assertChecksumModeWire(t, wire, mode.enabled, name, payload)
					}
					if deployment {
						commands, err := os.ReadFile(log)
						if err != nil {
							t.Fatal(err)
						}
						for _, want := range []string{"scp\n", "nohup ~/gosync serve", "--transport tcp", "StrictHostKeyChecking=yes"} {
							if !strings.Contains(string(commands), want) {
								t.Errorf("deployment commands missing %q: %s", want, commands)
							}
						}
					}
				})
			}
		}
	}
}

func TestChecksumModeCLIHelpAndSSH(t *testing.T) {
	out, err := checksumModeCLI(t, []string{"-help"})
	if err != nil {
		t.Fatalf("help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "-checksum") || !strings.Contains(string(out), "default off; QUIC TLS remains enabled") {
		t.Fatalf("help must describe checksum off independently of TLS: %s", out)
	}
	source := t.TempDir()
	for _, mode := range []struct {
		name  string
		flags []string
		valid bool
	}{
		{"default", nil, true},
		{"false", []string{"-checksum=false"}, true},
		{"true", []string{"-checksum=true"}, false},
		{"bare", []string{"-checksum"}, false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			args := append([]string{"-transport", "ssh", "-dry-run", "-quiet"}, mode.flags...)
			out, err := checksumModeCLI(t, append(args, source, "host:/target"))
			if mode.valid {
				if err != nil || len(out) != 0 {
					t.Fatalf("SSH checksum off: %v\n%s", err, out)
				}
			} else if err == nil || !strings.Contains(string(out), "SSH transport does not support application SHA-256 verification") {
				t.Fatalf("SSH checksum on should be rejected before connecting: %v\n%s", err, out)
			}
		})
	}
}
