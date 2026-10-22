package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func runtimeFixture() (ProviderRegistry, AgentRuntimeConfig) {
	r := ProviderRegistry{DefaultProvider: "primary", Providers: map[string]Provider{
		"primary": {Name: "Primary", Protocol: "openai_compatible", BaseURL: "https://primary.example/v1", APIKey: "PRIVATE-KEY-CANARY", Models: []string{"strong", "standard"}},
		"other":   {Name: "Other", Protocol: "openai_compatible", BaseURL: "http://localhost:11434/v1"},
	}}
	c := AgentRuntimeConfig{SchemaVersion: 1, Tiers: map[AgentImportance]TierPolicy{
		ImportanceCore:       {ModelRef: ModelRef{"primary", "strong"}},
		ImportanceSupporting: {ModelRef: ModelRef{"primary", "standard"}},
		ImportanceBackground: {ModelRef: ModelRef{"other", "manually-entered"}},
	}, Characters: map[string]CharacterAgentConfig{
		"沈砚": {Importance: ImportanceCore},
		"罗绮": {Importance: ImportanceCore, ModelOverride: &ModelRef{"other", "different-model"}},
	}}
	return r, c
}

func TestAgentModelResolution(t *testing.T) {
	r, c := runtimeFixture()
	for _, tc := range []struct{ character, provider, model string }{
		{"沈砚", "primary", "strong"}, {"罗绮", "other", "different-model"},
	} {
		ref, err := c.ResolveModel(tc.character, r)
		if err != nil || ref.ProviderID != tc.provider || ref.Model != tc.model {
			t.Fatalf("wrong route for %s: %#v, %v", tc.character, ref, err)
		}
	}
	if _, err := c.ResolveModel("unregistered", r); err == nil {
		t.Fatal("unregistered character silently got a model")
	}
	delete(r.Providers, "other")
	if _, err := c.ResolveModel("罗绮", r); err == nil {
		t.Fatal("missing provider silently fell back")
	}
}

func TestProviderPublicViewExcludesPrivateFields(t *testing.T) {
	r, c := runtimeFixture()
	p := r.Providers["primary"]
	p.ExtraBody = map[string]any{"authorization": "BODY-SECRET-CANARY"}
	r.Providers["primary"] = p
	view := r.PublicView()
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PRIVATE-KEY-CANARY", "BODY-SECRET-CANARY", "extra_body", `"api_key"`} {
		if strings.Contains(string(b), secret) {
			t.Fatal("public view exposed private configuration")
		}
	}
	if !view["primary"].HasAPIKey {
		t.Fatal("public view lost key presence")
	}
	view["primary"].Models[0] = "changed"
	if r.Providers["primary"].Models[0] != "strong" {
		t.Fatal("public model list aliases private config")
	}
	worldJSON, _ := json.Marshal(c)
	if strings.Contains(string(worldJSON), "PRIVATE-KEY-CANARY") {
		t.Fatal("world config contains a provider credential")
	}
}

func TestAgentConfigRejectsCredentialFieldsAndInvalidLimits(t *testing.T) {
	r, c := runtimeFixture()
	b, _ := json.Marshal(c)
	if _, err := DecodeAgentRuntimeConfig(b, r); err != nil {
		t.Fatal(err)
	}
	for _, input := range [][]byte{
		[]byte(strings.Replace(string(b), `"schema_version":1`, `"schema_version":1,"api_key":"secret"`, 1)),
		[]byte(strings.Replace(string(b), `"provider_id":"primary"`, `"api_key":"secret","provider_id":"primary"`, 1)),
		append(append([]byte{}, b...), []byte(` {}`)...),
	} {
		if _, err := DecodeAgentRuntimeConfig(input, r); err == nil {
			t.Fatal("invalid/credential-bearing world config accepted")
		}
	}
	p := c.Tiers[ImportanceCore]
	p.MaxTokens = -1
	c.Tiers[ImportanceCore] = p
	if err := c.Validate(r); err == nil {
		t.Fatal("negative token limit accepted")
	}
	r, c = runtimeFixture()
	c.Characters["沈砚"] = CharacterAgentConfig{Importance: "unknown"}
	if err := c.Validate(r); err == nil {
		t.Fatal("unknown importance accepted")
	}
}

func TestProviderRejectsCredentialsInBaseURL(t *testing.T) {
	for _, endpoint := range []string{"https://user:secret@example.com/v1", "https://example.com/v1?key=secret", "ftp://example.com", "not a URL"} {
		r, _ := runtimeFixture()
		p := r.Providers["primary"]
		p.BaseURL = endpoint
		r.Providers["primary"] = p
		err := r.Validate()
		if err == nil {
			t.Fatal("unsafe/invalid endpoint accepted")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("validation error exposed embedded secret")
		}
	}
}
