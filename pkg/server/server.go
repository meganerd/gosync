package server

import (
	"bufio"

	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gbjohnso/gosync/pkg/checksum"
	"github.com/gbjohnso/gosync/pkg/ranged"
	"github.com/quic-go/quic-go"
)

type Server struct {
	listenAddr     string
	baseDir        string
	bufferSize     int
	listener       net.Listener
	clients        map[net.Conn]struct{}
	protocol       string
	certFile       string
	keyFile        string
	certOutput     string
	certificatePEM []byte
	quicListener   *quic.Listener
	quicGroup      *quicGroup
	quicConns      map[*quic.Conn]struct{}
	maxConnections int
	tokens         map[string]*rangeTransfer
	tokenTimeout   time.Duration
	sweeperDone    chan struct{}
	started        bool
	stopped        bool
	stopOnce       sync.Once
	workers        sync.WaitGroup
	mu             sync.RWMutex
}

func NewServer(listenAddr, baseDir string) *Server {
	return &Server{
		listenAddr:     listenAddr,
		baseDir:        baseDir,
		bufferSize:     checksum.DefaultBufferSize,
		clients:        make(map[net.Conn]struct{}),
		protocol:       "tcp",
		quicConns:      make(map[*quic.Conn]struct{}),
		maxConnections: 1,
		tokens:         make(map[string]*rangeTransfer),
		tokenTimeout:   minTokenTimeout,
	}
}

// SetBufferSize sets the size in bytes of each of the four transfer copy buffers.
// The size must be between 4 KiB and 4 MiB inclusive; zero is not valid.
// Call it before Start; concurrent changes are not supported.
func (s *Server) SetBufferSize(size int) error {
	if err := checksum.ValidateBufferSize(size); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return fmt.Errorf("buffer size must be configured before Start")
	}
	s.bufferSize = size
	return nil
}

// SetMaxConnections sets how many UDP sockets the QUIC receiver binds to its
// single listen port with SO_REUSEPORT, and the limit it advertises in FANOUT.
// Valid values are 1 to ranged.MaxConnections; 1 is the default and reproduces
// the original single-listener behavior exactly. Call it before Start.
func (s *Server) SetMaxConnections(connections int) error {
	if connections < 1 || connections > ranged.MaxConnections {
		return fmt.Errorf("connections %d out of range 1-%d", connections, ranged.MaxConnections)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return fmt.Errorf("connections must be configured before Start")
	}
	s.maxConnections = connections
	return nil
}

// SetTokenTimeout sets how long a ranged transfer may sit idle before the
// receiver deletes its staged file and forgets its token. Values below
// minTokenTimeout are raised to it, because a shorter idle window risks
// discarding a live transfer between ranges. Call it before Start.
func (s *Server) SetTokenTimeout(timeout time.Duration) error {
	if timeout < minTokenTimeout {
		timeout = minTokenTimeout
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return fmt.Errorf("token timeout must be configured before Start")
	}
	s.tokenTimeout = timeout
	return nil
}

// SetTransport selects tcp (the default) or quic. Configure it before Start.
func (s *Server) SetTransport(protocol string) error {
	if protocol != "tcp" && protocol != "quic" {
		return fmt.Errorf("unsupported transport %q: expected tcp or quic", protocol)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return fmt.Errorf("transport must be configured before Start")
	}
	s.protocol = protocol
	return nil
}

// Start blocks serving clients until Stop or a listener error. Servers are single-use.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return fmt.Errorf("server already started or stopped")
	}
	s.started = true
	var err error
	if s.protocol == "quic" {
		err = s.startQUIC()
	} else if s.certFile != "" || s.keyFile != "" || s.certOutput != "" {
		err = fmt.Errorf("TLS configuration requires QUIC transport")
	} else {
		s.listener, err = net.Listen("tcp", s.listenAddr)
	}
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("listen failed: %w", err)
	}
	listener, quicListener := s.listener, s.quicListener
	var extra []*quic.Listener
	if s.quicGroup != nil {
		extra = append(extra, s.quicGroup.listeners[1:]...)
	}
	s.mu.Unlock()
	defer s.Stop()
	s.startTokenSweeper()
	if quicListener != nil {
		fmt.Printf("gosync server listening on %s (QUIC/UDP)\n", quicListener.Addr())
		fmt.Printf("Base directory: %s\n", s.baseDir)
		if s.certFile == "" {
			fmt.Println("QUIC uses a generated certificate; clients must trust its certificate pin")
		}
		if len(extra) > 0 {
			fmt.Printf("QUIC fan-out: %d sockets share %s via SO_REUSEPORT\n", len(extra)+1, quicListener.Addr())
		}
		s.acceptExtraQUIC(extra)
		return s.acceptQUIC(quicListener)
	}
	fmt.Printf("gosync server listening on %s (TCP)\n", listener.Addr())
	fmt.Printf("Base directory: %s\n", s.baseDir)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return s.acceptError(err)
		}
		s.serveClient(conn)
	}
}

func (s *Server) acceptError(err error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.stopped {
		return nil
	}
	return fmt.Errorf("accept failed: %w", err)
}

func (s *Server) serveClient(conn net.Conn) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		conn.Close()
		return
	}
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		s.handleClient(conn)
	}()
}

func (s *Server) handleClient(conn net.Conn) {
	defer conn.Close()

	remoteAddr := conn.RemoteAddr().String()
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.clients[conn] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
	}()

	fmt.Printf("Client connected: %s\n", remoteAddr)

	reader := bufio.NewReader(conn)
	_, singleOperation := conn.(*quicStreamConn)
	processed := false
	for {
		if singleOperation && processed {
			return
		}
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

		// QUIC uses one operation per bidirectional stream, including errors.
		processed = true
		cmd := parts[0]
		switch cmd {
		case "SEND", "SEND-NOHASH":
			// Format: SEND <size> <base64(path)>. The path is base64-encoded
			// because it may contain spaces, which would break a space-split.
			if len(parts) < 3 {
				s.sendError(conn, "SEND requires size and path")
				continue
			}
			size, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil || size < 0 {
				s.sendError(conn, "SEND requires non-negative numeric size")
				continue
			}
			path, err := decodePath(parts[2])
			if err != nil {
				s.sendError(conn, "SEND path not base64 encoded")
				continue
			}
			s.handleSendMode(reader, conn, path, size, cmd == "SEND")

		case "RECEIVE", "RECEIVE-NOHASH":
			if len(parts) < 2 || (cmd == "RECEIVE-NOHASH" && len(parts) != 2) {
				s.sendError(conn, "RECEIVE requires path")
				continue
			}
			path, err := decodePath(parts[1])
			if err != nil {
				s.sendError(conn, "RECEIVE path not base64 encoded")
				continue
			}
			s.handleReceiveMode(conn, path, cmd == "RECEIVE")

		case "CAPS":
			if len(parts) != 1 {
				s.sendError(conn, "CAPS takes no arguments")
				continue
			}
			s.sendResponse(conn, "CAPS NOHASH")

		case ranged.CmdFanout:
			// Fan-out is a separate command because clients compare the CAPS
			// reply byte for byte; see docs/multi-connection-quic-design.md.
			s.handleFanout(conn, parts)

		case ranged.CmdSendRange, ranged.CmdSendRangeHash:
			// Ranged commands carry five arguments, so they re-split the raw
			// line rather than using the three-way split above.
			s.handleSendRange(reader, conn, line, cmd == ranged.CmdSendRangeHash)

		case ranged.CmdCommit, ranged.CmdCommitHash:
			s.handleCommit(conn, line, cmd == ranged.CmdCommitHash)

		case ranged.CmdAbort:
			s.handleAbort(conn, line)

		case "LIST":
			s.handleList(conn)

		case "PING":
			pong := "PONG"
			if len(parts) == 2 && parts[1] == "BASE" {
				if baseDir, err := filepath.Abs(s.baseDir); err == nil {
					pong += " " + base64.StdEncoding.EncodeToString([]byte(baseDir))
				}
			}
			s.sendResponse(conn, pong)

		case "QUIT":
			fmt.Printf("Client disconnected: %s\n", remoteAddr)
			return

		default:
			s.sendError(conn, fmt.Sprintf("unknown command: %s", cmd))
		}
	}
}

func decodePath(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (s *Server) handleSend(reader io.Reader, conn net.Conn, remotePath string, size int64) {
	s.handleSendMode(reader, conn, remotePath, size, true)
}

func (s *Server) handleSendMode(reader io.Reader, conn net.Conn, remotePath string, size int64, withChecksum bool) {
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

	checksumText := "NONE"
	if withChecksum {
		_, digest, copyErr := checksum.CopyNWithSHA256Buffer(file, reader, size, s.bufferSize)
		err = copyErr
		checksumText = hex.EncodeToString(digest[:])
	} else {
		_, err = checksum.CopyNBuffer(file, reader, size, s.bufferSize)
	}
	if err != nil {
		s.sendError(conn, fmt.Sprintf("receive failed: %v", err))
		return
	}

	s.sendResponse(conn, fmt.Sprintf("%s %d", checksumText, size))
}

func (s *Server) handleReceive(conn net.Conn, remotePath string) {
	s.handleReceiveMode(conn, remotePath, true)
}

func (s *Server) handleReceiveMode(conn net.Conn, remotePath string, withChecksum bool) {
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

	trailer := fmt.Sprintf("END %d\n", stat.Size())
	if withChecksum {
		_, digest, copyErr := checksum.CopyNWithSHA256Buffer(conn, file, stat.Size(), s.bufferSize)
		err = copyErr
		trailer = fmt.Sprintf("CHECKSUM %s\n", hex.EncodeToString(digest[:]))
	} else {
		_, err = checksum.CopyNBuffer(conn, file, stat.Size(), s.bufferSize)
	}
	if err != nil {
		s.sendError(conn, fmt.Sprintf("send failed: %v", err))
		return
	}

	fmt.Fprint(conn, trailer)
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

// Stop closes listeners, active sessions and streams, and waits for handlers.
// It is safe to call concurrently and repeatedly, including before Start.
func (s *Server) Stop() error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		listener, quicListener, group := s.listener, s.quicListener, s.quicGroup
		sweeper := s.sweeperDone
		s.sweeperDone = nil
		clients := make([]net.Conn, 0, len(s.clients))
		for conn := range s.clients {
			clients = append(clients, conn)
		}
		sessions := make([]*quic.Conn, 0, len(s.quicConns))
		for conn := range s.quicConns {
			sessions = append(sessions, conn)
		}
		s.mu.Unlock()
		if sweeper != nil {
			close(sweeper)
		}
		if listener != nil {
			listener.Close()
		}
		if group != nil {
			// Closes every listener in the SO_REUSEPORT group, including
			// quicListener, plus the transports and their UDP sockets.
			group.close()
		} else if quicListener != nil {
			quicListener.Close()
		}
		for _, conn := range sessions {
			conn.CloseWithError(0, "server stopped")
		}
		for _, conn := range clients {
			conn.Close()
		}
		// Ranged transfers in flight are abandoned: their staged files are
		// removed so a stopped receiver leaves no partial garbage behind.
		// Handlers still copying fail on the closed connection or file.
		s.cleanupTokens()
	})
	s.workers.Wait()
	return nil
}

// Addr returns the bound listen address once Start has created the listener.
func (s *Server) Addr() net.Addr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.quicListener != nil {
		return s.quicListener.Addr()
	}
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}
