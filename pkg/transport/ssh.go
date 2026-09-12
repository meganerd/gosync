package transport

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHTransport struct {
	progressCallback func(int64)
	config           Config
	client           *ssh.Client
	session          *ssh.Session
	connected        bool
}

func NewSSHTransport(config Config) *SSHTransport {
	return &SSHTransport{config: config}
}

// SetProgressCallback implements ProgressReporter.
func (s *SSHTransport) SetProgressCallback(callback func(int64)) {
	s.progressCallback = callback
}

func (s *SSHTransport) Name() string {
	return "ssh"
}

func (s *SSHTransport) Connect(host string, port int) error {
	addr := fmt.Sprintf("%s:%d", host, port)

	authMethods := []ssh.AuthMethod{}
	if s.config.KeyFile != "" {
		key, err := os.ReadFile(s.config.KeyFile)
		if err != nil {
			return fmt.Errorf("read key file failed: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return fmt.Errorf("parse key failed: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	sshConfig := &ssh.ClientConfig{
		User:            s.config.Username,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Duration(s.config.Timeout) * time.Second,
	}

	client, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return fmt.Errorf("ssh dial failed: %w", err)
	}

	s.client = client
	s.connected = true
	return nil
}

func (s *SSHTransport) SendFile(localPath, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file failed: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat file failed: %w", err)
	}

	return s.SendStream(file, remotePath, stat.Size())
}

func (s *SSHTransport) ReceiveFile(remotePath, localPath string) error {
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file failed: %w", err)
	}
	defer file.Close()

	return s.ReceiveStream(remotePath, file)
}

func (s *SSHTransport) SendStream(reader io.Reader, remotePath string, size int64) error {
	if !s.connected {
		return fmt.Errorf("not connected")
	}

	session, err := s.client.NewSession()
	if err != nil {
		return fmt.Errorf("create session failed: %w", err)
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("get stdin pipe failed: %w", err)
	}

	cmd := fmt.Sprintf("cat > %s", remotePath)
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("start command failed: %w", err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(progressWriter{stdin, s.progressCallback}, hasher)

	_, err = io.Copy(writer, reader)
	if err != nil {
		return fmt.Errorf("send data failed: %w", err)
	}

	stdin.Close()
	session.Wait()

	return nil
}

func (s *SSHTransport) ReceiveStream(remotePath string, writer io.Writer) error {
	if !s.connected {
		return fmt.Errorf("not connected")
	}

	session, err := s.client.NewSession()
	if err != nil {
		return fmt.Errorf("create session failed: %w", err)
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("get stdout pipe failed: %w", err)
	}

	cmd := fmt.Sprintf("cat %s", remotePath)
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("start command failed: %w", err)
	}

	hasher := sha256.New()
	multiWriter := io.MultiWriter(writer, hasher)

	_, err = io.Copy(multiWriter, stdout)
	if err != nil && err != io.EOF {
		return fmt.Errorf("receive data failed: %w", err)
	}

	session.Wait()
	return nil
}

func (s *SSHTransport) Close() error {
	if s.client != nil {
		s.connected = false
		return s.client.Close()
	}
	return nil
}

func (s *SSHTransport) IsConnected() bool {
	return s.connected
}

func (s *SSHTransport) buildRemotePath(localPath, baseDir string) string {
	rel, err := filepath.Rel(baseDir, localPath)
	if err != nil {
		return filepath.Base(localPath)
	}
	return filepath.Join(baseDir, rel)
}
