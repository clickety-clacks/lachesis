package provider

import (
	"context"
	"io"
	"time"

	"github.com/clickety-clacks/lachesis/internal/model"
)

type Credential struct {
	Raw          []byte
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
	Expiry       time.Time
	Scopes       []string
}

type LoginProcess interface {
	Output() io.ReadCloser
	Wait() error
	Terminate() error
	Kill() error
}

type CodeSubmitter interface {
	SubmitCode(string) error
}

type Adapter interface {
	Name() model.Provider
	CLIAvailable() bool
	DefaultBinding() (model.StoreBinding, *model.ErrorDetail)
	ManagedBinding(home string) model.StoreBinding
	ParseCredential([]byte) (Credential, *model.ErrorDetail)
	Usage(context.Context, Credential) (*model.UsageSample, *model.ErrorDetail)
	Refresh(context.Context, Credential) ([]byte, *model.ErrorDetail)
	StartLogin(context.Context, string) (LoginProcess, *model.ErrorDetail)
}
