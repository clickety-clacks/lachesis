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

func TestFileCommitCreatesMissingCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	s, err := NewFile(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Commit(context.Background(), [32]byte{}, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "new" {
		t.Fatalf("read created credential: %q, %v", b, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("created mode: %v", info.Mode().Perm())
	}
}

func TestFileResolvesCredentialSymlinkToOneAuthority(t *testing.T) {
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "target")
	if err := os.Mkdir(targetDir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "auth.json")
	if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "auth-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s, err := NewFile(dir, link)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Binding().CredentialPath; got != target {
		t.Fatalf("canonical path %q, want %q", got, target)
	}
	digest, err := s.Digest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Commit(context.Background(), digest, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "new" {
		t.Fatalf("target content: %q, %v", b, err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("original symlink changed: %v", info.Mode())
	}
}

func TestFileResolvesMissingCredentialThroughSymlinkedHome(t *testing.T) {
	dir := t.TempDir()
	targetHome := filepath.Join(dir, "real-home")
	if err := os.Mkdir(targetHome, 0700); err != nil {
		t.Fatal(err)
	}
	linkedHome := filepath.Join(dir, "linked-home")
	if err := os.Symlink(targetHome, linkedHome); err != nil {
		t.Fatal(err)
	}
	s, err := NewFile(linkedHome, filepath.Join(linkedHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(targetHome, "auth.json")
	if got := s.Binding().CredentialPath; got != want {
		t.Fatalf("canonical path %q, want %q", got, want)
	}
	if err = s.Commit(context.Background(), [32]byte{}, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(want); err != nil || string(b) != "new" {
		t.Fatalf("resolved create: %q, %v", b, err)
	}
}

func TestFileRejectsCredentialOutsideCanonicalHome(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	outside := filepath.Join(dir, "outside", "auth.json")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(outside), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFile(home, outside); err == nil {
		t.Fatal("outside credential path was accepted")
	}
}

func TestKeychainCommitFailsClosedWithoutAtomicProof(t *testing.T) {
	keychain, err := NewKeychain("synthetic-service", "synthetic-account")
	if err != nil {
		t.Fatal(err)
	}
	if err = keychain.Commit(context.Background(), [32]byte{}, []byte("synthetic")); err != ErrAtomicUnavailable {
		t.Fatalf("commit error = %v", err)
	}
}
