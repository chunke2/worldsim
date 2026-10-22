package narrative

import (
	"os"
	"path/filepath"
	"testing"
	"worldsim/internal/engine"
)

func TestStagnationRequiresThreeUnevidencedCycles(t *testing.T) {
	entries := []engine.LogEntry{
		{Revision: 1, Day: 1, CommandID: "observed"},
		{Revision: 2, Day: 1, CommandID: "rel", Reason: "earned trust", SourceEvents: []string{"event:1:observed"}, Changes: []engine.Change{{Path: "entities.A.relationship.B", Op: "set", Value: 0.2}}},
	}
	a := Assess(entries, 4)
	if !a.Stalled || a.Days[0].Status != "evidenced_change" || a.Days[3].Status != "stagnation_risk" || len(a.Pressures) != 1 {
		t.Fatalf("unexpected assessment: %#v", a)
	}
	plan := State{Version: 1, ReviewedRevision: 1, ReviewedDay: 4, ReviewReason: "author verified a quiet transition"}
	a = AssessAfterReview(entries, 4, plan)
	if a.Stalled || len(a.Days) != 0 {
		t.Fatalf("review boundary not applied: %#v", a)
	}
}

func TestAssessmentRejectsUnknownAndSelfEvidence(t *testing.T) {
	entries := []engine.LogEntry{
		{Revision: 1, Day: 1, CommandID: "unknown", Reason: "claim", SourceEvents: []string{"event:999:none"}, Changes: []engine.Change{{Path: "entities.A.money", Op: "add", Value: -1}}},
		{Revision: 2, Day: 2, CommandID: "self", Reason: "claim", SourceEvents: []string{"event:2:self"}, Changes: []engine.Change{{Path: "entities.A.health", Op: "add", Value: -1}}},
	}
	a := Assess(entries, 2)
	for _, day := range a.Days {
		if len(day.Evidence) != 0 {
			t.Fatalf("invalid source counted as evidence: %#v", a)
		}
	}
}

func TestNarrativeClaimsNeedEvidenceAndPublishedReaderState(t *testing.T) {
	dir := t.TempDir()
	world := &engine.WorldState{Revision: 3, Day: 2, Entities: map[string]engine.Entity{"A": {}}}
	bad := State{Version: 1, Threads: []Thread{{ID: "s", Kind: "secret", Description: "door code", Status: "resolved", Characters: []string{"A"}}}}
	if err := SavePlan(dir, bad, world, map[string]bool{}, map[int]bool{}); err == nil {
		t.Fatal("unsupported resolution accepted")
	}
	bad.Threads[0].Status = "planned"
	bad.Threads[0].ReaderChapters = []int{1}
	if err := SavePlan(dir, bad, world, map[string]bool{}, map[int]bool{}); err == nil {
		t.Fatal("unpublished reader knowledge accepted")
	}
	bad.Threads[0].ReaderChapters = nil
	if err := SavePlan(dir, bad, world, map[string]bool{}, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "narrative.json")); err != nil {
		t.Fatal(err)
	}
}
