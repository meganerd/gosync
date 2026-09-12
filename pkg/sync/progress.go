package sync

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gbjohnso/gosync/pkg/progress"
	"github.com/gbjohnso/gosync/pkg/transport"
)

// progressPaths describes the paths used by transfers, without changing routing.
// Receiver metadata takes precedence over the requested destination: gosync
// servers store relative SEND paths under their own configured base directory.
func (s *Sync) progressPaths() (string, string) {
	source := s.source
	if absolute, err := filepath.Abs(source); err == nil {
		source = absolute
	}
	authority := s.destination
	if i := strings.Index(authority, ":"); i >= 0 {
		authority = authority[:i]
	}
	authority = fmt.Sprintf("%s:%d", authority, extractPort(s.destination))

	base := s.remoteBase
	if provider, ok := s.transport.(transport.RemoteBaseProvider); ok && provider.RemoteBase() != "" {
		base = provider.RemoteBase()
	}
	relative := "."
	if info, err := os.Stat(s.source); err == nil && !info.IsDir() {
		relative = filepath.ToSlash(s.buildRemotePath(s.source))
	} else if s.createTargetDir {
		relative = filepath.Base(s.source)
	}
	if base != "" {
		target := path.Join(base, relative)
		if !path.IsAbs(target) && !(len(target) > 2 && target[1] == ':') {
			target += " (remote-relative; absolute base unavailable)"
		}
		return source, authority + ":" + target
	}
	return source, authority + ":" + relative + " (relative to remote base; absolute base unavailable)"
}

// transferProgress is rendered only by the result consumer. Transport workers
// update its thread-safe tracker without writing to the terminal.
type transferProgress struct {
	tracker     *progress.Progress
	writer      io.Writer
	terminal    bool
	streaming   bool
	width       int
	source      string
	destination string
}

func (p *transferProgress) clear() {
	if p.terminal && p.width > 0 {
		fmt.Fprintf(p.writer, "\r%s\r", strings.Repeat(" ", p.width))
		p.width = 0
	}
}

func (p *transferProgress) render(final bool) {
	if !p.terminal || p.width == 0 || final {
		p.clear()
		if p.source != "" || p.destination != "" {
			fmt.Fprintf(p.writer, "Source: %s\nDestination: %s\n", p.source, p.destination)
		}
	}
	info := p.tracker.GetInfo()
	percent := info.Percent
	if info.TotalBytes == 0 && info.CompletedFiles == info.TotalFiles && info.FailedFiles == 0 {
		percent = 100
	}
	eta := "--"
	if info.Speed > 0 {
		eta = info.ETA.Round(time.Second).String()
	}
	if final && info.CompletedFiles == info.TotalFiles {
		eta = "0s"
	}
	line := fmt.Sprintf("%.1f%% %s/%s %s/s ETA %s | Files: %d/%d, %d failed",
		percent, formatSize(info.TransferredBytes), formatSize(info.TotalBytes),
		formatSize(int64(info.Speed)), eta, info.CompletedFiles, info.TotalFiles, info.FailedFiles)
	if p.terminal {
		padding := ""
		if p.width > len(line) {
			padding = strings.Repeat(" ", p.width-len(line))
		}
		fmt.Fprintf(p.writer, "\r%s%s", line, padding)
		p.width = len(line)
		if final {
			fmt.Fprintln(p.writer)
			p.width = 0
		}
	} else {
		fmt.Fprintln(p.writer, line)
	}
}
