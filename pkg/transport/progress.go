package transport

import "io"

// ProgressReporter is an optional capability for reporting live send progress.
// Callbacks receive payload byte deltas successfully written by SendFile or
// SendStream, excluding protocol headers and checksums. A write is not an
// acknowledgement that the remote peer has stored the data.
//
// Configure the callback before starting sends; do not change it while sends
// are in flight. The caller must make the callback safe for concurrent use:
// concurrent workers report independently, and QUIC fan-out also reports from
// several data connections within a single file, summing to the file's size.
// Bytes from a range that fails and is retried are counted again, so live
// progress can briefly exceed the bytes the receiver keeps.
// Callbacks run synchronously during writes and must not reenter the transport.
// Passing nil removes the callback.
type ProgressReporter interface {
	SetProgressCallback(func(int64))
}

// progressWriter wraps only the payload destination, not protocol writes or
// hashing, so partial writes are counted even when the write returns an error.
type progressWriter struct {
	writer   io.Writer
	callback func(int64)
}

func (w progressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 && w.callback != nil {
		w.callback(int64(n))
	}
	return n, err
}
