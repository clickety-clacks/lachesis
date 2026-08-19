//go:build !darwin

package store

import (
	"context"
	"errors"

	"github.com/clickety-clacks/lachesis/internal/model"
)

var ErrAtomicUnavailable = errors.New("native keychain atomic commit is unavailable")

type Keychain struct{ binding model.StoreBinding }

func NewKeychain(service, account string) (*Keychain, error) {
	if service == "" || account == "" {
		return nil, errors.New("keychain service and account are required")
	}
	return &Keychain{binding: model.StoreBinding{Kind: "keychain", Service: service, Account: account}}, nil
}
func (k *Keychain) Binding() model.StoreBinding                    { return k.binding }
func (k *Keychain) Read(context.Context) ([]byte, error)           { return nil, ErrAtomicUnavailable }
func (k *Keychain) Digest(context.Context) ([32]byte, error)       { return [32]byte{}, ErrAtomicUnavailable }
func (k *Keychain) Commit(context.Context, [32]byte, []byte) error { return ErrAtomicUnavailable }
