package server

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Server struct {
	listenAddr string
	baseDir    string
	listener   net.Listener
	clients    map[string]net.Conn
	mu         sync.RWMutex
}

func NewServer(listenAddr, baseDir string) *Server {
	return &Server{
		listenAddr: listenAddr,
		baseDir:    baseDir,
		clients:    make(map[string]net.Conn),
	}
}

func (s *Server) Start() error {
	var err error
	s.listener, err = net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}

	fmt.Printf("gosync server listening on %s\n", s.listenAddr)
	fmt.Printf("Base directory: %s\n", s.baseDir)

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			fmt.Printf("accept error: %v\n", err)
			continue
		}

		go s.handleClient(conn)
	}
}

func (s *Server) handleClient(conn net.Conn) {
	defer conn.Close()

	remoteAddr := conn.RemoteAddr().String()
	s.mu.Lock()
	s.clients[remoteAddr] = conn
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.clients, remoteAddr)
		s.mu.Unlock()
	}()

	fmt.Printf("Client connected: %s\n", remoteAddr)

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 0 {
			continue
		}

		cmd := parts[0]
		switch cmd {
		case "SEND":
			if len(parts) < 3 {
				s.sendError(conn, "SEND requires path and size")
				continue
			}
			path := parts[1]
			var size int64
			fmt.Sscanf(parts[2], "%d", &size)
			s.handleSend(reader, conn, path, size)

		case "RECEIVE":
			if len(parts) < 2 {
				s.sendError(conn, "RECEIVE requires path")
				continue
			}
			path := parts[1]
			s.handleReceive(conn, path)

		case "LIST":
			s.handleList(conn)

		case "PING":
			s.sendResponse(conn, "PONG")

		case "QUIT":
			fmt.Printf("Client disconnected: %s\n", remoteAddr)
			return

		default:
			s.sendError(conn, fmt.Sprintf("unknown command: %s", cmd))
		}
	}
}

func (s *Server) handleSend(reader io.Reader, conn net.Conn, remotePath string, size int64) {
	fullPath := filepath.Join(s.baseDir, remotePath)

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		s.sendError(conn, fmt.Sprintf("create dir failed: %v", err))
		return
	}

	file, err := os.Create(fullPath)
	if err != nil {
		s.sendError(conn, fmt.Sprintf("create file failed: %v", err))
		return
	}
	defer file.Close()

	hasher := sha256.New()
	writer := io.MultiWriter(file, hasher)

	remaining := size
	buf := make([]byte, 32*1024)
	for remaining > 0 {
		toRead := int64(len(buf))
		if toRead > remaining {
			toRead = remaining
		}
		n, err := io.ReadAtLeast(reader, buf[:toRead], int(toRead))
		if n > 0 {
			_, writeErr := writer.Write(buf[:n])
			if writeErr != nil {
				s.sendError(conn, fmt.Sprintf("write failed: %v", writeErr))
				return
			}
			remaining -= int64(n)
		}
		if err != nil {
			if err != io.EOF {
				s.sendError(conn, fmt.Sprintf("read failed: %v", err))
			}
			break
		}
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	s.sendResponse(conn, fmt.Sprintf("%s %d", checksum, size))
}

func (s *Server) handleReceive(conn net.Conn, remotePath string) {
	fullPath := filepath.Join(s.baseDir, remotePath)

	file, err := os.Open(fullPath)
	if err != nil {
		s.sendError(conn, fmt.Sprintf("open file failed: %v", err))
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		s.sendError(conn, fmt.Sprintf("stat file failed: %v", err))
		return
	}

	s.sendResponse(conn, fmt.Sprintf("SIZE %d", stat.Size()))

	hasher := sha256.New()
	writer := io.MultiWriter(conn, hasher)

	if _, err := io.Copy(writer, file); err != nil {
		s.sendError(conn, fmt.Sprintf("send failed: %v", err))
		return
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	fmt.Fprintf(conn, "CHECKSUM %s\n", checksum)
}

func (s *Server) handleList(conn net.Conn) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		s.sendError(conn, fmt.Sprintf("list failed: %v", err))
		return
	}

	var lines []string
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %d %d", entry.Name(), info.Size(), info.ModTime().Unix()))
	}

	s.sendResponse(conn, fmt.Sprintf("LIST %d\n%s", len(lines), strings.Join(lines, "\n")))
}

func (s *Server) sendResponse(conn net.Conn, msg string) {
	fmt.Fprintf(conn, "OK %s\n", msg)
}

func (s *Server) sendError(conn net.Conn, msg string) {
	fmt.Fprintf(conn, "ERROR %s\n", msg)
}

func (s *Server) Stop() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *Server) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}