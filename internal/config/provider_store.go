package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// APIRevision includes private data in its hash without returning those data.
// The opaque revision detects edits through both legacy and Provider endpoints.
func APIRevision(c *APIConfig) string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ValidateSavedAgentReferences protects the same invariant for legacy and new
// configuration save paths. It reads world model references, never private memory.
func ValidateSavedAgentReferences(apiPath string, next *APIConfig) error {
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(apiPath), "worlds", "*", "agent_runtime.json"))
	if err != nil {
		return err
	}
	providers := ProviderRegistry{DefaultProvider: next.DefaultProvider, Providers: next.Providers}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var c AgentRuntimeConfig
		if err := json.Unmarshal(b, &c); err != nil {
			return fmt.Errorf("saved Agent configuration cannot be verified")
		}
		if err := c.Validate(providers); err != nil {
			return fmt.Errorf("saved Agent model references would become invalid: %w", err)
		}
	}
	return nil
}

// ReadAPIConfig is pure even when no configuration exists yet.
func ReadAPIConfig(path string) (*APIConfig, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return DefaultAPIConfig(), nil
	}
	return LoadAPIConfig(path)
}

type ProviderUpdate struct {
	Revision        string                     `json:"revision"`
	DefaultProvider *string                    `json:"default_provider,omitempty"`
	Providers       map[string]json.RawMessage `json:"providers,omitempty"`
	Delete          []string                   `json:"delete,omitempty"`
}

func UpdateProviders(path string, update ProviderUpdate) (*APIConfig, error) {
	unlock := LockAPIFile(path)
	defer unlock()
	base, err := ReadAPIConfig(path)
	if err != nil {
		return nil, err
	}
	if update.Revision == "" || update.Revision != APIRevision(base) {
		return nil, ErrRevisionConflict
	}
	patch := map[string]any{"providers": update.Providers}
	if update.Providers == nil {
		delete(patch, "providers")
	}
	if update.DefaultProvider != nil {
		patch["default_provider"] = *update.DefaultProvider
	}
	// The compatibility Provider is edited through top-level fields so old model
	// callers retain their endpoint and credentials after a new-style save.
	if raw, ok := update.Providers[LegacyProviderID]; ok {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return nil, fmt.Errorf("invalid compatibility provider")
		}
		for src, dst := range map[string]string{"base_url": "base_url", "api_key": "api_key", "clear_api_key": "clear_api_key", "timeout_seconds": "http_timeout_seconds", "url_strict": "url_strict", "extra_body": "extra_body"} {
			if value, ok := fields[src]; ok {
				patch[dst] = value
			}
		}
	}
	b, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	next, err := MergeAPIConfig(base, b)
	if err != nil {
		return nil, err
	}
	for _, id := range update.Delete {
		if id == LegacyProviderID {
			return nil, fmt.Errorf("compatibility provider cannot be deleted; clear its key or edit legacy settings")
		}
		if _, ok := next.Providers[id]; !ok {
			return nil, fmt.Errorf("provider to delete does not exist")
		}
		delete(next.Providers, id)
	}
	if err := (ProviderRegistry{DefaultProvider: next.DefaultProvider, Providers: next.Providers}).Validate(); err != nil {
		return nil, err
	}
	if err := SaveAPIConfig(next, path); err != nil {
		return nil, err
	}
	return next, nil
}
