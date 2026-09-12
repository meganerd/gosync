package deploy

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gbjohnso/gosync/pkg/checksum"
	"github.com/gbjohnso/gosync/pkg/ranged"
)

const (
	portMin      = 49152
	portMax      = 65535
	portMaxTries = 20
)

type Deployer struct {
	source         string
	destination    string
	keyFile        string
	username       string
	host           string
	port           int
	serverPort     int
	serverPortSet  bool
	baseDir        string
	bufferSize     int
	connections    int
	transportType  string
	remoteCert     string
	remoteKey      string
	certExportDir  string
	certificatePEM []byte
}

func NewDeployer(source, destination, keyFile string) *Deployer {
	return &Deployer{
		source:        source,
		destination:   destination,
		keyFile:       keyFile,
		serverPort:    0,
		bufferSize:    checksum.DefaultBufferSize,
		connections:   1,
		transportType: "tcp",
	}
}

// SetTransport selects the receiver protocol; deployment itself still uses SSH.
func (d *Deployer) SetTransport(protocol string) error {
	switch protocol {
	case "quic", "tcp":
		d.transportType = protocol
	case "server":
		d.transportType = "tcp"
	default:
		return fmt.Errorf("deployment supports quic, tcp, or server, not %q", protocol)
	}
	return nil
}

// SetBufferSize configures the receiver before deployment.
func (d *Deployer) SetBufferSize(size int) error {
	if err := checksum.ValidateBufferSize(size); err != nil {
		return err
	}
	d.bufferSize = size
	return nil
}

// SetConnections tells the receiver how many sockets to bind to its single
// port, matching the sender's QUIC data-connection fan-out.
func (d *Deployer) SetConnections(connections int) error {
	if connections < 1 || connections > ranged.MaxConnections {
		return fmt.Errorf("connections must be between 1 and %d", ranged.MaxConnections)
	}
	d.connections = connections
	return nil
}

func (d *Deployer) SetServerPort(port int) {
	d.serverPort = port
	d.serverPortSet = port > 0
}

func (d *Deployer) GetServerPort() int {
	return d.serverPort
}

func (d *Deployer) Deploy() (err error) {
	d.certificatePEM = nil
	if err := d.cleanupCertificateExport(); err != nil {
		return fmt.Errorf("cleanup previous certificate export failed: %w", err)
	}
	if err := d.parseDestination(); err != nil {
		return fmt.Errorf("parse destination failed: %w", err)
	}

	if !d.serverPortSet {
		port, err := d.findFreePort()
		if err != nil {
			return fmt.Errorf("find free port failed: %w", err)
		}
		d.serverPort = port
	}

	if err := d.cleanupStale(); err != nil {
		return fmt.Errorf("cleanup stale process failed: %w", err)
	}

	if err := d.copyBinary(); err != nil {
		return fmt.Errorf("copy binary failed: %w", err)
	}

	if d.transportType == "quic" {
		defer func() {
			if cleanupErr := d.cleanupCertificateExport(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup certificate export failed: %w", cleanupErr))
			}
			if err != nil {
				d.certificatePEM = nil
			}
		}()
		if err := d.createCertificateExport(); err != nil {
			return fmt.Errorf("create certificate export failed: %w", err)
		}
	}

	if err := d.startRemoteServer(); err != nil {
		return fmt.Errorf("start remote server failed: %w", err)
	}
	if d.transportType == "quic" {
		if err := d.fetchCertificate(); err != nil {
			return fmt.Errorf("fetch remote certificate failed: %w", err)
		}
	}

	return nil
}

func (d *Deployer) findFreePort() (int, error) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < portMaxTries; i++ {
		port := portMin + rng.Intn(portMax-portMin+1)
		free, err := d.checkRemotePort(port)
		if err != nil {
			return 0, fmt.Errorf("check remote port failed: %w", err)
		}
		if free {
			fmt.Printf("Selected available port %d on %s\n", port, d.host)
			return port, nil
		}
	}

	return 0, fmt.Errorf("no free port found after %d attempts", portMaxTries)
}

func (d *Deployer) checkRemotePort(port int) (bool, error) {
	args := append(d.sshBaseArgs(),
		fmt.Sprintf("%s@%s", d.username, d.host),
		d.portCheckCommand(port))

	output, err := boundedOutput("ssh", args, 4096)
	if err != nil {
		return false, fmt.Errorf("check remote %s port: %w", d.transportType, err)
	}
	return strings.TrimSpace(string(output)) == "", nil
}

func (d *Deployer) portCheckCommand(port int) string {
	options := "-tlnH"
	if d.transportType == "quic" {
		options = "-ulnH"
	}
	return fmt.Sprintf("ss %s 'sport = :%d'", options, port)
}

// remoteCleanupScript stops only a gosync server this deployment started and
// removes its marker, rather than killing every gosync process on the host.
// The marker holds "<pid> <starttime>" (field 22 of /proc/<pid>/stat, in clock
// ticks) written by remoteServerCommand. Cleanup runs only when the recorded
// pid is numeric, greater than 1, still alive, and its starttime still matches,
// so a recycled pid or another user's process is never touched.
const remoteCleanupScript = `pidfile="$HOME/.gosync.pid"
[ -r "$pidfile" ] || exit 0
read pid start < "$pidfile" || exit 0
case "$pid" in
''|*[!0-9]*) exit 0 ;;
esac
[ "$pid" -gt 1 ] 2>/dev/null || exit 0
if [ -z "$start" ]; then rm -f "$pidfile"; exit 0; fi
if [ ! -r "/proc/$pid/stat" ]; then rm -f "$pidfile"; exit 0; fi
now=$(awk '{print $22}' "/proc/$pid/stat" 2>/dev/null)
if [ "$now" != "$start" ]; then exit 0; fi
kill "$pid" 2>/dev/null
i=0
while [ -d "/proc/$pid" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i+1)); done
if [ -d "/proc/$pid" ]; then kill -9 "$pid" 2>/dev/null; fi
rm -f "$pidfile" "$HOME/gosync"`

func (d *Deployer) Cleanup() error {
	exportErr := d.cleanupCertificateExport()
	args := append(d.sshBaseArgs(),
		fmt.Sprintf("%s@%s", d.username, d.host), remoteCleanupScript)

	fmt.Printf("Cleaning up gosync on %s@%s\n", d.username, d.host)

	return errors.Join(exportErr, runCommand("ssh", args, os.Stdout, os.Stderr))
}

func (d *Deployer) sshBaseArgs() []string {
	args := []string{
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
	}
	if d.port != 22 {
		args = append(args, "-p", fmt.Sprintf("%d", d.port))
	}
	if d.keyFile != "" {
		args = append(args, "-i", d.keyFile)
	}
	return args
}

func (d *Deployer) startRemoteServer() error {
	if d.baseDir != "~" {
		args := append(d.sshBaseArgs(),
			fmt.Sprintf("%s@%s", d.username, d.host),
			fmt.Sprintf("mkdir -p -- %s", quoteRemotePath(d.baseDir)))
		fmt.Printf("Creating remote directory %s\n", d.baseDir)
		if err := runCommand("ssh", args, os.Stdout, os.Stderr); err != nil {
			return fmt.Errorf("create remote base dir failed: %w", err)
		}
	}

	remoteCmd := d.remoteServerCommand()

	args := append(d.sshBaseArgs(),
		fmt.Sprintf("%s@%s", d.username, d.host), remoteCmd)

	fmt.Printf("Starting remote %s server on %s@%s (base: %s, port: %d)\n",
		d.transportType, d.username, d.host, d.baseDir, d.serverPort)

	return runCommand("ssh", args, os.Stdout, os.Stderr)
}

func (d *Deployer) remoteServerCommand() string {
	tlsArgs := ""
	if d.transportType == "quic" {
		// Fan-out is QUIC-only, so the receiver sizes its SO_REUSEPORT socket
		// group only for QUIC; TCP deployment keeps its existing command.
		connections := d.connections
		if connections < 1 {
			connections = 1
		}
		tlsArgs += fmt.Sprintf(" --connections %d", connections)
		if d.certExportDir != "" {
			tlsArgs += " --cert-out " + quoteRemotePath(d.certExportDir+"/certificate.pem")
		}
		if d.remoteCert != "" {
			tlsArgs += " --cert " + quoteRemotePath(d.remoteCert) + " --key " + quoteRemotePath(d.remoteKey)
		}
	}
	return fmt.Sprintf(
		"nohup ~/gosync serve --listen 0.0.0.0:%d --base %s --buffer-size %d --transport %s%s > /tmp/gosync-server.log 2>&1 </dev/null & sleep 2; pid=$!; start=$(awk '{print $22}' /proc/$pid/stat 2>/dev/null); echo \"$pid $start\" > \"$HOME/.gosync.pid\"",
		d.serverPort, quoteRemotePath(d.baseDir), d.bufferSize, d.transportType, tlsArgs,
	)
}

func (d *Deployer) parseDestination() error {
	parts := strings.Split(d.destination, "@")
	if len(parts) == 2 {
		d.username = parts[0]
		d.host = parts[1]
	} else {
		d.host = parts[0]
		currentUser, err := user.Current()
		if err != nil {
			d.username = "root"
		} else {
			d.username = currentUser.Username
		}
	}

	d.port = 22
	d.baseDir = "~"

	hostParts := strings.SplitN(d.host, ":", 2)
	d.host = hostParts[0]

	if len(hostParts) == 2 {
		rest := hostParts[1]
		if rest == "" {
			d.baseDir = "~"
		} else if port, err := strconv.Atoi(rest); err == nil && port > 0 && port < 65536 {
			d.port = port
		} else {
			d.baseDir = rest
		}
	}

	d.baseDir = strings.TrimRight(d.baseDir, "/\\")

	return nil
}

func (d *Deployer) cleanupStale() error {
	args := append(d.sshBaseArgs(),
		fmt.Sprintf("%s@%s", d.username, d.host), remoteCleanupScript)

	fmt.Printf("Cleaning up any stale gosync on %s\n", d.host)

	return runCommand("ssh", args, os.Stdout, os.Stderr)
}

func (d *Deployer) copyBinary() error {
	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path failed: %w", err)
	}

	binaryPath, err = filepath.EvalSymlinks(binaryPath)
	if err != nil {
		return fmt.Errorf("eval symlinks failed: %w", err)
	}

	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		return fmt.Errorf("binary not found: %s", binaryPath)
	}

	scpArgs := []string{
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
	}

	if d.port != 22 {
		scpArgs = append(scpArgs, "-P", fmt.Sprintf("%d", d.port))
	}

	if d.keyFile != "" {
		scpArgs = append(scpArgs, "-i", d.keyFile)
	}

	scpArgs = append(scpArgs, binaryPath, fmt.Sprintf("%s@%s:gosync", d.username, d.host))

	fmt.Printf("Copying binary to %s@%s:~/gosync\n", d.username, d.host)

	return runCommand("scp", scpArgs, os.Stdout, os.Stderr)
}

func (d *Deployer) GetHost() string {
	return d.host
}

func (d *Deployer) GetPort() int {
	if d.port == 0 {
		return 22
	}
	return d.port
}

func (d *Deployer) GetUsername() string {
	if d.username == "" {
		currentUser, err := user.Current()
		if err != nil {
			return "root"
		}
		return currentUser.Username
	}
	return d.username
}

func (d *Deployer) GetBaseDir() string {
	return d.baseDir
}
