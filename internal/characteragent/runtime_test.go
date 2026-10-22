package characteragent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"worldsim/internal/config"
	"worldsim/internal/engine"
)

func runtimeFixture(t *testing.T, client CompletionClient) (*IndependentRuntime, *engine.WorldState) {
	t.Helper()
	dir, e, _ := visibilityFixture(t)
	for id, entity := range e.State().Entities {
		entity.Money = 10
		entity.Extra = map[string]any{"persona": id + "-UNREVEALED-PRIVATE-CANARY"}
		e.State().Entities[id] = entity
	}
	e.State().WorldLevel.Locations = map[string]engine.Location{"白室": {Name: "白室", State: "正常"}, "走廊": {Name: "走廊", State: "正常"}}
	initial, _ := e.Snapshot()
	providers := config.ProviderRegistry{Providers: map[string]config.Provider{"mock": {Name: "mock", Protocol: "openai_compatible", BaseURL: "http://unused.local"}}}
	policies := config.AgentRuntimeConfig{SchemaVersion: 1, ExecutionMode: "independent", Tiers: map[config.AgentImportance]config.TierPolicy{}, Characters: map[string]config.CharacterAgentConfig{}}
	for _, tier := range []config.AgentImportance{config.ImportanceCore, config.ImportanceSupporting, config.ImportanceBackground} {
		policies.Tiers[tier] = config.TierPolicy{ModelRef: config.ModelRef{ProviderID: "mock", Model: "mock"}}
	}
	for id := range e.State().Entities {
		policies.Characters[id] = config.CharacterAgentConfig{Importance: config.ImportanceCore}
	}
	r, err := NewIndependentRuntime(dir, e, policies, providers, client)
	if err != nil {
		t.Fatal(err)
	}
	return r, initial
}

func replyWithIntent(r ModelRequest, intent CharacterIntent) ModelReply {
	b, _ := json.Marshal(map[string]any{"actor_id": r.Owner.CharacterID, "turn_id": r.TurnID, "updates": []any{}, "intent": intent})
	return ModelReply{Content: string(b), FinishReason: "stop"}
}

func TestIndependentFivePlayerGameTerminatesAndReplays(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	client := CompletionFunc(func(_ context.Context, r ModelRequest) (ModelReply, error) {
		for id := range fiveCharacters().Entities {
			got := strings.Contains(r.Messages[1].Content, id+"-UNREVEALED-PRIVATE-CANARY")
			if got != (id == r.Owner.CharacterID) {
				t.Error("game request included another persona")
			}
		}
		var local characterContext
		if json.Unmarshal([]byte(r.Messages[1].Content), &local) != nil {
			t.Fatal("bad context")
		}
		var visible struct {
			Hand      []string `json:"hand"`
			TableRank string   `json:"table_rank"`
			Pending   bool     `json:"pending"`
			NextActor string   `json:"next_actor"`
		}
		source := ""
		for i := len(local.Observations) - 1; i >= 0; i-- {
			text := local.Observations[i].Text
			if strings.HasPrefix(text, "你的牌局可见状态：") {
				if json.Unmarshal([]byte(strings.TrimPrefix(text, "你的牌局可见状态：")), &visible) != nil {
					t.Fatal("bad visible game state")
				}
				source = local.Observations[i].ID
				break
			}
		}
		if source == "" || visible.NextActor != r.Owner.CharacterID {
			t.Fatal("wrong activation or missing private hand")
		}
		intent := CharacterIntent{ActorID: r.Owner.CharacterID, TurnID: r.TurnID, Action: "challenge", Sources: []string{source}, Speech: "这是我自己的选择：" + r.Owner.CharacterID}
		if !visible.Pending {
			if len(visible.Hand) == 0 {
				t.Fatal("empty active hand")
			}
			intent.Action = "play"
			intent.Cards = []string{visible.Hand[0]}
			intent.Claim = visible.TableRank
		}
		mu.Lock()
		seen[r.Owner.CharacterID]++
		mu.Unlock()
		return replyWithIntent(r, intent), nil
	})
	r, initial := runtimeFixture(t, client)
	if err := r.InitializeCards(context.Background(), 42, 1); err != nil {
		t.Fatal(err)
	}
	winner := ""
	for i := 0; i < 20; i++ {
		result, err := r.RunTurn(context.Background())
		if err != nil {
			t.Fatalf("game turn %d: %v", i, err)
		}
		if len(result.Snapshot.Actors) > 1 {
			t.Fatal("strict game activated multiple actors")
		}
		if result.Winner != "" {
			winner = result.Winner
			break
		}
	}
	if winner == "" {
		t.Fatal("fixed game did not reach unique survivor")
	}
	state, _ := r.engine.Snapshot()
	alive := []string{}
	for id, e := range state.Entities {
		if e.Alive {
			alive = append(alive, id)
		}
	}
	if len(alive) != 1 || alive[0] != winner || len(seen) != 5 {
		t.Fatal("expected five independently acting players and one survivor", alive, seen)
	}
	prior := state.Revision
	result, err := r.RunTurn(context.Background())
	if err != nil || result.Winner != winner || r.engine.State().Revision != prior {
		t.Fatal("completed game generated more actions")
	}
	replayed, err := engine.Replay(filepath.Join(r.dir, "event_log.jsonl"), state.Revision, initial)
	if err != nil || !reflect.DeepEqual(state, replayed) {
		t.Fatal("deterministic world journal replay differs", err)
	}
}

func TestIndependentFreeSceneOnlyExecutesSubmittedActions(t *testing.T) {
	client := CompletionFunc(func(_ context.Context, r ModelRequest) (ModelReply, error) {
		var local characterContext
		_ = json.Unmarshal([]byte(r.Messages[1].Content), &local)
		intent := CharacterIntent{ActorID: r.Owner.CharacterID, TurnID: r.TurnID, Action: "wait", Sources: []string{local.Observations[0].ID}}
		if r.Owner.CharacterID == "沈砚" {
			intent.Action = "speak"
			intent.Speech = "我决定只告诉罗绮这句话"
			intent.Audience = []string{"罗绮"}
		}
		return replyWithIntent(r, intent), nil
	})
	r, _ := runtimeFixture(t, client)
	result, err := r.RunTurn(context.Background())
	if err != nil || len(result.Snapshot.Actors) != 5 {
		t.Fatal("free activation failed", err)
	}
	for id := range fiveCharacters().Entities {
		s, _ := NewCognitionStore(r.dir, id)
		state, _ := s.Read()
		text := string(mustJSON(t, state))
		heard := strings.Contains(text, "我决定只告诉罗绮这句话")
		if heard != (id == "沈砚" || id == "罗绮") {
			t.Fatal("speech was broadened or not remembered by speaker")
		}
	}
}

func TestIndependentFailedThinkingDoesNotAdvanceWorld(t *testing.T) {
	fail := true
	client := CompletionFunc(func(_ context.Context, r ModelRequest) (ModelReply, error) {
		if fail {
			return ModelReply{}, errors.New("model unavailable")
		}
		var local characterContext
		_ = json.Unmarshal([]byte(r.Messages[1].Content), &local)
		return replyWithIntent(r, CharacterIntent{ActorID: r.Owner.CharacterID, TurnID: r.TurnID, Action: "wait", Sources: []string{local.Observations[0].ID}}), nil
	})
	r, _ := runtimeFixture(t, client)
	if err := r.InitializeFree(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := r.engine.State().Revision
	if _, err := r.RunTurn(context.Background()); err == nil {
		t.Fatal("failed requests became fake waiting")
	}
	if r.engine.State().Revision != before {
		t.Fatal("failed request committed a turn")
	}
	fail = false
	if _, err := r.RunTurn(context.Background()); err != nil {
		t.Fatal("retry failed", err)
	}
}

func TestDeterministicAdjudicatorRejectsIllegalCardsAndStaleTurns(t *testing.T) {
	r, _ := runtimeFixture(t, CompletionFunc(func(context.Context, ModelRequest) (ModelReply, error) { return ModelReply{}, nil }))
	if err := r.InitializeCards(context.Background(), 42, 1); err != nil {
		t.Fatal(err)
	}
	state, _ := r.engine.Snapshot()
	scene, _ := readScene(state)
	actor := scene.Game.Players[scene.Game.Current]
	turn := TurnSnapshot{ID: "fixed", WorldRevision: state.Revision, Actors: []string{actor}, Mode: "turn_based"}
	intent := CharacterIntent{ActorID: actor, TurnID: turn.ID, Action: "play", Cards: []string{"not-owned"}, Claim: scene.Game.TableRank, Sources: []string{"validated-before-adjudication"}}
	p, outcomes, err := Adjudicate(state, turn, []CharacterIntent{intent})
	if err != nil || outcomes[0].Accepted {
		t.Fatal("illegal hand accepted", err)
	}
	if len(p.Occurrences) != 1 || p.Occurrences[0].Audience[0] != actor {
		t.Fatal("illegal choice disclosed hidden state")
	}
	if _, _, err := Adjudicate(state, TurnSnapshot{ID: "stale", WorldRevision: state.Revision - 1}, nil); err == nil {
		t.Fatal("stale batch accepted")
	}
	intent.Action = "modify_everyone"
	if _, _, err := Adjudicate(state, turn, []CharacterIntent{intent}); err == nil {
		t.Fatal("arbitrary action accepted")
	}
}
