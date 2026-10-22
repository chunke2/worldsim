package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotDoesNotAliasLiveState(t *testing.T) {
	e := NewStateEngine(Rules{}, "")
	e.State().Entities["A"] = Entity{Extra: map[string]any{"private": "original"}}
	snapshot, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	a := snapshot.Entities["A"]
	a.Extra["private"] = "changed"
	delete(snapshot.Entities, "A")
	if e.State().Entities["A"].Extra["private"] != "original" {
		t.Fatal("snapshot consumer mutated authoritative state")
	}
}

func TestIdentityNeverUsesMapOrder(t *testing.T) {
	s := &WorldState{Entities: map[string]Entity{"A": {}, "B": {}}, WorldLevel: WorldLevel{Protagonist: "B", NarrativePOV: "A"}}
	for i := 0; i < 100; i++ {
		h, p, e := ResolveIdentity(s)
		if e != nil || h != "B" || p != "A" {
			t.Fatal(h, p, e)
		}
	}
	s.WorldLevel = WorldLevel{}
	if _, _, err := ResolveIdentity(s); err == nil {
		t.Fatal("ambiguous identity silently chosen")
	}
}

func TestFailedProposalDoesNotMutateState(t *testing.T) {
	e := NewStateEngine(Rules{}, filepath.Join(t.TempDir(), "events.jsonl"))
	before, _ := json.Marshal(e.State())
	p := &Proposal{CommandID: "bad", Changes: []Change{{Path: "entities.A.money", Op: "set", Value: 4.0}, {Path: "entities.A.invalid_field", Op: "set", Value: 1}}}
	if err := e.Submit(context.Background(), p); err == nil {
		t.Fatal("invalid proposal accepted")
	}
	after, _ := json.Marshal(e.State())
	if string(before) != string(after) {
		t.Fatal("partial mutation")
	}
	p.Changes = []Change{{Path: "entities..money", Op: "set", Value: 4}}
	if err := e.Submit(context.Background(), p); err == nil {
		t.Fatal("empty entity accepted")
	}
}

func TestJournalFailureAndReplay(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "directory")
	os.Mkdir(blocked, 0755)
	e := NewStateEngine(Rules{}, blocked)
	p := &Proposal{CommandID: "one", Changes: []Change{{Path: "entities.A.money", Op: "set", Value: 4.0}}}
	if err := e.Submit(context.Background(), p); err == nil {
		t.Fatal("journal directory accepted")
	}
	if e.State().Revision != 0 || len(e.State().Entities) > 0 {
		t.Fatal("state changed without journal")
	}
	path := filepath.Join(dir, "events.jsonl")
	e = NewStateEngine(Rules{}, path)
	if err := e.Submit(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	initial := &WorldState{Entities: map[string]Entity{}}
	got, err := Replay(path, 1, initial)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(e.State())
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("replay differs\n%s\n%s", a, b)
	}
	if len(initial.Entities) != 0 {
		t.Fatal("replay modified caller snapshot")
	}
	os.WriteFile(path, []byte("broken\n"), 0644)
	if _, err := Replay(path, 1, nil); err == nil {
		t.Fatal("corruption hidden")
	}
}
