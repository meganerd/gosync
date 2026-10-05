package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/gbjohnso/gosync/pkg/transport"
)

const (
	deploymentCommandTimeout = 30 * time.Second
	maxCertificateSize       = 1024 * 1024
)

// SetRemoteTLSFiles selects existing remote TLS files. Both must be provided,
// or both empty to let the receiver generate a certificate. They are never fetched
// or removed by the deployer. Configure before Deploy.
func (d *Deployer) SetRemoteTLSFiles(remoteCert, remoteKey string) error {
	if (remoteCert == "") != (remoteKey == "") {
		return fmt.Errorf("remote TLS certificate and key must be provided together")
	}
	if strings.ContainsRune(remoteCert, 0) || strings.ContainsRune(remoteKey, 0) {
		return fmt.Errorf("remote TLS paths must not contain NUL")
	}
	d.remoteCert, d.remoteKey = remoteCert, remoteKey
	return nil
}

// CertificatePEM returns a defensive copy of the public certificate obtained by
// the last successful QUIC Deploy, or nil before deployment or after failure.
func (d *Deployer) CertificatePEM() []byte {
	return bytes.Clone(d.certificatePEM)
}

// Quote the shell argument, but expand only a leading ~/ (or ~) on the remote
// host. In particular, shell metacharacters in the remaining path stay literal.
func quoteRemotePath(path string) string {
	if path == "~" {
		return "\"$HOME\""
	}
	if strings.HasPrefix(path, "~/") {
		return "\"$HOME\"/" + shellQuote(path[2:])
	}
	return shellQuote(path)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

var certificateExportPath = regexp.MustCompile(`^/tmp/gosync-cert\.[A-Za-z0-9]{10}$`)

func (d *Deployer) createCertificateExport() error {
	output, err := boundedOutput("ssh", d.remoteArgs("umask 077; mktemp -d /tmp/gosync-cert.XXXXXXXXXX"), 4096)
	path := strings.TrimSpace(string(output))
	// Retain a valid cleanup target even if SSH fails after reporting creation.
	// Never turn arbitrary SSH output into a cleanup target.
	if certificateExportPath.MatchString(path) {
		d.certExportDir = path
		return err
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("invalid certificate export directory returned by SSH")
}

func (d *Deployer) cleanupCertificateExport() error {
	if d.certExportDir == "" {
		return nil
	}
	path := d.certExportDir
	// Remove only our public export, then the empty directory. No recursive
	// removal or wildcard can touch user TLS files or unrelated artifacts.
	command := "rm -f -- " + quoteRemotePath(path+"/certificate.pem") + " && rmdir -- " + quoteRemotePath(path)
	if _, err := boundedOutput("ssh", d.remoteArgs(command), 4096); err != nil {
		return err
	}
	d.certExportDir = ""
	return nil
}

func (d *Deployer) fetchCertificate() error {
	path := quoteRemotePath(d.certExportDir + "/certificate.pem")
	// The server publishes its certificate at readiness. Poll for at most ten
	// seconds, and read one extra byte to detect oversized output, not truncate
	// it into an apparently valid certificate. The local bound is independent.
	command := fmt.Sprintf("i=0; while [ ! -s %s ]; do i=$((i+1)); [ \"$i\" -lt 10 ] || exit 1; sleep 1; done; head -c %d -- %s", path, maxCertificateSize+1, path)
	data, err := boundedOutput("ssh", d.remoteArgs(command), maxCertificateSize)
	if err != nil {
		return err
	}
	// The transport validator permits empty input to select system PKI; a
	// deployment bootstrap must instead fail closed without an explicit pin.
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("remote certificate export is empty")
	}
	if err := transport.ValidateCertificatePEM(data); err != nil {
		return fmt.Errorf("invalid remote public certificate: %w", err)
	}
	d.certificatePEM = bytes.Clone(data)
	return nil
}

func (d *Deployer) remoteArgs(command string) []string {
	return append(d.sshBaseArgs(), fmt.Sprintf("%s@%s", d.username, d.host), command)
}

func runCommand(name string, args []string, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), deploymentCommandTimeout)
	defer cancel()
	return runCommandContext(ctx, name, args, stdout, stderr)
}

func runCommandContext(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Bound pipe draining even if an SSH descendant keeps a pipe open.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

var errOutputLimit = errors.New("remote command output exceeds size limit")

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		return 0, errOutputLimit
	}
	return b.buffer.Write(p)
}

func boundedOutput(name string, args []string, limit int) ([]byte, error) {
	output := &boundedBuffer{limit: limit}
	stderr := &boundedBuffer{limit: 4096}
	if err := runCommand(name, args, output, stderr); err != nil {
		detail := strings.TrimSpace(stderr.buffer.String())
		if detail != "" {
			return output.buffer.Bytes(), fmt.Errorf("%s: %w: %s", name, err, detail)
		}
		return output.buffer.Bytes(), fmt.Errorf("%s: %w", name, err)
	}
	return output.buffer.Bytes(), nil
}
