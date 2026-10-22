package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
	"worldsim/internal/engine"
	"worldsim/internal/novel"
	"worldsim/internal/sim"
)

func waitIndependentLoop(t *testing.T, ws *worldServer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ws.loopMu.Lock()
		running := ws.loopRunning
		ws.loopMu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	ws.loopMu.Lock()
	if ws.loopCancel != nil {
		ws.loopCancel()
	}
	ws.loopMu.Unlock()
	t.Fatal("independent loop did not terminate")
}

func TestIndependentBackgroundLoopStopsOnModelFailureWithoutAutomaticRetry(t *testing.T) {
	ws, mux := independentAPIFixture(t)
	inst := ws.inst()
	inst.sim = sim.NewSimulator(inst.engine, inst.dir)
	var calls atomic.Int32
	inst.sim.SetCharacterClient(characteragent.CompletionFunc(func(context.Context, characteragent.ModelRequest) (characteragent.ModelReply, error) {
		calls.Add(1)
		return characteragent.ModelReply{}, errors.New("mock unavailable")
	}), config.ProviderRegistry{Providers: inst.apiCfg.Providers, DefaultProvider: inst.apiCfg.DefaultProvider})
	mux.HandleFunc("POST /api/world/loop", ws.handleLoopSet)
	response := callAgentAPI(t, mux, "POST", "/api/world/loop", map[string]any{"action": "start", "days": 30})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	waitIndependentLoop(t, ws)
	if calls.Load() != 5 {
		t.Fatal("failed loop retried or did not activate each character", calls.Load())
	}
	if inst.engine.State().Day != 0 {
		t.Fatal("failed model request advanced world clock")
	}
}

func TestIndependentBackgroundGameContinuesPastReadinessUntilSoleSurvivor(t *testing.T) {
	ws, mux := independentAPIFixture(t)
	inst := ws.inst()
	response := callAgentAPI(t, mux, "POST", "/api/world/agents/scenario?world=test", map[string]any{"kind": "liars_room", "seed": 42, "lives": 1})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	inst.sim = sim.NewSimulator(inst.engine, inst.dir)
	var mu sync.Mutex
	acted := map[string]bool{}
	inst.sim.SetCharacterClient(characteragent.CompletionFunc(func(_ context.Context, req characteragent.ModelRequest) (characteragent.ModelReply, error) {
		var local struct {
			Observations []struct {
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"observations"`
		}
		_ = json.Unmarshal([]byte(req.Messages[1].Content), &local)
		var visible struct {
			Hand      []string `json:"hand"`
			Pending   bool     `json:"pending"`
			TableRank string   `json:"table_rank"`
		}
		source := ""
		for i := len(local.Observations) - 1; i >= 0; i-- {
			if strings.HasPrefix(local.Observations[i].Text, "你的牌局可见状态：") {
				_ = json.Unmarshal([]byte(strings.TrimPrefix(local.Observations[i].Text, "你的牌局可见状态：")), &visible)
				source = local.Observations[i].ID
				break
			}
		}
		intent := characteragent.CharacterIntent{ActorID: req.Owner.CharacterID, TurnID: req.TurnID, Action: "challenge", Sources: []string{source}}
		if !visible.Pending {
			if len(visible.Hand) == 0 {
				return characteragent.ModelReply{}, errors.New("mock missing local hand")
			}
			intent.Action = "play"
			intent.Cards = []string{visible.Hand[0]}
			intent.Claim = visible.TableRank
		}
		mu.Lock()
		acted[req.Owner.CharacterID] = true
		mu.Unlock()
		body, _ := json.Marshal(map[string]any{"actor_id": req.Owner.CharacterID, "turn_id": req.TurnID, "updates": []any{}, "intent": intent})
		return characteragent.ModelReply{Content: string(body), FinishReason: "stop"}, nil
	}), config.ProviderRegistry{Providers: inst.apiCfg.Providers, DefaultProvider: inst.apiCfg.DefaultProvider})
	mux.HandleFunc("POST /api/world/loop", ws.handleLoopSet)
	response = callAgentAPI(t, mux, "POST", "/api/world/loop", map[string]any{"action": "start", "days": 30})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	waitIndependentLoop(t, ws)
	status, err := characteragent.PublicSceneStatus(inst.engine.State())
	if err != nil {
		t.Fatal(err)
	}
	if status.Winner == "" || status.Turn <= 2 || !inst.lastDay.Paused {
		t.Fatal("loop stopped before the game ended")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(acted) != 5 {
		t.Fatal("not all five agents acted")
	}
}

func independentAPIFixture(t *testing.T) (*worldServer, *http.ServeMux) {
	t.Helper()
	ws, mux := agentAPIFixture(t, true)
	inst := ws.inst()
	state, _ := inst.engine.Snapshot()
	for id, entity := range state.Entities {
		entity.Location = "room"
		entity.Money = 10
		state.Entities[id] = entity
	}
	inst.engine = engine.NewStateEngine(engine.Rules{}, filepath.Join(inst.dir, "event_log.jsonl"))
	*inst.engine.State() = *state
	update := readAgentConfig(t, mux)
	update.Config.ExecutionMode = "independent"
	if response := callAgentAPI(t, mux, "PUT", "/api/world/agents/config?world=test", update); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	return ws, mux
}

func TestIndependentScenarioAndInspectionAPIWithoutModels(t *testing.T) {
	ws, mux := independentAPIFixture(t)
	result := callAgentAPI(t, mux, "POST", "/api/world/agents/scenario?world=test", map[string]any{"kind": "liars_room", "seed": 42, "lives": 1})
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"model_calls":0`) {
		t.Fatal(result.Code, result.Body.String())
	}
	revision := ws.inst().engine.State().Revision
	if result := callAgentAPI(t, mux, "POST", "/api/world/agents/scenario?world=test", map[string]any{"kind": "liars_room", "seed": 43, "lives": 1}); result.Code == 200 || ws.inst().engine.State().Revision != revision {
		t.Fatal("scenario overwritten")
	}
	before, _ := os.ReadFile(filepath.Join(ws.inst().dir, "event_log.jsonl"))
	for _, path := range []string{"/api/world/agents/runtime", "/api/world/agents/A/cognition", "/api/world/agents/A/audit"} {
		result := callAgentAPI(t, mux, "GET", path+"?world=test", nil)
		if result.Code != 200 {
			t.Fatal(result.Code, result.Body.String())
		}
		if path == "/api/world/agents/runtime" {
			for _, secret := range []string{`"hands"`, `"seed"`, `"cards"`} {
				if strings.Contains(result.Body.String(), secret) {
					t.Fatal("public summary exposed game secrets")
				}
			}
		}
		if result := callAgentAPI(t, mux, "GET", path+"?world=old", nil); result.Code != 409 {
			t.Fatal("stale inspection world accepted")
		}
	}
	after, _ := os.ReadFile(filepath.Join(ws.inst().dir, "event_log.jsonl"))
	if string(before) != string(after) {
		t.Fatal("GET changed journal")
	}
	registry, _ := characteragent.NewRegistry(ws.inst().dir)
	doc, _, _ := registry.Read()
	if _, err := os.Stat(filepath.Join(ws.inst().dir, "agents", doc.Agents["A"].AgentID, "cognition.json")); !os.IsNotExist(err) {
		t.Fatal("inspection initialized private cognition")
	}
	pressure := characteragent.WorldPressure{ID: "public-clue", Kind: "opportunity", Description: "桌面出现可见提示", Rationale: "AUTHOR-ONLY-REASON", DueTurn: 1, SourceID: "event:2:independent-card-init", Actors: []string{"A", "B", "C", "D", "E"}}
	result = callAgentAPI(t, mux, "POST", "/api/world/agents/pressures?world=test", pressure)
	if result.Code != 200 {
		t.Fatal(result.Code, result.Body.String())
	}
}

func TestScenarioAndPressureAreBlockedDuringActiveWorldWork(t *testing.T) {
	ws, mux := independentAPIFixture(t)
	end, err := config.BeginWorldActivity(filepath.Dir(ws.baseDir), ws.inst().dir)
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	revision := ws.inst().engine.State().Revision
	for _, path := range []string{"/api/world/agents/scenario", "/api/world/agents/pressures"} {
		if result := callAgentAPI(t, mux, "POST", path+"?world=test", map[string]any{}); result.Code != 409 {
			t.Fatal("active work did not block mutation")
		}
	}
	if ws.inst().engine.State().Revision != revision {
		t.Fatal("busy request changed world")
	}
}

func TestIndependentNovelForceCannotBypassVerifiedMaterialReadiness(t *testing.T) {
	ws, mux := independentAPIFixture(t)
	result := callAgentAPI(t, mux, "POST", "/api/world/agents/scenario?world=test", map[string]any{"kind": "liars_room", "seed": 42, "lives": 1})
	if result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	inst := ws.inst()
	inst.sim = sim.NewSimulator(inst.engine, inst.dir)
	inst.novelW = &novel.Writer{BookDir: filepath.Join(inst.dir, "book"), DaysPerCh: 3}
	mux.HandleFunc("POST /api/world/novel/generate", ws.handleNovelGenerate)
	result = callAgentAPI(t, mux, "POST", "/api/world/novel/generate", map[string]any{"force": true})
	if result.Code != 409 || !strings.Contains(result.Body.String(), "至少两次") {
		t.Fatal("novel readiness incorrectly routed", result.Code, result.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(result.Body.Bytes(), &body)
	if _, err := os.Stat(inst.novelW.BookDir); !os.IsNotExist(err) {
		t.Fatal("rejected request wrote book")
	}
}
