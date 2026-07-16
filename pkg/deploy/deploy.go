package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

type Deployer struct {
	source      string
	destination string
	keyFile     string
	username    string
	host        string
	port        int
	remoteDir   string
}

func NewDeployer(source, destination, keyFile string) *Deployer {
	return &Deployer{
		source:      source,
		destination: destination,
		keyFile:     keyFile,
	}
}

func (d *Deployer) Deploy() error {
	if err := d.parseDestination(); err != nil {
		return fmt.Errorf("parse destination failed: %w", err)
	}

	if err := d.ensureRemoteDir(); err != nil {
		return fmt.Errorf("create remote directory failed: %w", err)
	}

	if err := d.copyBinary(); err != nil {
		return fmt.Errorf("copy binary failed: %w", err)
	}

	if err := d.startRemoteServer(); err != nil {
		return fmt.Errorf("start remote server failed: %w", err)
	}

	return nil
}

func (d *Deployer) startRemoteServer() error {
	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=10",
	}

	if d.port != 22 {
		sshArgs = append(sshArgs, "-p", fmt.Sprintf("%d", d.port))
	}

	if d.keyFile != "" {
		sshArgs = append(sshArgs, "-i", d.keyFile)
	}

	remoteCmd := fmt.Sprintf("cd %s && nohup ./gosync serve --listen 0.0.0.0:8443 --base %s > /tmp/gosync-server.log 2>&1 &", d.remoteDir, d.remoteDir)
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", d.username, d.host), remoteCmd)

	fmt.Printf("Starting remote server on %s@%s\n", d.username, d.host)

	cmd := exec.Command("ssh", sshArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func (d *Deployer) ensureRemoteDir() error {
	sshArgs := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=10",
	}

	if d.port != 22 {
		sshArgs = append(sshArgs, "-p", fmt.Sprintf("%d", d.port))
	}

	if d.keyFile != "" {
		sshArgs = append(sshArgs, "-i", d.keyFile)
	}

	remoteCmd := fmt.Sprintf("test -f %s && rm -f %s; mkdir -p %s", d.remoteDir, d.remoteDir, d.remoteDir)
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", d.username, d.host), remoteCmd)

	fmt.Printf("Creating directory %s@%s:%s\n", d.username, d.host, d.remoteDir)

	cmd := exec.Command("ssh", sshArgs...)
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

	hostParts := strings.SplitN(d.host, ":", 2)
	if len(hostParts) == 2 && hostParts[1] != "" {
		d.host = hostParts[0]
		fmt.Sscanf(hostParts[1], "%d", &d.port)
	} else {
		d.host = hostParts[0]
		d.port = 22
	}

	if d.remoteDir == "" {
		d.remoteDir = fmt.Sprintf("~/%s", filepath.Base(d.source))
	}

	d.remoteDir = strings.TrimRight(d.remoteDir, "/\\")

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

	scpArgs = append(scpArgs, binaryPath, fmt.Sprintf("%s@%s:%s", d.username, d.host, d.remoteDir))

	fmt.Printf("Copying binary to %s@%s:%s\n", d.username, d.host, d.remoteDir)

	cmd := exec.Command("scp", scpArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func (d *Deployer) buildSSHArgs() []string {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "ConnectTimeout=10",
		"-p", fmt.Sprintf("%d", d.port),
	}

	if d.keyFile != "" {
		args = append(args, "-i", d.keyFile)
	}

	return args
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
