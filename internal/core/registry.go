package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/clickety-clacks/lachesis/internal/model"
)

type RegistryStore struct {
	mu   sync.RWMutex
	path string
	data model.Registry
}

func OpenRegistry(stateDir string) (*RegistryStore, error) {
	if !filepath.IsAbs(stateDir) {
		return nil, errors.New("state directory must be absolute")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(stateDir, 0700); err != nil {
		return nil, err
	}
	r := &RegistryStore{path: filepath.Join(stateDir, "accounts.json"), data: model.Registry{Version: 1, Accounts: []model.RegistryAccount{}}}
	b, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &r.data); err != nil {
		return nil, fmt.Errorf("malformed registry: %w", err)
	}
	if r.data.Version != 1 || r.data.Accounts == nil {
		return nil, errors.New("malformed registry: expected version 1 and accounts array")
	}
	return r, nil
}

func (r *RegistryStore) Snapshot() model.Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := r.data
	out.Accounts = append([]model.RegistryAccount(nil), r.data.Accounts...)
	return out
}

func (r *RegistryStore) Find(id string) (model.RegistryAccount, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.data.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return model.RegistryAccount{}, false
}

func (r *RegistryStore) FindStore(key string) (model.RegistryAccount, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.data.Accounts {
		if a.Store.CanonicalKey() == key {
			return a, true
		}
	}
	return model.RegistryAccount{}, false
}

func (r *RegistryStore) Add(a model.RegistryAccount) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.data.Accounts {
		if existing.ID == a.ID {
			return errors.New("duplicate account id")
		}
		if existing.Store.CanonicalKey() == a.Store.CanonicalKey() {
			return errors.New("duplicate store")
		}
	}
	next := r.data
	next.Accounts = append(append([]model.RegistryAccount(nil), r.data.Accounts...), a)
	if err := r.write(next); err != nil {
		return err
	}
	r.data = next
	return nil
}

func (r *RegistryStore) Remove(id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := r.data
	next.Accounts = make([]model.RegistryAccount, 0, len(r.data.Accounts))
	found := false
	for _, a := range r.data.Accounts {
		if a.ID == id {
			found = true
			continue
		}
		next.Accounts = append(next.Accounts, a)
	}
	if !found {
		return false, nil
	}
	if err := r.write(next); err != nil {
		return false, err
	}
	r.data = next
	return true, nil
}

func (r *RegistryStore) write(next model.Registry) error {
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".accounts-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(b)
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
	if err = os.Rename(tmpName, r.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(r.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
