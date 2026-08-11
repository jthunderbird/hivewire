// Package tailer follows an append-only text file and yields complete lines.
package tailer

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// Tailer reads newly appended complete lines from a file across repeated Poll
// calls. A partial trailing line is held back until its newline arrives, so a
// consumer never sees half a JSON record.
type Tailer struct {
	Path string

	off     int64
	partial []byte
}

func New(path string) *Tailer { return &Tailer{Path: path} }

// Offset reports how far into the file the tailer has consumed.
func (t *Tailer) Offset() int64 { return t.off }

// SeekEnd skips existing content so only future appends are read.
func (t *Tailer) SeekEnd() {
	if fi, err := os.Stat(t.Path); err == nil {
		t.off = fi.Size()
		t.partial = nil
	}
}

// Poll returns any complete lines appended since the previous call.
// A file that shrinks (rotated or rewritten) is re-read from the start.
func (t *Tailer) Poll() ([][]byte, error) {
	fi, err := os.Stat(t.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if fi.Size() < t.off {
		t.off = 0
		t.partial = nil
	}
	if fi.Size() == t.off {
		return nil, nil
	}

	f, err := os.Open(t.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return nil, err
	}
	chunk, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	t.off += int64(len(chunk))

	buf := append(t.partial, chunk...)
	nl := bytes.LastIndexByte(buf, '\n')
	if nl < 0 {
		t.partial = buf
		return nil, nil
	}
	complete := buf[:nl]
	t.partial = append([]byte(nil), buf[nl+1:]...)

	var out [][]byte
	for _, line := range bytes.Split(complete, []byte{'\n'}) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue
		}
		out = append(out, append([]byte(nil), line...))
	}
	return out, nil
}
