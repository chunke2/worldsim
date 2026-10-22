package sim

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"worldsim/internal/characteragent"
	"worldsim/internal/engine"
)

func TestAgentRegistrationPrecedesModelCalls(t *testing.T) {
	dir := t.TempDir()
	e := engine.NewStateEngine(engine.Rules{}, "")
	e.State().Revision, e.State().Day = 1, 3
	e.State().WorldLevel.Protagonist = "A"
	e.State().Entities = map[string]engine.Entity{"A": {Alive: true, Status: "active"}}
	s := NewSimulator(e, dir)
	calls := 0
	s.EnableLLM(&LLMClient{Mock: func(_, _ string) string { calls++; return "{}" }})
	// Three unverified cycles stop simulation, but registration must already exist.
	if _, err := s.RunDay(context.Background()); err == nil {
		t.Fatal("expected narrative guard")
	}
	r, _ := characteragent.NewRegistry(dir)
	d, exists, err := r.Read()
	if err != nil || !exists || len(d.Agents) != 1 || calls != 0 {
		t.Fatal("registration failed or invoked model")
	}
}

func TestSnapshotRewindReconcilesAgentLifecycle(t *testing.T) {
	dir := t.TempDir()
	e := engine.NewStateEngine(engine.Rules{}, "")
	e.State().Revision, e.State().Day = 1, 1
	e.State().WorldLevel.Protagonist = "A"
	e.State().Entities = map[string]engine.Entity{"A": {Alive: true, Status: "active"}}
	s := NewSimulator(e, dir)
	if err := e.Save(filepath.Join(dir, "world_state.json")); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileCharacterAgents(); err != nil {
		t.Fatal(err)
	}
	r, _ := characteragent.NewRegistry(dir)
	before, _, _ := r.Read()
	id := before.Agents["A"].AgentID
	privatePath := filepath.Join(dir, "agents", id, "memory-canary")
	if err := os.WriteFile(privatePath, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveSnapshot("before death"); err != nil {
		t.Fatal(err)
	}
	e.State().Revision, e.State().Day = 2, 2
	e.State().Entities["A"] = engine.Entity{Alive: false, Status: "dead"}
	if err := s.ReconcileCharacterAgents(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RewindTo(1); err != nil {
		t.Fatal(err)
	}
	after, _, err := r.Read()
	if err != nil || after.WorldRevision != 1 || after.Agents["A"].AgentID != id || after.Agents["A"].State != "active" {
		t.Fatal("rewind lost identity or active state")
	}
	b, _ := os.ReadFile(privatePath)
	if string(b) != "preserve" {
		t.Fatal("registry rewind changed private files")
	}
}
