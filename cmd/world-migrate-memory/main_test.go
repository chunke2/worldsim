package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"worldsim/internal/characteragent"
	"worldsim/internal/engine"
)

func TestCommandPreviewAndExplicitApply(t *testing.T) {
	dir := t.TempDir()
	r, _ := characteragent.NewRegistry(dir)
	if _, err := r.Reconcile(&engine.WorldState{Revision: 1, Entities: map[string]engine.Entity{"甲": {Alive: true, Status: "active"}}}); err != nil {
		t.Fatal(err)
	}
	source := []byte(`{"memories":{"甲":[{"actor":"甲","kind":"event","content":"PRIVATE-CANARY"}]}}`)
	if err := os.WriteFile(filepath.Join(dir, "agents_memory.json"), source, 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"--world", dir}, &out, &errOut); code != 0 {
		t.Fatalf("preview failed: %d %s", code, errOut.String())
	}
	if bytes.Contains(out.Bytes(), []byte("PRIVATE-CANARY")) {
		t.Fatal("report leaked memory text")
	}
	var preview struct {
		Mode string                             `json:"mode"`
		Plan characteragent.LegacyMigrationPlan `json:"plan"`
	}
	if json.Unmarshal(out.Bytes(), &preview) != nil || preview.Mode != "preview" || preview.Plan.ID == "" {
		t.Fatal("invalid report")
	}
	if _, err := os.Stat(filepath.Join(dir, "memory_migrations")); !os.IsNotExist(err) {
		t.Fatal("default command wrote migration files")
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"--world", dir, "--apply"}, &out, &errOut); code != 2 {
		t.Fatal("apply accepted without preview plan")
	}
	for range 2 {
		out.Reset()
		errOut.Reset()
		if code := run([]string{"--world", dir, "--apply", "--plan", preview.Plan.ID}, &out, &errOut); code != 0 {
			t.Fatalf("explicit application failed: %s", errOut.String())
		}
		if bytes.Contains(out.Bytes(), []byte("PRIVATE-CANARY")) {
			t.Fatal("apply report leaked memory text")
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "agents_memory.json"))
	if err != nil || !bytes.Equal(source, got) {
		t.Fatal("command changed source")
	}
	s, _ := characteragent.NewCognitionStore(dir, "甲")
	if state, err := s.Read(); err != nil || state.Revision != 0 {
		t.Fatal("command promoted unverified content")
	}
	if bundle, err := s.ReadLegacyQuarantine(preview.Plan.ID); err != nil || len(bundle.Items) != 1 {
		t.Fatal("quarantine unavailable", err)
	}
}

func TestCommandInvalidInputsDoNotWrite(t *testing.T) {
	for _, args := range [][]string{nil, {"--apply"}, {"--unknown"}, {"--world", t.TempDir()}, {"--world", t.TempDir(), "unexpected"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code == 0 {
			t.Fatal("invalid arguments accepted")
		}
		if out.Len() != 0 {
			t.Fatal("invalid command emitted successful report")
		}
	}
}
