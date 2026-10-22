package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
)

// ProviderRegistry belongs to the global private configuration, never a world.
// Integration with legacy APIConfig is deliberately deferred to 2A-2.
type ProviderRegistry struct {
	DefaultProvider string              `json:"default_provider,omitempty"`
	Providers       map[string]Provider `json:"providers"`
}

type Provider struct {
	Name           string         `json:"name"`
	Protocol       string         `json:"protocol"`
	BaseURL        string         `json:"base_url"`
	APIKey         string         `json:"api_key,omitempty"`
	Models         []string       `json:"models,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	MaxParallel    int            `json:"max_parallel,omitempty"`
	URLStrict      bool           `json:"url_strict,omitempty"`
	ExtraBody      map[string]any `json:"extra_body,omitempty"`
}

// PublicProvider is an explicit allowlist. ExtraBody can contain credentials,
// so it is not included in the public view either.
type PublicProvider struct {
	Name           string   `json:"name"`
	Protocol       string   `json:"protocol"`
	BaseURL        string   `json:"base_url"`
	Models         []string `json:"models"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	MaxParallel    int      `json:"max_parallel"`
	URLStrict      bool     `json:"url_strict"`
	HasAPIKey      bool     `json:"has_api_key"`
}

func (r ProviderRegistry) PublicView() map[string]PublicProvider {
	out := make(map[string]PublicProvider, len(r.Providers))
	for id, p := range r.Providers {
		out[id] = PublicProvider{
			Name: p.Name, Protocol: p.Protocol, BaseURL: p.BaseURL,
			Models: append([]string{}, p.Models...), TimeoutSeconds: p.TimeoutSeconds,
			MaxParallel: p.MaxParallel, URLStrict: p.URLStrict,
			HasAPIKey: strings.TrimSpace(p.APIKey) != "",
		}
	}
	return out
}

func (r ProviderRegistry) Validate() error {
	if r.DefaultProvider != "" {
		if _, ok := r.Providers[r.DefaultProvider]; !ok {
			return fmt.Errorf("default provider does not exist")
		}
	}
	for id, p := range r.Providers {
		if !validConfigID(id) || strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("invalid provider identifier or name")
		}
		if p.Protocol != "openai_compatible" {
			return fmt.Errorf("unsupported provider protocol for %s", id)
		}
		u, err := url.Parse(p.BaseURL)
		// Never print a URL in errors: it could contain credentials.
		if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(p.BaseURL) != p.BaseURL {
			return fmt.Errorf("provider %s requires an HTTP(S) base URL without embedded credentials, query or fragment", id)
		}
		if p.TimeoutSeconds < 0 || p.TimeoutSeconds > 3600 || p.MaxParallel < 0 || p.MaxParallel > 64 {
			return fmt.Errorf("provider %s timeout must be within 0..3600 and parallelism within 0..64", id)
		}
		seen := map[string]bool{}
		for _, model := range p.Models {
			if strings.TrimSpace(model) == "" || model != strings.TrimSpace(model) || seen[model] {
				return fmt.Errorf("invalid or duplicate model for provider %s", id)
			}
			seen[model] = true
		}
		if _, err := json.Marshal(p.ExtraBody); err != nil {
			return fmt.Errorf("provider %s extra_body is not JSON compatible", id)
		}
	}
	return nil
}

func validConfigID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

type ModelRef struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

func (m ModelRef) Validate(r ProviderRegistry) error {
	p, ok := r.Providers[m.ProviderID]
	if !ok || strings.TrimSpace(m.Model) == "" || strings.TrimSpace(m.Model) != m.Model {
		return fmt.Errorf("model reference requires an existing provider and nonempty model")
	}
	// An empty catalog permits manual model entry. A nonempty one is explicit.
	if len(p.Models) > 0 {
		for _, model := range p.Models {
			if model == m.Model {
				return nil
			}
		}
		return fmt.Errorf("model is not in the provider catalog")
	}
	return nil
}

type AgentImportance string

const (
	ImportanceCore       AgentImportance = "core"
	ImportanceSupporting AgentImportance = "supporting"
	ImportanceBackground AgentImportance = "background"
)

func (i AgentImportance) Valid() bool {
	return i == ImportanceCore || i == ImportanceSupporting || i == ImportanceBackground
}

// TierPolicy contains defaults for a class of characters, never credentials.
type TierPolicy struct {
	ModelRef
	MaxTokens           int      `json:"max_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	ContextBudgetTokens int      `json:"context_budget_tokens,omitempty"`
	MaxParallel         int      `json:"max_parallel,omitempty"`
	WakePolicy          string   `json:"wake_policy,omitempty"`
	ReflectionEvery     int      `json:"reflection_every,omitempty"`
}

type CharacterAgentConfig struct {
	Importance    AgentImportance `json:"importance"`
	ModelOverride *ModelRef       `json:"model_override,omitempty"`
}

// AgentRuntimeConfig is the world-scoped config. Revision is a configuration
// revision, separate from world event revisions; concurrency enforcement is 2A-4.
type AgentRuntimeConfig struct {
	ExecutionMode string                          `json:"execution_mode,omitempty"`
	SchemaVersion int                             `json:"schema_version"`
	Revision      int                             `json:"revision"`
	Tiers         map[AgentImportance]TierPolicy  `json:"tiers"`
	Characters    map[string]CharacterAgentConfig `json:"characters"`
}

func (c AgentRuntimeConfig) Validate(r ProviderRegistry) error {
	if c.ExecutionMode != "" && c.ExecutionMode != "legacy" && c.ExecutionMode != "independent" {
		return fmt.Errorf("invalid agent execution mode")
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if c.SchemaVersion != 1 || c.Revision < 0 || len(c.Tiers) != 3 {
		return fmt.Errorf("agent config requires schema version 1, nonnegative revision and exactly three tiers")
	}
	for _, importance := range []AgentImportance{ImportanceCore, ImportanceSupporting, ImportanceBackground} {
		p, ok := c.Tiers[importance]
		if !ok {
			return fmt.Errorf("missing agent tier: %s", importance)
		}
		if err := p.ModelRef.Validate(r); err != nil {
			return fmt.Errorf("tier %s: %w", importance, err)
		}
		if p.MaxTokens < 0 || p.ContextBudgetTokens < 0 || p.MaxParallel < 0 || p.MaxParallel > 64 || p.ReflectionEvery < 0 {
			return fmt.Errorf("negative limits in tier %s", importance)
		}
		if p.Temperature != nil && (math.IsNaN(*p.Temperature) || math.IsInf(*p.Temperature, 0) || *p.Temperature < 0 || *p.Temperature > 2) {
			return fmt.Errorf("temperature must be within 0..2")
		}
		if p.WakePolicy != "" && p.WakePolicy != "every_relevant_turn" && p.WakePolicy != "on_involvement" && p.WakePolicy != "on_direct_event" {
			return fmt.Errorf("invalid wake policy")
		}
	}
	for id, character := range c.Characters {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id || !character.Importance.Valid() {
			return fmt.Errorf("invalid character identifier or importance")
		}
		if character.ModelOverride != nil {
			if err := character.ModelOverride.Validate(r); err != nil {
				return fmt.Errorf("character model override: %w", err)
			}
		}
	}
	return nil
}

// DecodeAgentRuntimeConfig rejects unknown fields, including accidentally pasted
// API keys, instead of silently persisting them into a world configuration.
func DecodeAgentRuntimeConfig(data []byte, r ProviderRegistry) (AgentRuntimeConfig, error) {
	var c AgentRuntimeConfig
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid agent runtime configuration")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("expected one configuration object")
	}
	return c, c.Validate(r)
}

// ResolveModel does not silently change providers when configuration is invalid.
// It resolves references only; execution, timeouts and rate limits are Phase 2B.
func (c AgentRuntimeConfig) ResolveModel(characterID string, r ProviderRegistry) (ModelRef, error) {
	if err := c.Validate(r); err != nil {
		return ModelRef{}, err
	}
	character, ok := c.Characters[characterID]
	if !ok {
		return ModelRef{}, fmt.Errorf("character has not been registered")
	}
	if character.ModelOverride != nil {
		return *character.ModelOverride, nil
	}
	return c.Tiers[character.Importance].ModelRef, nil
}
