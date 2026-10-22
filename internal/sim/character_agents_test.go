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

func TestSyncCharacterObservationsUsesOnlyCommittedLocalEvidence(t *testing.T) {
	dir := t.TempDir()
	e := engine.NewStateEngine(engine.Rules{}, filepath.Join(dir, "event_log.jsonl"))
	e.State().Revision = 1
	e.State().WorldLevel.Protagonist = "A"
	e.State().Entities = map[string]engine.Entity{
		"A": {Alive: true, Status: "active", Location: "room", Extra: map[string]any{"secret": "GLOBAL-PRIVATE-CANARY"}},
		"B": {Alive: true, Status: "active", Location: "room"},
		"C": {Alive: true, Status: "active", Location: "corridor"},
	}
	s := NewSimulator(e, dir)
	calls := 0
	s.EnableLLM(&LLMClient{Mock: func(_, _ string) string { calls++; return "{}" }})
	if err := e.Submit(context.Background(), &engine.Proposal{CommandID: "whisper", ActorID: "world_agent", BaseRevision: 1, Type: "adjudication", Reason: "AUTHOR-PRIVATE-CANARY", Occurrences: []engine.Occurrence{{Kind: "utterance", Actor: "A", Content: "private speech", Location: "room", Audience: []string{"A", "B"}}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := e.Snapshot()
	report, err := s.SyncCharacterObservations()
	if err != nil || report.Added != 2 || calls != 0 {
		t.Fatal("bridge failed or invoked model", err)
	}
	for _, id := range []string{"A", "B", "C"} {
		store, err := characteragent.NewCognitionStore(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		state, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if id == "C" {
			want = 0
		}
		if len(state.Observations) != want || len(state.Records) != 0 {
			t.Fatal("bridge crossed audience or generated beliefs")
		}
		for _, o := range state.Observations {
			if o.Text != "private speech" || o.Speaker != "A" {
				t.Fatal("bridge used author text or world state")
			}
		}
	}
	after, _ := e.Snapshot()
	if before.Revision != after.Revision || before.Entities["A"].Extra["secret"] != after.Entities["A"].Extra["secret"] {
		t.Fatal("bridge changed world facts")
	}
	if report, err := s.SyncCharacterObservations(); err != nil || report.Added != 0 {
		t.Fatal("bridge duplicated observations")
	}
}
