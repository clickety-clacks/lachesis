package core

import (
	"github.com/clickety-clacks/lachesis/internal/model"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryCommitAndReload(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := model.RegistryAccount{ID: "00000000-0000-4000-8000-000000000001", Label: "one", Provider: model.ProviderCodex, Store: model.StoreBinding{Kind: "file", Home: dir, CredentialPath: filepath.Join(dir, "auth.json")}}
	if err = r.Add(a); err != nil {
		t.Fatal(err)
	}
	r2, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Snapshot().Accounts) != 1 {
		t.Fatal("account missing")
	}
	info, _ := os.Stat(filepath.Join(dir, "accounts.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}
