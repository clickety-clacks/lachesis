package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/clickety-clacks/lachesis/internal/api"
	"github.com/clickety-clacks/lachesis/internal/core"
	"github.com/clickety-clacks/lachesis/internal/model"
	"github.com/clickety-clacks/lachesis/internal/processcheck"
	"github.com/clickety-clacks/lachesis/internal/provider"
	"github.com/clickety-clacks/lachesis/internal/provider/claude"
	"github.com/clickety-clacks/lachesis/internal/provider/codex"
	"github.com/clickety-clacks/lachesis/internal/teach"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		startupError(teach.New(teach.InvalidRequest, "The command must be serve.", "health", nil, map[string]any{}, nil, "run lachesis serve"))
		os.Exit(2)
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(ioDiscard{})
	home, _ := os.UserHomeDir()
	stateDir := fs.String("state-dir", filepath.Join(home, "Library", "Application Support", "Lachesis"), "absolute state directory")
	if fs.Parse(os.Args[2:]) != nil || fs.NArg() != 0 || !filepath.IsAbs(*stateDir) {
		startupError(teach.New(teach.InvalidRequest, "serve accepts only --state-dir with an absolute path.", "health", nil, map[string]any{}, nil, "run lachesis serve --state-dir /absolute/path"))
		os.Exit(2)
	}
	svc, d := core.OpenService(*stateDir, []provider.Adapter{codex.New(nil), claude.New(nil)}, processcheck.PS{})
	if d != nil {
		startupError(d)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc.Start(ctx)
	defer svc.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:7843")
	if err != nil {
		startupError(teach.New(teach.UpstreamUnavailable, "The loopback listener could not start.", "health", nil, map[string]any{"address": "127.0.0.1:7843"}, nil, "stop the existing listener and retry"))
		os.Exit(1)
	}
	server := &http.Server{Handler: api.New(svc).Handler(), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-done
	case err = <-done:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "server stopped")
		}
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
func startupError(d *model.ErrorDetail) {
	_ = json.NewEncoder(os.Stderr).Encode(model.ErrorEnvelope{Error: d, RequestID: "startup"})
}
