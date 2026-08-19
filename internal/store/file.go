package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/clickety-clacks/lachesis/internal/model"
)

var ErrChanged = errors.New("credential store changed")

type File struct{ binding model.StoreBinding }

func NewFile(home, credentialPath string) (*File, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(credentialPath) {
		return nil, errors.New("credential home and path must be absolute")
	}
	home, err := filepath.EvalSymlinks(home)
	if errors.Is(err, os.ErrNotExist) {
		home, err = filepath.Abs(home)
	}
	if err != nil {
		return nil, err
	}
	credentialPath = filepath.Clean(credentialPath)
	return &File{binding: model.StoreBinding{Kind: "file", Home: home, CredentialPath: credentialPath}}, nil
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
	if err != nil {
		return err
	}
	if sha256.Sum256(current) != expected {
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
	if err = os.Rename(name, f.binding.CredentialPath); err != nil {
		return err
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
