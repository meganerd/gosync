// Package archive implements gosync's bounded, streaming directory archive.
package archive

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	Version          = 1
	MaxFrame         = 1 << 20
	MaxPath          = 4096
	MaxEntries       = 1_000_000
	headerSize       = 32
	recordEntry      = 2
	recordData       = 3
	recordEndEntry   = 5
	recordEndArchive = 6
)

type Policy string

const (
	PolicyStrict     Policy = "strict"
	PolicyPortable   Policy = "portable"
	PolicyBestEffort Policy = "best-effort"
)

type Options struct {
	Policy     Policy
	BufferSize int
}
type Result struct {
	Entries  uint64
	Bytes    uint64
	Warnings []string
}

type entry struct {
	ID    uint64 `json:"id"`
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Size  int64  `json:"size,omitempty"`
	Mode  uint32 `json:"mode,omitempty"`
	Mtime int64  `json:"mtime_ns,omitempty"`
	Link  string `json:"link,omitempty"`
}

type encoder struct {
	w    io.Writer
	seq  uint64
	wire hash.Hash
}

// writeRecord writes one bounded frame. The archive digest is maintained by
// hashing each complete frame as it is emitted, without buffering the archive.
func (e *encoder) writeRecord(typ byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("archive frame exceeds %d bytes", MaxFrame)
	}
	h := make([]byte, headerSize)
	copy(h, "GSA1")
	h[4] = typ
	binary.BigEndian.PutUint16(h[6:8], headerSize)
	binary.BigEndian.PutUint64(h[8:16], e.seq)
	binary.BigEndian.PutUint64(h[16:24], uint64(len(payload)))
	binary.BigEndian.PutUint32(h[24:28], crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)))
	if err := writeFull(e.w, h); err != nil {
		return err
	}
	if err := writeFull(e.w, payload); err != nil {
		return err
	}
	e.wire.Write(h)
	e.wire.Write(payload)
	e.seq++
	return nil
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// Send walks root lazily and streams file data in bounded frames.
func Send(ctx context.Context, w io.Writer, root string, opts Options) (Result, error) {
	var out Result
	select {
	case <-ctx.Done():
		return out, ctx.Err()
	default:
	}
	info, err := os.Stat(root)
	if err != nil {
		return out, err
	}
	if !info.IsDir() {
		return out, fmt.Errorf("archive source is not a directory")
	}
	enc := &encoder{w: w, wire: sha256.New()}
	var id uint64
	err = filepath.WalkDir(root, func(path string, de os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if out.Entries >= MaxEntries {
			return fmt.Errorf("archive entry limit exceeded")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if err := ValidatePath(rel); err != nil {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		id++
		en := entry{ID: id, Path: rel, Mode: uint32(fi.Mode().Perm()), Mtime: fi.ModTime().UnixNano()}
		switch {
		case fi.Mode().IsDir():
			en.Kind = "directory"
		case fi.Mode().IsRegular():
			en.Kind = "regular"
			en.Size = fi.Size()
		case fi.Mode()&os.ModeSymlink != 0:
			en.Kind = "symlink"
			en.Link, err = os.Readlink(path)
			if err != nil {
				return err
			}
			if err = ValidateLink(rel, en.Link); err != nil {
				return err
			}
			if opts.Policy == PolicyPortable {
				out.Warnings = append(out.Warnings, "skipped symlink: "+rel)
				return nil
			}
		default:
			return fmt.Errorf("unsafe or unsupported archive entry %q", rel)
		}
		meta, _ := json.Marshal(en)
		if err = enc.writeRecord(recordEntry, meta); err != nil {
			return err
		}
		out.Entries++
		if en.Kind != "regular" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		before := fi.ModTime()
		h := sha256.New()
		bufSize := opts.BufferSize
		if bufSize <= 0 || bufSize > MaxFrame-16 {
			bufSize = 64 << 10
		}
		buf := make([]byte, bufSize+16)
		var off int64
		for off < en.Size {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			want := int64(bufSize)
			if en.Size-off < want {
				want = en.Size - off
			}
			n, rerr := io.ReadFull(f, buf[16:16+want])
			if n > 0 {
				binary.BigEndian.PutUint64(buf, id)
				binary.BigEndian.PutUint64(buf[8:], uint64(off))
				h.Write(buf[16 : 16+n])
				if err = enc.writeRecord(recordData, buf[:16+n]); err != nil {
					return err
				}
				off += int64(n)
				out.Bytes += uint64(n)
			}
			if rerr != nil {
				return fmt.Errorf("source changed while reading %q: %w", rel, rerr)
			}
		}
		after, err := f.Stat()
		if err != nil {
			return err
		}
		if after.Size() != en.Size || !after.ModTime().Equal(before) {
			return fmt.Errorf("source changed while reading %q", rel)
		}
		end := make([]byte, 8+sha256.Size)
		binary.BigEndian.PutUint64(end, id)
		copy(end[8:], h.Sum(nil))
		return enc.writeRecord(recordEndEntry, end)
	})
	if err != nil {
		return out, err
	}
	sum := enc.wire.Sum(nil)
	final := make([]byte, 48)
	binary.BigEndian.PutUint64(final, out.Entries)
	binary.BigEndian.PutUint64(final[8:], out.Bytes)
	copy(final[16:], sum)
	if err = enc.writeRecord(recordEndArchive, final); err != nil {
		return out, err
	}
	return out, nil
}

type decoder struct {
	r    io.Reader
	seq  uint64
	wire hash.Hash
}

func (d *decoder) next() (byte, []byte, error) {
	h := make([]byte, headerSize)
	if _, err := io.ReadFull(d.r, h); err != nil {
		return 0, nil, err
	}
	if string(h[:4]) != "GSA1" || h[5] != 0 || binary.BigEndian.Uint16(h[6:8]) != headerSize || binary.BigEndian.Uint32(h[28:]) != 0 {
		return 0, nil, fmt.Errorf("malformed archive header")
	}
	if binary.BigEndian.Uint64(h[8:16]) != d.seq {
		return 0, nil, fmt.Errorf("archive sequence gap")
	}
	n := binary.BigEndian.Uint64(h[16:24])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("archive frame limit exceeded")
	}
	p := make([]byte, int(n))
	if _, err := io.ReadFull(d.r, p); err != nil {
		return 0, nil, err
	}
	if crc32.Checksum(p, crc32.MakeTable(crc32.Castagnoli)) != binary.BigEndian.Uint32(h[24:28]) {
		return 0, nil, fmt.Errorf("archive CRC mismatch")
	}
	d.seq++
	if h[4] != recordEndArchive {
		d.wire.Write(h)
		d.wire.Write(p)
	}
	return h[4], p, nil
}

// Receive extracts a stream beneath root. Regular files are committed only
// after their digest passes; staging files are removed on every error path.
func Receive(ctx context.Context, r io.Reader, root string, opts Options) (res Result, err error) {
	if fi, e := os.Lstat(root); e == nil && (!fi.IsDir() || fi.Mode()&os.ModeSymlink != 0) {
		return res, fmt.Errorf("unsafe extraction root")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return res, e
	}
	if err = os.MkdirAll(root, 0755); err != nil {
		return res, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return res, err
	}
	d := &decoder{r: r, wire: sha256.New()}
	var cur *entry
	var f *os.File
	var tmp string
	var h = sha256.New()
	var written int64
	var directories []entry
	seen := make(map[string]struct{})
	var lastID uint64
	cleanup := func() {
		if f != nil {
			f.Close()
		}
		if tmp != "" {
			os.Remove(tmp)
		}
	}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		typ, p, e := d.next()
		if e != nil {
			return res, e
		}
		switch typ {
		case recordEntry:
			if cur != nil {
				return res, fmt.Errorf("entry before previous entry ended")
			}
			var en entry
			if json.Unmarshal(p, &en) != nil {
				return res, fmt.Errorf("malformed entry")
			}
			if err = ValidatePath(en.Path); err != nil {
				return res, err
			}
			if en.ID != lastID+1 {
				return res, fmt.Errorf("non-monotonic archive entry id")
			}
			lastID = en.ID
			key := collisionKey(en.Path, runtime.GOOS)
			if _, exists := seen[key]; exists {
				return res, fmt.Errorf("archive name collision %q", en.Path)
			}
			seen[key] = struct{}{}
			if res.Entries >= MaxEntries {
				return res, fmt.Errorf("archive entry limit exceeded")
			}
			target := filepath.Join(root, filepath.FromSlash(en.Path))
			if err = ensureContained(root, target); err != nil {
				return res, err
			}
			if err = secureParents(root, filepath.Dir(target)); err != nil {
				return res, err
			}
			switch en.Kind {
			case "directory":
				err = os.Mkdir(target, os.FileMode(en.Mode))
				if errors.Is(err, os.ErrExist) {
					var x os.FileInfo
					x, err = os.Lstat(target)
					if err == nil && !x.IsDir() {
						err = fmt.Errorf("destination collision %q", en.Path)
					}
				}
				if err == nil {
					_ = os.Chmod(target, os.FileMode(en.Mode))
					directories = append(directories, en)
				}
			case "symlink":
				if opts.Policy == PolicyPortable {
					res.Warnings = append(res.Warnings, "skipped symlink: "+en.Path)
					break
				}
				if err = ValidateLink(en.Path, en.Link); err == nil {
					if _, e = os.Lstat(target); e == nil {
						err = fmt.Errorf("refusing to replace existing path %q", en.Path)
					} else if !errors.Is(e, os.ErrNotExist) {
						err = e
					} else {
						err = os.Symlink(en.Link, target)
					}
				}
			case "regular":
				if en.Size < 0 {
					return res, fmt.Errorf("negative file size")
				}
				tmp = target + fmt.Sprintf(".gosync-part-%d", os.Getpid())
				f, err = os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err == nil {
					cur = &en
					h = sha256.New()
					written = 0
				}
			default:
				err = fmt.Errorf("unsafe or unsupported archive kind %q", en.Kind)
			}
			if err != nil {
				return res, err
			}
			res.Entries++
		case recordData:
			if cur == nil || f == nil || len(p) < 16 {
				return res, fmt.Errorf("DATA outside regular entry")
			}
			if binary.BigEndian.Uint64(p) != cur.ID || int64(binary.BigEndian.Uint64(p[8:16])) != written {
				return res, fmt.Errorf("non-contiguous archive data")
			}
			n, er := f.Write(p[16:])
			if er != nil || n != len(p)-16 {
				return res, fmt.Errorf("write staged file: %w", er)
			}
			h.Write(p[16:])
			written += int64(n)
			res.Bytes += uint64(n)
			if written > cur.Size {
				return res, fmt.Errorf("file exceeds declared size")
			}
		case recordEndEntry:
			if cur == nil || len(p) != 40 || binary.BigEndian.Uint64(p) != cur.ID || written != cur.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), hex.EncodeToString(p[8:])) {
				return res, fmt.Errorf("archive file checksum mismatch")
			}
			target := filepath.Join(root, filepath.FromSlash(cur.Path))
			if err = f.Sync(); err != nil {
				return res, err
			}
			if err = f.Close(); err != nil {
				return res, err
			}
			f = nil
			if _, e = os.Lstat(target); e == nil {
				if e = os.Remove(target); e != nil {
					return res, e
				}
			} else if !errors.Is(e, os.ErrNotExist) {
				return res, e
			}
			if err = os.Rename(tmp, target); err != nil {
				return res, err
			}
			tmp = ""
			_ = os.Chmod(target, os.FileMode(cur.Mode))
			_ = os.Chtimes(target, time.Now(), time.Unix(0, cur.Mtime))
			cur = nil
		case recordEndArchive:
			if cur != nil || len(p) != 48 {
				return res, fmt.Errorf("incomplete archive")
			}
			if binary.BigEndian.Uint64(p) != res.Entries || binary.BigEndian.Uint64(p[8:]) != res.Bytes {
				return res, fmt.Errorf("archive totals mismatch")
			}
			sum := d.wire.Sum(nil)
			if !strings.EqualFold(hex.EncodeToString(sum), hex.EncodeToString(p[16:])) {
				return res, fmt.Errorf("archive checksum mismatch")
			}
			for i := len(directories) - 1; i >= 0; i-- {
				target := filepath.Join(root, filepath.FromSlash(directories[i].Path))
				_ = os.Chmod(target, os.FileMode(directories[i].Mode))
				_ = os.Chtimes(target, time.Now(), time.Unix(0, directories[i].Mtime))
			}
			return res, nil
		default:
			return res, fmt.Errorf("unknown archive record %d", typ)
		}
	}
}

func ValidatePath(p string) error {
	return validatePathForOS(p, runtime.GOOS)
}

// validatePathForOS keeps destination-name rules testable on every build host.
// The receiver still calls ValidatePath with its actual operating system.
func validatePathForOS(p, goos string) error {
	if p == "" || len(p) > MaxPath || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("unsafe archive path %q", p)
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || len(c) > 255 {
			return fmt.Errorf("unsafe archive path %q", p)
		}
		for _, r := range c {
			if r < 0x20 {
				return fmt.Errorf("unsafe archive path %q", p)
			}
		}
	}
	if vol := filepath.VolumeName(p); vol != "" {
		return fmt.Errorf("unsafe archive path %q", p)
	}
	first := strings.Split(p, "/")[0]
	if (len(first) >= 2 && first[1] == ':') || strings.HasPrefix(p, "//") {
		return fmt.Errorf("unsafe archive path %q", p)
	}
	if goos == "windows" {
		for _, c := range strings.Split(p, "/") {
			trimmed := strings.TrimRight(c, ". ")
			base := strings.ToUpper(strings.Split(trimmed, ".")[0])
			reservedPort := len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9'
			if trimmed != c || base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || reservedPort {
				return fmt.Errorf("unsafe archive path %q", p)
			}
		}
	}
	return nil
}

func collisionKey(path, goos string) string {
	if goos == "windows" || goos == "darwin" {
		return strings.ToLower(path)
	}
	return path
}
func ValidateLink(path, target string) error {
	if target == "" || filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.ContainsRune(target, 0) || strings.Contains(target, "\\") {
		return fmt.Errorf("unsafe link target %q", target)
	}
	base := filepath.ToSlash(filepath.Dir(path))
	clean := filepath.Clean(filepath.Join(base, filepath.FromSlash(target)))
	if clean == ".." || strings.HasPrefix(filepath.ToSlash(clean), "../") {
		return fmt.Errorf("link escapes archive root")
	}
	return nil
}
func ensureContained(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path escapes extraction root")
	}
	return nil
}
func secureParents(root, dir string) error {
	if err := ensureContained(root, dir); err != nil {
		return err
	}
	rel, _ := filepath.Rel(root, dir)
	cur := root
	for _, c := range strings.Split(rel, string(os.PathSeparator)) {
		if c == "." || c == "" {
			continue
		}
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(cur, 0755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe destination parent %q", cur)
		}
	}
	return nil
}
