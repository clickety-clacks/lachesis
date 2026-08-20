package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/clickety-clacks/lachesis/internal/model"
)

type markerState string

const (
	markerAbsent  markerState = "absent"
	markerPresent markerState = "present"
	markerUnknown markerState = "unknown"
)

type cleanupTarget interface {
	cleanupTarget()
}

type providerHomeCleanupTarget struct {
	provider model.Provider
	home     string
}

func (providerHomeCleanupTarget) cleanupTarget() {}

type transactionCleanupTarget struct {
	path string
}

func (transactionCleanupTarget) cleanupTarget() {}

type providerHomeCleanupSkippedEvent struct {
	Event    string         `json:"event"`
	Provider model.Provider `json:"provider"`
	Reason   string         `json:"reason"`
	Action   string         `json:"action"`
}

func (s *Service) preserveProviderHome(target providerHomeCleanupTarget) markerState {
	state := s.providerHomeMarkerState(target)
	switch state {
	case markerPresent:
		s.writeProviderHomeCleanupSkipped(target.provider, state)
	case markerUnknown:
		s.writeProviderHomeCleanupSkipped(target.provider, state)
	}
	return state
}

func (s *Service) providerHomeMarkerState(target providerHomeCleanupTarget) markerState {
	root := filepath.Clean(filepath.Join(s.stateDir, "providers", string(target.provider)))
	home := filepath.Clean(target.home)
	if filepath.Dir(home) != root {
		return markerUnknown
	}
	info, err := s.lstat(home)
	if err != nil || !info.IsDir() {
		return markerUnknown
	}
	adapter := s.adapters[target.provider]
	if adapter == nil {
		return markerUnknown
	}
	binding := adapter.ManagedBinding(home)
	marker := filepath.Clean(binding.CredentialPath)
	if binding.Kind != "file" || filepath.Clean(binding.Home) != home || !filepath.IsAbs(marker) || filepath.Dir(marker) != home {
		return markerUnknown
	}
	_, err = s.lstat(marker)
	switch {
	case err == nil:
		return markerPresent
	case errors.Is(err, os.ErrNotExist):
		return markerAbsent
	default:
		return markerUnknown
	}
}

func (s *Service) writeProviderHomeCleanupSkipped(providerName model.Provider, state markerState) {
	var reason string
	switch state {
	case markerPresent:
		reason = "credential_present"
	case markerUnknown:
		reason = "credential_status_unknown"
	default:
		return
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	_ = json.NewEncoder(s.events).Encode(providerHomeCleanupSkippedEvent{
		Event:    "provider_home_cleanup_skipped",
		Provider: providerName,
		Reason:   reason,
		Action:   "preserved",
	})
}
