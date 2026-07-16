package deploy

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	portMin       = 49152
	portMax       = 65535
	portMaxTries  = 20
)

type Deployer struct {
	source        string
	destination   string
	keyFile       string
	username      string
	host          string
	port          int
	serverPort    int
	serverPortSet bool
	baseDir       string
}

func NewDeployer(source, destination, keyFile string) *Deployer {
	return &Deployer{
		source:      source,
		destination: destination,
		keyFile:     keyFile,
		serverPort:  0,
	}
}

func (d *Deployer) SetServerPort(port int) {
	d.serverPort = port
	d.serverPortSet = port > 0
}

func (d *Deployer) GetServerPort() int {
	return d.serverPort
}

func (d *Deployer) Deploy() error {
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

	if err := d.copyBinary(); err != nil {
		return fmt.Errorf("copy binary failed: %w", err)
	}

	if err := d.startRemoteServer(); err != nil {
		return fmt.Errorf("start remote server failed: %w", err)
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
		fmt.Sprintf("ss -tlnH 'sport = :%d' | grep -q .", port))

	cmd := exec.Command("ssh", args...)
	return cmd.Run() != nil, nil
}

func (d *Deployer) Cleanup() error {
	remoteCmd := "pkill -x gosync 2>/dev/null; rm -f ~/gosync"
	args := append(d.sshBaseArgs(),
		fmt.Sprintf("%s@%s", d.username, d.host), remoteCmd)

	fmt.Printf("Cleaning up gosync on %s@%s\n", d.username, d.host)

	cmd := exec.Command("ssh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func (d *Deployer) sshBaseArgs() []string {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
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
			fmt.Sprintf("mkdir -p %s", d.baseDir))
		fmt.Printf("Creating remote directory %s\n", d.baseDir)
		if err := exec.Command("ssh", args...).Run(); err != nil {
			return fmt.Errorf("create remote base dir failed: %w", err)
		}
	}

	remoteCmd := fmt.Sprintf(
		"nohup ~/gosync serve --listen 0.0.0.0:%d --base %s > /tmp/gosync-server.log 2>&1 </dev/null & sleep 2",
		d.serverPort, d.baseDir,
	)

	args := append(d.sshBaseArgs(),
		fmt.Sprintf("%s@%s", d.username, d.host), remoteCmd)

	fmt.Printf("Starting remote server on %s@%s (base: %s, port: %d)\n",
		d.username, d.host, d.baseDir, d.serverPort)

	cmd := exec.Command("ssh", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
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
		"-o", "StrictHostKeyChecking=no",
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

	cmd := exec.Command("scp", scpArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
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