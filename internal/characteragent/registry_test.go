package characteragent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"worldsim/internal/config"
	"worldsim/internal/engine"
)

func fiveCharacters() *engine.WorldState {
	return &engine.WorldState{Revision: 1, Day: 1,
		WorldLevel: engine.WorldLevel{Protagonist: "沈砚"},
		Entities: map[string]engine.Entity{
			"沈砚": {Alive: true, Status: "active"},
			"罗绮": {Alive: true, Status: "active", Extra: map[string]any{"role": "rival"}},
			"顾闻": {Alive: true, Status: "active", Extra: map[string]any{"role": "important_npc"}},
			"叶晴": {Alive: true, Status: "active", Extra: map[string]any{"role": "minor_npc"}},
			"程野": {Alive: true, Status: "active", Extra: map[string]any{"tier": "walkon"}},
		}}
}

func TestRegistryFiveCharactersAndPureRead(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(dir)
	if _, exists, err := r.Read(); err != nil || exists {
		t.Fatal("absent registry should be readable without creation")
	}
	if _, err := os.Stat(filepath.Join(dir, RegistryFile)); !os.IsNotExist(err) {
		t.Fatal("read created registry")
	}
	d, err := r.Reconcile(fiveCharacters())
	if err != nil || len(d.Agents) != 5 {
		t.Fatalf("five registrations failed: %v", err)
	}
	seen := map[string]bool{}
	for _, a := range d.Agents {
		if seen[a.AgentID] || a.State != "active" {
			t.Fatal("duplicate or inactive identity")
		}
		seen[a.AgentID] = true
		if _, err := os.Stat(filepath.Join(dir, "agents", a.AgentID)); err != nil {
			t.Fatal(err)
		}
	}
	if d.Agents["沈砚"].Importance != config.ImportanceCore || d.Agents["罗绮"].Importance != config.ImportanceCore || d.Agents["顾闻"].Importance != config.ImportanceSupporting || d.Agents["程野"].Importance != config.ImportanceBackground {
		t.Fatal("wrong initial importance")
	}
	before, _ := os.ReadFile(filepath.Join(dir, RegistryFile))
	reopened, _ := NewRegistry(dir)
	got, err := reopened.Reconcile(fiveCharacters())
	after, _ := os.ReadFile(filepath.Join(dir, RegistryFile))
	if err != nil || !bytes.Equal(before, after) || got.WorldID != d.WorldID || got.Revision != d.Revision {
		t.Fatal("reconcile/restart is not idempotent")
	}
}

func TestLifecyclePreservesIdentityImportanceAndPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(dir)
	w := fiveCharacters()
	d, err := r.Reconcile(w)
	if err != nil {
		t.Fatal(err)
	}
	a := d.Agents["沈砚"]
	a.Importance = config.ImportanceBackground // simulate an explicit persisted user choice
	d.Agents["沈砚"] = a
	b, _ := json.Marshal(d)
	if err := os.WriteFile(filepath.Join(dir, RegistryFile), b, 0600); err != nil {
		t.Fatal(err)
	}
	memory := filepath.Join(dir, "agents", a.AgentID, "working_memory.jsonl")
	if err := os.WriteFile(memory, []byte("PRIVATE-CANARY"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"dead", "departed", "mentioned", "dormant", "active"} {
		w.Revision++
		e := w.Entities["沈砚"]
		e.Status, e.Alive = status, status != "dead"
		w.Entities["沈砚"] = e
		d, err = r.Reconcile(w)
		if err != nil {
			t.Fatal(err)
		}
		got := d.Agents["沈砚"]
		if got.AgentID != a.AgentID || got.Importance != config.ImportanceBackground || (got.State == "active") != (status == "active") {
			t.Fatal("lifecycle overwrote identity, user setting or state")
		}
	}
	delete(w.Entities, "沈砚")
	w.Revision++
	d, err = r.Reconcile(w)
	if err != nil || d.Agents["沈砚"].DormantReason != "absent_from_world" {
		t.Fatal("removed entity lost registry entry")
	}
	mem, _ := os.ReadFile(memory)
	if string(mem) != "PRIVATE-CANARY" {
		t.Fatal("lifecycle changed private memory")
	}
	index, _ := os.ReadFile(filepath.Join(dir, RegistryFile))
	if bytes.Contains(index, []byte("PRIVATE-CANARY")) {
		t.Fatal("private memory entered registry")
	}
}

func TestRegistrationRejectsInvalidWorldAndCorruptRegistry(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(dir)
	w := fiveCharacters()
	w.Entities[""] = engine.Entity{}
	if _, err := r.Reconcile(w); err == nil {
		t.Fatal("empty ID accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, RegistryFile)); !os.IsNotExist(err) {
		t.Fatal("invalid world wrote registry")
	}
	corrupt := []byte(`{"schema_version":999}`)
	if err := os.WriteFile(filepath.Join(dir, RegistryFile), corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(fiveCharacters()); err == nil {
		t.Fatal("corruption overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(dir, RegistryFile))
	if !bytes.Equal(corrupt, after) {
		t.Fatal("corrupt registry changed")
	}
}

func TestRegistrationFailureCanBeRetried(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(dir)
	if err := os.WriteFile(filepath.Join(dir, "agents"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(fiveCharacters()); err == nil {
		t.Fatal("partition failure ignored")
	}
	if _, err := os.Stat(filepath.Join(dir, RegistryFile)); !os.IsNotExist(err) {
		t.Fatal("published registry without partitions")
	}
	if err := os.Remove(filepath.Join(dir, "agents")); err != nil {
		t.Fatal(err)
	}
	if d, err := r.Reconcile(fiveCharacters()); err != nil || len(d.Agents) != 5 {
		t.Fatal("retry failed")
	}
}

func TestStaleSnapshotRequiresExplicitRewind(t *testing.T) {
	r, _ := NewRegistry(t.TempDir())
	w := fiveCharacters()
	d, _ := r.Reconcile(w)
	id := d.Agents["沈砚"].AgentID
	w.Revision = 3
	e := w.Entities["沈砚"]
	e.Status = "dead"
	e.Alive = false
	w.Entities["沈砚"] = e
	if _, err := r.Reconcile(w); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(fiveCharacters()); err == nil {
		t.Fatal("stale snapshot revived dead character")
	}
	d, err := r.ReconcileAfterRewind(fiveCharacters())
	if err != nil || d.Agents["沈砚"].AgentID != id || d.Agents["沈砚"].State != "active" {
		t.Fatal("explicit rewind did not preserve identity")
	}
}

func TestConcurrentReconcileUsesOneIdentitySet(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := NewRegistry(dir)
			if _, err := r.Reconcile(fiveCharacters()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r, _ := NewRegistry(dir)
	d, _, err := r.Read()
	if err != nil || d.Revision != 1 || len(d.Agents) != 5 {
		t.Fatal("concurrent reconcile created multiple versions")
	}
}
