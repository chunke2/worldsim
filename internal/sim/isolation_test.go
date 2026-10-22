package sim

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"worldsim/internal/engine"
)

func TestIndependentMemoryRequests(t *testing.T) {
	ms := NewMemoryStore(filepath.Join(t.TempDir(), "mem.json"))
	for i := 0; i < 6; i++ {
		ms.AddDay("A", "A-secret", "observation", 0.5, i)
		ms.AddDay("B", "B-secret", "observation", 0.5, i)
	}
	calls := 0
	c := &LLMClient{WorldRefs: "author-secret", Mock: func(system, user string) string {
		calls++
		all := system + user
		if strings.Contains(all, "author-secret") || strings.Contains(all, "A-secret") && strings.Contains(all, "B-secret") {
			t.Fatal("private context crossed boundary")
		}
		return `{}`
	}}
	ms.BatchReflect(context.Background(), c, []string{"A", "B"})
	if calls != 2 {
		t.Fatalf("wanted per-character calls, got %d", calls)
	}
}
func TestObservationAndDialogueAudience(t *testing.T) {
	e := engine.NewStateEngine(engine.Rules{}, "")
	e.State().Entities = map[string]engine.Entity{"A": {Location: "room"}, "B": {Location: "room"}, "C": {Location: "elsewhere"}}
	s := NewSimulator(e, t.TempDir())
	s.heroName = "A"
	ev := EventCard{ID: "e", Location: "elsewhere", Severity: 1, Frame: "secret", Observations: map[string]string{"A": "remote secret"}}
	if len(s.buildObservation([]EventCard{ev}).Observations) != 0 {
		t.Fatal("remote severity leaked")
	}
	ev.Location = "room"
	ev.Observations = map[string]string{"B": "B-only"}
	if len(s.buildObservation([]EventCard{ev}).Observations) != 0 {
		t.Fatal("another character observation leaked")
	}
	s.recordMemories(&DayResult{Day: 1, Dialogue: []DialogueTurn{{Speaker: "B", Speech: "B-secret", Audience: []string{"A", "B"}, SourceEvent: "event:1:said-b"}, {Speaker: "C", Speech: "C-secret", Audience: []string{"C"}, SourceEvent: "event:2:said-c"}}})
	foundSource := false
	for _, m := range s.mem.Recent("A", 20) {
		if strings.Contains(m.Content, "C-secret") {
			t.Fatal("other scene leaked")
		}
		if strings.Contains(m.Content, "B-secret") && len(m.SourceEvents) == 1 && m.SourceEvents[0] == "event:1:said-b" {
			foundSource = true
		}
	}
	if !foundSource {
		t.Fatal("heard dialogue did not retain its immutable source")
	}
}
