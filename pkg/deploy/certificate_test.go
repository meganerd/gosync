package deploy

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRemoteTLSFiles(t *testing.T) {
	d := NewDeployer("", "host", "")
	if err := d.SetRemoteTLSFiles("~/cert's file.pem", "~/key's file.pem"); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"cert", ""}, {"", "key"}, {"bad\x00cert", "key"}} {
		if err := d.SetRemoteTLSFiles(pair[0], pair[1]); err == nil {
			t.Fatalf("accepted invalid TLS pair %q", pair)
		}
		if d.remoteCert != "~/cert's file.pem" || d.remoteKey != "~/key's file.pem" {
			t.Fatal("invalid setter changed previous configuration")
		}
	}
	if err := d.SetRemoteTLSFiles("", ""); err != nil || d.remoteCert != "" || d.remoteKey != "" {
		t.Fatal("could not reset TLS files")
	}
}

func TestQuoteRemotePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, path := range []string{"~", "~/", "~/dir with spaces/a'b.pem", "/tmp/a'b c", "$(touch injected); `false` $HOME", "-option", "", "~other/file", "line\nbreak"} {
		t.Run(path, func(t *testing.T) {
			output, err := exec.Command("sh", "-c", "printf '%s' "+quoteRemotePath(path)).Output()
			if err != nil {
				t.Fatal(err)
			}
			want := path
			if path == "~" {
				want = home
			} else if strings.HasPrefix(path, "~/") {
				want = home + "/" + path[2:]
			}
			if string(output) != want {
				t.Fatalf("round trip %q = %q, want %q", path, output, want)
			}
		})
	}
}

func TestRemoteServerTLSArguments(t *testing.T) {
	d := NewDeployer("", "host:~/base's directory", "")
	if err := d.parseDestination(); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTransport("quic"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRemoteTLSFiles("~/TLS/cert's file", "/keys/key's file"); err != nil {
		t.Fatal(err)
	}
	d.certExportDir = "/tmp/gosync-cert.0123456789"
	command := d.remoteServerCommand()
	for _, flag := range []string{
		"--base " + quoteRemotePath(d.baseDir),
		"--cert-out " + quoteRemotePath(d.certExportDir+"/certificate.pem"),
		"--cert " + quoteRemotePath(d.remoteCert),
		"--key " + quoteRemotePath(d.remoteKey),
	} {
		if !strings.Contains(command, flag) {
			t.Fatalf("missing %s in %s", flag, command)
		}
	}
	if err := d.SetTransport("tcp"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.remoteServerCommand(), "--cert") || strings.Contains(d.remoteServerCommand(), "--key") {
		t.Fatal("TCP command includes TLS flags")
	}
}

func testCertificate(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// Fake SSH/SCP subprocesses exercise the real exec path and local size limits.
// Only export creation, reading, and scoped cleanup execute shell commands;
// existing broad process cleanup and server startup are deliberately simulated.
func TestDeploymentSSHHelper(t *testing.T) {
	if os.Getenv("GOSYNC_DEPLOY_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	log, err := os.OpenFile(os.Getenv("GOSYNC_DEPLOY_LOG"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(90)
	}
	_ = json.NewEncoder(log).Encode(args)
	_ = log.Close()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "StrictHostKeyChecking=yes") || !strings.Contains(joined, "BatchMode=yes") {
		os.Exit(91)
	}
	if args[0] == "scp" {
		os.Exit(0)
	}
	command := args[len(args)-1]
	mode := os.Getenv("GOSYNC_DEPLOY_MODE")
	switch {
	case strings.Contains(command, "mktemp -d"):
		if mode == "create-failure" {
			os.Exit(1)
		}
		if mode == "invalid-directory" {
			_, _ = os.Stdout.WriteString("/tmp/not-a-deployment-export\n")
			os.Exit(0)
		}
	case strings.HasPrefix(command, "mkdir -p"):
		if mode == "base-failure" {
			os.Exit(1)
		}
		os.Exit(0)
	case strings.HasPrefix(command, "nohup"):
		if mode == "start-failure" {
			os.Exit(1)
		}
		match := regexp.MustCompile(`--cert-out '([^']+)'`).FindStringSubmatch(command)
		if len(match) != 0 {
			info, err := os.Stat(filepath.Dir(match[1]))
			if err != nil || info.Mode().Perm() != 0700 {
				os.Exit(92)
			}
			data, err := os.ReadFile(os.Getenv("GOSYNC_DEPLOY_PEM"))
			if err != nil || os.WriteFile(match[1], data, 0600) != nil {
				os.Exit(93)
			}
		}
		os.Exit(0)
	case strings.HasPrefix(command, "i=0;"):
		if mode == "fetch-failure" || mode == "empty" {
			if mode == "empty" {
				os.Exit(0)
			}
			os.Exit(1)
		}
	case strings.HasPrefix(command, "rm -f --"):
		if mode == "cleanup-failure" {
			os.Exit(1)
		}
	default:
		os.Exit(0)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
	if mode == "create-reported-failure" && strings.Contains(command, "mktemp -d") {
		os.Exit(1)
	}
	os.Exit(0)
}

func fakeDeploymentSSH(t *testing.T, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"ssh", "scp"} {
		script := "#!/bin/sh\nexec " + shellQuote(exe) + " -test.run=^TestDeploymentSSHHelper$ -- " + tool + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "commands.jsonl")
	if err := os.WriteFile(log, nil, 0600); err != nil {
		t.Fatal(err)
	}
	pemPath := filepath.Join(dir, "public.pem")
	if err := os.WriteFile(pemPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOSYNC_DEPLOY_HELPER", "1")
	t.Setenv("GOSYNC_DEPLOY_LOG", log)
	t.Setenv("GOSYNC_DEPLOY_PEM", pemPath)
	return log
}

func deploymentCommands(t *testing.T, log string) string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var args []string
		if err := json.Unmarshal(line, &args); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, strings.Join(args, " "))
	}
	return strings.Join(commands, "\n")
}

func TestQUICDeploymentBootstrap(t *testing.T) {
	public := testCertificate(t)
	for _, mode := range []string{"success", "create-failure", "create-reported-failure", "invalid-directory", "base-failure", "start-failure", "fetch-failure", "empty", "malformed", "private-key", "oversized", "cleanup-failure"} {
		t.Run(mode, func(t *testing.T) {
			data := public
			switch mode {
			case "malformed":
				data = []byte("not a certificate")
			case "private-key":
				data = append(bytes.Clone(public), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("secret")})...)
			case "oversized":
				data = append(bytes.Clone(public), bytes.Repeat([]byte(" "), maxCertificateSize)...)
			}
			log := fakeDeploymentSSH(t, data)
			t.Setenv("GOSYNC_DEPLOY_MODE", mode)
			d := NewDeployer("", "user@host:/base's directory", "/local key's path")
			if err := d.SetTransport("quic"); err != nil {
				t.Fatal(err)
			}
			d.SetServerPort(54321)
			userTLS := filepath.Join(t.TempDir(), "user's TLS.pem")
			if err := os.WriteFile(userTLS, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := d.SetRemoteTLSFiles(userTLS, userTLS); err != nil {
				t.Fatal(err)
			}
			d.certificatePEM = []byte("old pin")
			err := d.Deploy()
			if (err == nil) != (mode == "success") {
				t.Fatalf("Deploy error = %v for %s", err, mode)
			}
			if mode == "success" {
				if !bytes.Equal(d.CertificatePEM(), public) {
					t.Fatal("public certificate not returned")
				}
				copy := d.CertificatePEM()
				copy[0] ^= 1
				if !bytes.Equal(d.CertificatePEM(), public) {
					t.Fatal("CertificatePEM exposes mutable state")
				}
			} else if d.CertificatePEM() != nil {
				t.Fatal("failed deploy exposed a certificate")
			}
			commands := deploymentCommands(t, log)
			if mode != "create-failure" && mode != "invalid-directory" && !strings.Contains(commands, "rm -f -- '/tmp/gosync-cert.") {
				t.Fatal("missing scoped cleanup")
			}
			if mode == "invalid-directory" && strings.Contains(commands, "rm -f --") {
				t.Fatal("untrusted directory output used for cleanup")
			}
			if mode == "cleanup-failure" {
				if d.certExportDir == "" {
					t.Fatal("lost failed cleanup target")
				}
				t.Setenv("GOSYNC_DEPLOY_MODE", "success")
				if err := d.Cleanup(); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range regexp.MustCompile(`/tmp/gosync-cert\.[A-Za-z0-9]{10}`).FindAllString(commands, -1) {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("export directory remains: %s (%v)", path, err)
					_ = os.Remove(filepath.Join(path, "certificate.pem"))
					_ = os.Remove(path)
				}
			}
			if data, err := os.ReadFile(userTLS); err != nil || string(data) != "untouched" {
				t.Fatal("user TLS file changed")
			}
		})
	}
}

func TestRepeatedQUICDeploymentUsesUniqueExports(t *testing.T) {
	public := testCertificate(t)
	log := fakeDeploymentSSH(t, public)
	t.Setenv("GOSYNC_DEPLOY_MODE", "success")
	d := NewDeployer("", "user@host", "")
	if err := d.SetTransport("quic"); err != nil {
		t.Fatal(err)
	}
	d.SetServerPort(54321)
	for i := 0; i < 2; i++ {
		if err := d.Deploy(); err != nil {
			t.Fatal(err)
		}
		if d.certExportDir != "" || !bytes.Equal(d.CertificatePEM(), public) {
			t.Fatal("deployment did not clean its export and retain its pin")
		}
	}
	commands := deploymentCommands(t, log)
	matches := regexp.MustCompile(`--cert-out '([^']+)'`).FindAllStringSubmatch(commands, -1)
	if len(matches) != 2 || matches[0][1] == matches[1][1] {
		t.Fatalf("exports were not unique: %v", matches)
	}
	if strings.Contains(commands, "--cert ") || strings.Contains(commands, "--key ") {
		t.Fatal("generated-certificate deployment supplied TLS file flags")
	}
	// A later failure must not expose the pin from an earlier successful run.
	t.Setenv("GOSYNC_DEPLOY_MODE", "fetch-failure")
	if err := d.Deploy(); err == nil || d.CertificatePEM() != nil {
		t.Fatal("failed redeployment retained old certificate")
	}
}

func TestTCPDeploymentDoesNotFetchCertificate(t *testing.T) {
	log := fakeDeploymentSSH(t, nil)
	d := NewDeployer("", "user@host:2222", "key with spaces")
	d.SetServerPort(54321)
	d.certificatePEM = []byte("old pin")
	if err := d.Deploy(); err != nil {
		t.Fatal(err)
	}
	commands := deploymentCommands(t, log)
	if strings.Contains(commands, "mktemp") || strings.Contains(commands, "certificate.pem") || d.CertificatePEM() != nil {
		t.Fatal("TCP performed certificate bootstrap or retained old pin")
	}
	if !strings.Contains(commands, "-p 2222") || !strings.Contains(commands, "-P 2222") {
		t.Fatal("SSH/SCP custom port lost")
	}
}

func TestBoundedCommand(t *testing.T) {
	if _, err := boundedOutput("sh", []string{"-c", "printf 12345"}, 4); !errors.Is(err, errOutputLimit) {
		t.Fatalf("oversized output error = %v", err)
	}
	if _, err := boundedOutput("sh", []string{"-c", "head -c 8192 /dev/zero >&2"}, 4); !errors.Is(err, errOutputLimit) {
		t.Fatalf("oversized stderr error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := runCommandContext(ctx, "sh", []string{"-c", "exec sleep 10"}, nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("command timeout was not bounded")
	}
}
