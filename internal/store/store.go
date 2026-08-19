package store

import (
	"context"
	"crypto/sha256"

	"github.com/clickety-clacks/lachesis/internal/model"
)

type Adapter interface {
	Read(context.Context) ([]byte, error)
	Digest(context.Context) ([32]byte, error)
	Commit(context.Context, [32]byte, []byte) error
	Binding() model.StoreBinding
}

func DigestBytes(b []byte) [32]byte { return sha256.Sum256(b) }
