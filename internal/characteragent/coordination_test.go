package characteragent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"worldsim/internal/config"
)

func waitingClient(t *testing.T, calls *atomic.Int32) CompletionClient {
	return CompletionFunc(func(_ context.Context, req ModelRequest) (ModelReply, error) {
		calls.Add(1)
		if strings.Contains(req.Messages[1].Content, "AUTHOR-ONLY-REASON") {
			t.Error("author rationale entered cognition context")
		}
		var local characterContext
		_ = json.Unmarshal([]byte(req.Messages[1].Content), &local)
		return replyWithIntent(req, CharacterIntent{ActorID: req.Owner.CharacterID, TurnID: req.TurnID, Action: "wait", Sources: []string{local.Observations[0].ID}}), nil
	})
}

func TestEnvironmentPressureIsEvidencedDeterministicAndDoesNotDirectChoices(t *testing.T) {
	var calls atomic.Int32
	runtime, _ := runtimeFixture(t, waitingClient(t, &calls))
	if err := runtime.InitializeFree(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := runtime.engine.Snapshot()
	ids := occupants(state, "白室")
	p := WorldPressure{ID: "deadline-1", Kind: "deadline", Description: "公开计时器将在下一回合扣除三枚筹码", Rationale: "AUTHOR-ONLY-REASON", Actors: ids, Cost: 3, DueTurn: 1, SourceID: fmt.Sprintf("event:%d:independent-scene-init", state.Revision)}
	if err := SchedulePressure(context.Background(), runtime.dir, runtime.engine, p); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("scheduling called a model")
	}
	scheduled := runtime.engine.State().Revision
	if err := SchedulePressure(context.Background(), runtime.dir, runtime.engine, p); err == nil || runtime.engine.State().Revision != scheduled {
		t.Fatal("duplicate pressure modified world")
	}
	result, err := runtime.RunTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Fatal("wrong independent request count")
	}
	for _, outcome := range result.Outcomes {
		if outcome.Action != "wait" || !outcome.Accepted {
			t.Fatal("pressure scripted a choice")
		}
	}
	state, _ = runtime.engine.Snapshot()
	for _, id := range ids {
		if state.Entities[id].Money != 7 {
			t.Fatal("announced resource cost not applied")
		}
	}
	scene, err := readScene(state)
	if err != nil || !scene.Pressures[0].Applied || scene.Pressures[0].Outcome == "" {
		t.Fatal("missing pressure outcome", err)
	}
	revision := state.Revision
	if err := runtime.applyDuePressures(context.Background()); err != nil || runtime.engine.State().Revision != revision {
		t.Fatal("pressure applied twice", err)
	}
	report, err := AssessRuntime(runtime.dir, state)
	if err != nil || len(report.Turns) != 1 || len(report.Turns[0].Changes) != 0 {
		t.Fatal("waiting became fake plot progression", err)
	}
	if report.Turns[0].Reason == "" {
		t.Fatal("stagnation lacks explanation")
	}
}

func TestPressureRejectsUnverifiedSourceAndUnknownTargets(t *testing.T) {
	var calls atomic.Int32
	runtime, _ := runtimeFixture(t, waitingClient(t, &calls))
	_ = runtime.InitializeFree(context.Background())
	before, _ := runtime.engine.Snapshot()
	p := WorldPressure{ID: "p", Kind: "opportunity", Description: "visible condition", Rationale: "why", Actors: []string{"nonexistent"}, DueTurn: 1, SourceID: "invented"}
	if SchedulePressure(context.Background(), runtime.dir, runtime.engine, p) == nil {
		t.Fatal("unverified pressure accepted")
	}
	after, _ := runtime.engine.Snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("invalid pressure changed facts")
	}
}

func TestTierConcurrencyAndBackgroundDoesNotWakeOnOwnWaiting(t *testing.T) {
	var calls, active, maximum atomic.Int32
	base := waitingClient(t, &calls)
	client := CompletionFunc(func(ctx context.Context, req ModelRequest) (ModelReply, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if old >= count || maximum.CompareAndSwap(old, count) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		return base.Complete(ctx, req)
	})
	runtime, _ := runtimeFixture(t, client)
	for tier, policy := range runtime.policies.Tiers {
		policy.MaxParallel = 1
		runtime.policies.Tiers[tier] = policy
	}
	if _, err := runtime.RunTurn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 1 {
		t.Fatal("tier concurrency ignored")
	}
	var background string
	for id := range runtime.policies.Characters {
		if id != runtime.engine.State().WorldLevel.Protagonist {
			background = id
			break
		}
	}
	runtime.policies.Characters[background] = config.CharacterAgentConfig{Importance: config.ImportanceBackground}
	next, err := runtime.RunTurn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Snapshot.Actors) != 4 {
		t.Fatal("background woke from its own wait receipt")
	}
}

func TestInspectionIsReadOnlyAndRedacted(t *testing.T) {
	var calls atomic.Int32
	runtime, _ := runtimeFixture(t, waitingClient(t, &calls))
	_ = runtime.InitializeCards(context.Background(), 42, 1)
	before := migrationTree(t, runtime.dir)
	status, err := PublicSceneStatus(runtime.engine.State())
	if err != nil {
		t.Fatal(err)
	}
	text := string(mustJSON(t, status))
	for _, secret := range []string{"hands", "seed", "UNREVEALED", "rationale"} {
		if strings.Contains(text, secret) {
			t.Fatal("summary exposed hidden state")
		}
	}
	for id := range runtime.engine.State().Entities {
		if _, err := InspectAgent(runtime.dir, id); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, migrationTree(t, runtime.dir)) || calls.Load() != 0 {
		t.Fatal("inspection wrote or called a model")
	}
	// A corrupted persisted scene must be rejected before indexing/playing.
	state, _ := runtime.engine.Snapshot()
	scene, _ := readScene(state)
	scene.Game.Players[1] = scene.Game.Players[0]
	state.WorldLevel.Runtime = sceneValue(scene)
	if _, err := PublicSceneStatus(state); err == nil {
		t.Fatal("duplicate players accepted")
	}
}

func TestCardJournalRecoveryDoesNotRepeatCommittedAction(t *testing.T) {
	var calls atomic.Int32
	runtime, _ := runtimeFixture(t, waitingClient(t, &calls))
	_ = runtime.InitializeFree(context.Background())
	snapshot, _ := runtime.engine.Snapshot()
	// Append a deterministic completed world turn, but keep a stale in-memory snapshot.
	state, _ := runtime.engine.Snapshot()
	turn := TurnSnapshot{ID: "recover-fixed", WorldRevision: state.Revision, Actors: []string{state.WorldLevel.Protagonist}}
	p, _, err := Adjudicate(state, turn, []CharacterIntent{{ActorID: turn.Actors[0], TurnID: turn.ID, Action: "wait", Sources: []string{"already-checked"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.engine.Submit(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	*runtime.engine.State() = *snapshot
	if err := runtime.recoverTail(); err != nil {
		t.Fatal(err)
	}
	if runtime.engine.State().Revision != snapshot.Revision+1 || calls.Load() != 0 {
		t.Fatal("journal recovery repeated model work")
	}
	if _, err := os.Stat(filepath.Join(runtime.dir, "event_log.jsonl")); err != nil {
		t.Fatal(err)
	}
	// A partial journal tail is retained and fails closed.
	f, err := os.OpenFile(filepath.Join(runtime.dir, "event_log.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{")
	_ = f.Close()
	if runtime.recoverTail() == nil {
		t.Fatal("partial tail accepted")
	}
}

func TestLatestGameFrameIsMandatoryAndOldFramesMayBeEvicted(t *testing.T) {
	dir, spec := thoughtFixture(t)
	spec, _ = normalizedSpec(spec)
	store, _ := NewCognitionStore(dir, "沈砚")
	state, _ := store.Read()
	state.Observations = append(state.Observations, Observation{ID: "huge-frame", ContextSlot: "game_state", Kind: "direct_observation", Text: strings.Repeat("旧状态", 10000)})
	if _, _, err := buildCharacterRequest(state, "turn-frame", spec); err == nil {
		t.Fatal("required oversized current frame was silently dropped")
	}
	state.Observations = append(state.Observations, Observation{ID: "current-frame", ContextSlot: "game_state", Kind: "direct_observation", Text: "CURRENT-HAND-CANARY"})
	request, _, err := buildCharacterRequest(state, "turn-frame", spec)
	if err != nil || !strings.Contains(request.Messages[1].Content, "CURRENT-HAND-CANARY") || strings.Contains(request.Messages[1].Content, "旧状态") {
		t.Fatal("latest local game state not retained", err)
	}
}
