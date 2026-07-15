package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Deployer struct {
	source      string
	destination string
	keyFile     string
	username    string
	host        string
	port        int
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

	if err := d.copyBinary(); err != nil {
		return fmt.Errorf("copy binary failed: %w", err)
	}

	if err := d.startRemote(); err != nil {
		return fmt.Errorf("start remote failed: %w", err)
	}

	return nil
}

func (d *Deployer) parseDestination() error {
	parts := strings.Split(d.destination, "@")
	if len(parts) == 2 {
		d.username = parts[0]
		d.host = parts[1]
	} else {
		d.host = parts[0]
		d.username = "root"
	}

	hostParts := strings.Split(d.host, ":")
	if len(hostParts) == 2 {
		d.host = hostParts[0]
		fmt.Sscanf(hostParts[1], "%d", &d.port)
	} else {
		d.port = 22
	}

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

	remotePath := fmt.Sprintf("/usr/local/bin/gosync")
	sshArgs := d.buildSSHArgs()
	scpArgs := append(sshArgs, "-r", binaryPath, fmt.Sprintf("%s@%s:%s", d.username, d.host, remotePath))

	fmt.Printf("Copying binary to %s@%s:%s\n", d.username, d.host, remotePath)

	cmd := exec.Command("scp", scpArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func (d *Deployer) startRemote() error {
	sshArgs := d.buildSSHArgs()
	remoteCmd := "gosync serve --listen 0.0.0.0:8443 --base /tmp/gosync-receive &"
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", d.username, d.host), remoteCmd)

	fmt.Printf("Starting remote server on %s@%s\n", d.username, d.host)

	cmd := exec.Command("ssh", sshArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start remote server failed: %w", err)
	}

	time.Sleep(time.Second)
	fmt.Printf("Remote server started on %s:8443\n", d.host)
	return nil
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
		return "root"
	}
	return d.username
}
