package sim

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
	"worldsim/internal/engine"
)

func TestRunDayIndependentDoesNotInvokeLegacyAgents(t *testing.T) {
	dir := t.TempDir()
	e := engine.NewStateEngine(engine.Rules{}, filepath.Join(dir, "event_log.jsonl"))
	e.State().Revision = 1
	e.State().Day = 1
	e.State().WorldLevel.Protagonist = "A"
	for _, id := range []string{"A", "B", "C", "D", "E"} {
		e.State().Entities[id] = engine.Entity{Alive: true, Status: "active", Location: "room", Money: 10, Extra: map[string]any{"persona": id + "-PRIVATE"}}
	}
	r, _ := characteragent.NewRegistry(dir)
	doc, err := r.Reconcile(e.State())
	if err != nil {
		t.Fatal(err)
	}
	providers := config.ProviderRegistry{Providers: map[string]config.Provider{"p": {Name: "p", Protocol: "openai_compatible", BaseURL: "http://unused.local"}}}
	settings := config.AgentRuntimeConfig{SchemaVersion: 1, ExecutionMode: "independent", Tiers: map[config.AgentImportance]config.TierPolicy{}}
	for _, tier := range []config.AgentImportance{config.ImportanceCore, config.ImportanceSupporting, config.ImportanceBackground} {
		settings.Tiers[tier] = config.TierPolicy{ModelRef: config.ModelRef{ProviderID: "p", Model: "mock"}}
	}
	settings, _, _ = r.ConfigView(settings)
	if _, err := r.SaveConfig(settings, doc.Revision, providers); err != nil {
		t.Fatal(err)
	}
	s := NewSimulator(e, dir)
	legacyCalls := 0
	s.EnableLLM(&LLMClient{Mock: func(_, _ string) string { legacyCalls++; return "{}" }})
	var calls atomic.Int32
	s.SetCharacterClient(characteragent.CompletionFunc(func(_ context.Context, request characteragent.ModelRequest) (characteragent.ModelReply, error) {
		calls.Add(1)
		var local struct {
			Observations []struct {
				ID string `json:"id"`
			} `json:"observations"`
		}
		if json.Unmarshal([]byte(request.Messages[1].Content), &local) != nil {
			t.Error("invalid context")
		}
		body, _ := json.Marshal(map[string]any{"actor_id": request.Owner.CharacterID, "turn_id": request.TurnID, "updates": []any{}, "intent": characteragent.CharacterIntent{ActorID: request.Owner.CharacterID, TurnID: request.TurnID, Action: "wait", Sources: []string{local.Observations[0].ID}}})
		return characteragent.ModelReply{Content: string(body), FinishReason: "stop"}, nil
	}), providers)
	for turn := 0; turn < 3; turn++ {
		result, err := s.RunDay(context.Background())
		if err != nil || result.Mode != "independent" || result.AgentTurn == nil || len(result.AgentTurn.Snapshot.Actors) != 5 {
			t.Fatal("independent RunDay failed", err)
		}
		if turn == 2 && !result.Paused {
			t.Fatal("repeated waiting did not pause")
		}
	}
	if legacyCalls != 0 || calls.Load() != 15 {
		t.Fatal("legacy agents ran or wrong actor call count")
	}
	if _, err := s.RunDay(context.Background()); err == nil || calls.Load() != 15 {
		t.Fatal("stagnation still consumed model calls")
	}
}
