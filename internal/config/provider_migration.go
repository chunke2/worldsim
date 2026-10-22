package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

const LegacyProviderID = "legacy-default"

// MigrateProviders builds a compatibility view in memory. Load never rewrites
// an existing file; the next explicit save persists it atomically.
// The reserved legacy provider mirrors legacy fields, preserving existing callers.
func (c *APIConfig) MigrateProviders() error {
	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	if strings.TrimSpace(c.BaseURL) != "" {
		c.Providers[LegacyProviderID] = Provider{
			Name: "原有 API", Protocol: "openai_compatible", BaseURL: c.BaseURL,
			APIKey: c.APIKey, TimeoutSeconds: c.HTTPTimeoutSeconds,
			URLStrict: c.URLStrict, ExtraBody: c.ExtraBody,
			// Leave the catalog open: legacy callers can select any tier model.
		}
		if c.DefaultProvider == "" {
			c.DefaultProvider = LegacyProviderID
		}
	}
	return (ProviderRegistry{DefaultProvider: c.DefaultProvider, Providers: c.Providers}).Validate()
}

func mergePrivatePatch(c *APIConfig, fields map[string]json.RawMessage) error {
	// Work with a field map so blank password inputs never replace saved keys.
	explicitKey := credentialExplicit(fields)
	if err := preserveCredential(fields, c.APIKey); err != nil {
		return err
	}
	if endpoint, ok := fields["base_url"]; ok && c.APIKey != "" {
		var next string
		if err := json.Unmarshal(endpoint, &next); err != nil {
			return fmt.Errorf("invalid base URL")
		}
		if next != c.BaseURL && !explicitKey {
			return fmt.Errorf("changing the API address requires a new key or clear_api_key=true")
		}
	}
	if raw, ok := fields["providers"]; ok {
		var patches map[string]json.RawMessage
		if err := json.Unmarshal(raw, &patches); err != nil || patches == nil {
			return fmt.Errorf("providers must be an object")
		}
		if c.Providers == nil {
			c.Providers = map[string]Provider{}
		}
		for id, rawPatch := range patches {
			if id == LegacyProviderID {
				// The compatibility provider is changed only through legacy fields.
				// Its public projection may be round-tripped by existing clients.
				continue
			}
			var pfields map[string]json.RawMessage
			if err := json.Unmarshal(rawPatch, &pfields); err != nil || pfields == nil {
				return fmt.Errorf("provider patch must be an object")
			}
			old := c.Providers[id]
			explicitProviderKey := credentialExplicit(pfields)
			if err := preserveCredential(pfields, old.APIKey); err != nil {
				return err
			}
			if rawURL, ok := pfields["base_url"]; ok && old.APIKey != "" {
				var next string
				if err := json.Unmarshal(rawURL, &next); err != nil {
					return fmt.Errorf("invalid provider base URL")
				}
				if old.BaseURL != next && !explicitProviderKey {
					return fmt.Errorf("changing a provider address requires a new key or clear_api_key=true")
				}
			}
			delete(pfields, "has_api_key")
			data, _ := json.Marshal(pfields)
			if err := json.Unmarshal(data, &old); err != nil {
				return fmt.Errorf("invalid provider configuration")
			}
			c.Providers[id] = old
		}
		delete(fields, "providers")
	}
	delete(fields, "has_api_key")
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return fmt.Errorf("invalid API configuration")
	}
	return nil
}

func preserveCredential(fields map[string]json.RawMessage, existing string) error {
	clear := false
	if raw, ok := fields["clear_api_key"]; ok {
		if err := json.Unmarshal(raw, &clear); err != nil {
			return fmt.Errorf("clear_api_key must be boolean")
		}
	}
	key := ""
	if raw, ok := fields["api_key"]; ok {
		if err := json.Unmarshal(raw, &key); err != nil {
			return fmt.Errorf("api_key must be text")
		}
	}
	if clear {
		fields["api_key"] = json.RawMessage(`""`)
	} else if strings.TrimSpace(key) == "" {
		encoded, _ := json.Marshal(existing)
		fields["api_key"] = encoded
	}
	return nil
}

func credentialExplicit(fields map[string]json.RawMessage) bool {
	var clear bool
	_ = json.Unmarshal(fields["clear_api_key"], &clear)
	if clear {
		return true
	}
	// Called before normalization so retained keys do not count as new input.
	var key string
	_ = json.Unmarshal(fields["api_key"], &key)
	return strings.TrimSpace(key) != ""
}

// PublicAPIConfig deliberately excludes all free-form maps and credential-bearing
// connection fields. They remain on disk and survive partial saves.
func PublicAPIConfig(c *APIConfig) map[string]any {
	if c == nil {
		return map[string]any{}
	}
	view := map[string]any{
		"revision": APIRevision(c),
		"base_url": c.BaseURL, "url_strict": c.URLStrict, "model": c.Model,
		"has_api_key": c.APIKey != "", "max_tokens": c.MaxTokens,
		"http_timeout_seconds":  c.HTTPTimeoutSeconds,
		"context_budget_tokens": c.ContextBudgetTokens,
		"default_provider":      c.DefaultProvider,
		"providers":             (ProviderRegistry{Providers: c.Providers}).PublicView(),
		"model_tiers":           c.ModelTiers,
	}
	if c.Novel != nil {
		view["novel"] = map[string]any{"model": c.Novel.Model, "max_tokens": c.Novel.MaxTokens, "http_timeout_seconds": c.Novel.HTTPTimeoutSeconds}
	}
	return view
}
