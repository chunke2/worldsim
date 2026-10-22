package characteragent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"worldsim/internal/config"
)

func TestRuntimeConfigWriteFailureDoesNotPublishRevision(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(dir)
	d, err := r.Reconcile(fiveCharacters())
	if err != nil {
		t.Fatal(err)
	}
	providers := config.ProviderRegistry{Providers: map[string]config.Provider{"p": {Name: "P", Protocol: "openai_compatible", BaseURL: "http://localhost/v1"}}}
	c := config.AgentRuntimeConfig{SchemaVersion: 1, Tiers: map[config.AgentImportance]config.TierPolicy{}}
	for _, tier := range []config.AgentImportance{config.ImportanceCore, config.ImportanceSupporting, config.ImportanceBackground} {
		c.Tiers[tier] = config.TierPolicy{ModelRef: config.ModelRef{ProviderID: "p", Model: "m"}}
	}
	c, _, err = r.ConfigView(c)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, RuntimeConfigFile+".tmp")
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveConfig(c, d.Revision, providers); err == nil {
		t.Fatal("failed write reported success")
	}
	if _, err := os.Stat(filepath.Join(dir, RuntimeConfigFile)); !os.IsNotExist(err) {
		t.Fatal("failed save published config")
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	saved, err := r.SaveConfig(c, d.Revision, providers)
	if err != nil || saved.Revision != 1 {
		t.Fatal("retry did not publish first version")
	}
	before, _ := os.ReadFile(filepath.Join(dir, RuntimeConfigFile))
	if _, err := r.SaveConfig(c, d.Revision, providers); !errors.Is(err, config.ErrRevisionConflict) {
		t.Fatal("stale config accepted")
	}
	after, _ := os.ReadFile(filepath.Join(dir, RuntimeConfigFile))
	if !bytes.Equal(before, after) {
		t.Fatal("conflict changed saved bytes")
	}
}
