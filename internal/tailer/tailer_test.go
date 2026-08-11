package tailer

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestPollReturnsOnlyCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	tl := New(path)

	write(t, path, "one\ntwo\npart")
	lines, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || string(lines[0]) != "one" || string(lines[1]) != "two" {
		t.Fatalf("got %q", lines)
	}

	// The partial line is held back until its newline arrives.
	write(t, path, "ial\n")
	lines, _ = tl.Poll()
	if len(lines) != 1 || string(lines[0]) != "partial" {
		t.Fatalf("partial line not reassembled: %q", lines)
	}
}

func TestPollIsIdempotentWhenNothingChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	tl := New(path)
	write(t, path, "a\n")
	if _, err := tl.Poll(); err != nil {
		t.Fatal(err)
	}
	lines, _ := tl.Poll()
	if len(lines) != 0 {
		t.Fatalf("re-polling an unchanged file returned %q", lines)
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	tl := New(filepath.Join(t.TempDir(), "absent.jsonl"))
	lines, err := tl.Poll()
	if err != nil || lines != nil {
		t.Fatalf("lines=%q err=%v", lines, err)
	}
}

func TestTruncatedFileIsRereadFromTheStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	tl := New(path)
	write(t, path, "first\nsecond\n")
	tl.Poll()

	if err := os.WriteFile(path, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, _ := tl.Poll()
	if len(lines) != 1 || string(lines[0]) != "fresh" {
		t.Fatalf("rewritten file not re-read: %q", lines)
	}
}

func TestSeekEndSkipsExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, path, "old\n")
	tl := New(path)
	tl.SeekEnd()

	if lines, _ := tl.Poll(); len(lines) != 0 {
		t.Fatalf("SeekEnd should skip history, got %q", lines)
	}
	write(t, path, "new\n")
	lines, _ := tl.Poll()
	if len(lines) != 1 || string(lines[0]) != "new" {
		t.Fatalf("appends after SeekEnd should be read: %q", lines)
	}
}
