package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileCommitChecksDigestAndPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewFile(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.Digest(context.Background())
	if err = s.Commit(context.Background(), d, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "new" {
		t.Fatalf("got %q", b)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if err = s.Commit(context.Background(), d, []byte("bad")); err != ErrChanged {
		t.Fatalf("got %v", err)
	}
}
