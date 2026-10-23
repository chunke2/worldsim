package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyProviderMigrationDoesNotRewriteSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.json")
	source := []byte(`{"base_url":"https://old.example/v1","api_key":"KEY-CANARY","model":"old","models":["cheap","strong"],"max_tokens":9000,"extra_body":{"thinking":false}}`)
	if err := os.WriteFile(path, source, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAPIConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(source, after) {
		t.Fatal("load rewrote user config")
	}
	if cfg.DefaultProvider != LegacyProviderID || cfg.Providers[LegacyProviderID].APIKey != "KEY-CANARY" {
		t.Fatal("legacy migration lost provider/key")
	}
	if got := cfg.Providers[LegacyProviderID].Models; len(got) != 2 || got[0] != "cheap" || got[1] != "strong" {
		t.Fatal("legacy migration lost selectable model catalog")
	}
	next, err := MergeAPIConfig(cfg, []byte(`{"model":"new","api_key":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.APIKey != "KEY-CANARY" || next.MaxTokens != 9000 || next.ExtraBody["thinking"] != false {
		t.Fatal("partial update lost fields")
	}
	if err := SaveAPIConfig(next, path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadAPIConfig(path)
	if err != nil || reloaded.Providers[LegacyProviderID].APIKey != next.APIKey {
		t.Fatal("migration round trip failed")
	}
}

func TestCompatibilityModelCatalogRejectsAmbiguousEntries(t *testing.T) {
	base := &APIConfig{BaseURL: "https://api.example", Model: "cheap"}
	for _, patch := range []string{
		`{"models":["cheap","cheap"]}`,
		`{"models":[" cheap"]}`,
		`{"models":[""]}`,
	} {
		if _, err := MergeAPIConfig(base, []byte(patch)); err == nil {
			t.Fatalf("accepted invalid model catalog: %s", patch)
		}
	}
}

func TestProviderPartialPatchPreservesOtherProvidersAndSecrets(t *testing.T) {
	r, _ := runtimeFixture()
	base := &APIConfig{Providers: r.Providers, DefaultProvider: "primary", ExtraBody: map[string]any{"custom": true}, Novel: &GenerationConfig{MaxTokens: 18000, ExtraBody: map[string]any{"writing": true}}}
	p := base.Providers["primary"]
	p.ExtraBody = map[string]any{"advanced": true}
	base.Providers["primary"] = p
	next, err := MergeAPIConfig(base, []byte(`{"providers":{"primary":{"name":"Updated","api_key":""}},"novel":{"model":"writer"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.Providers["primary"].APIKey != "PRIVATE-KEY-CANARY" || len(next.Providers) != 2 || next.Providers["primary"].ExtraBody["advanced"] != true || next.Novel.MaxTokens != 18000 || next.Novel.ExtraBody["writing"] != true {
		t.Fatal("nested patch lost advanced fields")
	}
	if base.Providers["primary"].Name != "Primary" || base.Novel.Model != "" {
		t.Fatal("merge mutated base")
	}
	b, _ := json.Marshal(PublicAPIConfig(next))
	if strings.Contains(string(b), "CANARY") || strings.Contains(string(b), "advanced") {
		t.Fatal("public view leaked private fields")
	}
	// Posting the public projection must preserve all omitted private fields.
	roundTrip, err := MergeAPIConfig(next, b)
	if err != nil || roundTrip.Providers["primary"].APIKey != next.Providers["primary"].APIKey || roundTrip.Novel.ExtraBody["writing"] != true {
		t.Fatal("public round trip erased secrets")
	}
}

func TestCredentialRetentionReplacementAndEndpointChange(t *testing.T) {
	base := &APIConfig{BaseURL: "https://old.example/v1", APIKey: "OLD-KEY"}
	for _, patch := range []string{`{}`, `{"api_key":""}`, `{"api_key":null}`} {
		got, err := MergeAPIConfig(base, []byte(patch))
		if err != nil || got.APIKey != "OLD-KEY" {
			t.Fatal("blank key cleared saved credential")
		}
	}
	for _, patch := range []string{`{"base_url":"https://other.example/v1"}`, `{"base_url":"https://other.example/v1","api_key":""}`} {
		if _, err := MergeAPIConfig(base, []byte(patch)); err == nil {
			t.Fatal("retained key could be sent to changed endpoint")
		}
	}
	got, err := MergeAPIConfig(base, []byte(`{"clear_api_key":true}`))
	if err != nil || got.APIKey != "" || got.Providers[LegacyProviderID].APIKey != "" {
		t.Fatal("explicit key clearing failed")
	}
	got, err = MergeAPIConfig(base, []byte(`{"base_url":"https://other.example/v1","api_key":"NEW-KEY"}`))
	if err != nil || got.APIKey != "NEW-KEY" {
		t.Fatal("explicit replacement failed")
	}
	r, _ := runtimeFixture()
	base = &APIConfig{Providers: r.Providers}
	if _, err := MergeAPIConfig(base, []byte(`{"providers":{"primary":{"base_url":"https://other.example/v1","api_key":""}}}`)); err == nil {
		t.Fatal("provider endpoint change reused stored key")
	}
}
