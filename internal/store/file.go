package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/clickety-clacks/lachesis/internal/model"
)

var ErrChanged = errors.New("credential store changed")

type File struct{ binding model.StoreBinding }

func NewFile(home, credentialPath string) (*File, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(credentialPath) {
		return nil, errors.New("credential home and path must be absolute")
	}
	home, err := canonicalPath(home)
	if err != nil {
		return nil, err
	}
	credentialPath, err = canonicalPath(credentialPath)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(home, credentialPath)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, errors.New("credential path must be inside its canonical home")
	}
	return &File{binding: model.StoreBinding{Kind: "file", Home: home, CredentialPath: credentialPath}}, nil
}

// canonicalPath resolves every existing symlink in path while preserving a
// missing suffix. A missing credential can therefore be recreated at the same
// canonical authority without replacing a symlink or retaining two stores.
func canonicalPath(path string) (string, error) {
	path = filepath.Clean(path)
	current := path
	missing := []string{}
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func (f *File) Binding() model.StoreBinding          { return f.binding }
func (f *File) Read(context.Context) ([]byte, error) { return os.ReadFile(f.binding.CredentialPath) }
func (f *File) Digest(ctx context.Context) ([32]byte, error) {
	b, err := f.Read(ctx)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func (f *File) Commit(ctx context.Context, expected [32]byte, candidate []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	current, err := os.ReadFile(f.binding.CredentialPath)
	create := errors.Is(err, os.ErrNotExist)
	if err != nil && !create {
		return err
	}
	if create {
		if expected != ([32]byte{}) {
			return ErrChanged
		}
	} else if sha256.Sum256(current) != expected {
		return ErrChanged
	}
	dir := filepath.Dir(f.binding.CredentialPath)
	tmp, err := os.CreateTemp(dir, ".credential-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(candidate)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if create {
		if err = os.Link(name, f.binding.CredentialPath); err != nil {
			if errors.Is(err, os.ErrExist) {
				return ErrChanged
			}
			return err
		}
		if err = os.Remove(name); err != nil {
			return err
		}
	} else {
		current, err = os.ReadFile(f.binding.CredentialPath)
		if err != nil || sha256.Sum256(current) != expected {
			return ErrChanged
		}
		if err = os.Rename(name, f.binding.CredentialPath); err != nil {
			return err
		}
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return fmt.Errorf("sync credential directory: %w", err)
	}
	return nil
}
