package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
	"worldsim/internal/engine"
)

// This acceptance scenario exercises production instance loading and real API
// handlers against temporary files. It never runs simulation or character LLMs.
func TestFiveCharacterConfigurationAcceptance(t *testing.T) {
	ws, _ := agentAPIFixture(t, false)
	dir := ws.inst().dir
	names := []string{"林砚", "沈雾", "顾衡", "陆离", "苏眠"}
	state := ws.inst().engine.State()
	state.WorldLevel.Protagonist = names[0]
	state.Entities = map[string]engine.Entity{}
	for i, name := range names {
		role := "participant"
		if i == 0 {
			role = "protagonist"
		} else if i == 1 {
			role = "rival"
		} else if i == 4 {
			role = "temporary_npc"
		}
		state.Entities[name] = engine.Entity{Alive: true, Status: "active", Location: "白室", Extra: map[string]any{"role": role}}
	}
	if err := ws.inst().engine.Save(filepath.Join(dir, "world_state.json")); err != nil {
		t.Fatal(err)
	}

	// Separate local mock services prove that explicit connection tests use the
	// saved provider's model and key, without sending a request to the internet.
	var requests atomic.Int32
	mock := func(model, key string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			var body struct {
				Model string `json:"model"`
			}
			if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+key || json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != model {
				t.Error("local provider received incorrect endpoint, model or credentials")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
		}))
	}
	a := mock("m1", "A-PRIVATE-CANARY")
	defer a.Close()
	b := mock("m2", "B-PRIVATE-CANARY")
	defer b.Close()
	api, err := config.ReadAPIConfig(ws.apiConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	for id, endpoint := range map[string]string{"a": a.URL, "b": b.URL} {
		p := api.Providers[id]
		p.BaseURL = endpoint + "/v1"
		p.ExtraBody = map[string]any{"private_setting": "EXTRA-PRIVATE-CANARY"}
		api.Providers[id] = p
	}
	if err := config.SaveAPIConfig(api, ws.apiConfigPath()); err != nil {
		t.Fatal(err)
	}

	// Reconstruct server/instance objects, instead of reusing in-memory config or
	// calling Reconcile from the test: startup must register the saved entities.
	reload := func() *http.ServeMux {
		t.Helper()
		loaded, err := config.ReadAPIConfig(ws.apiConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		ws = &worldServer{baseDir: ws.baseDir, current: "test", apiCfg: loaded, worlds: map[string]*worldInstance{}}
		ws.worlds["test"] = ws.newInstance("test")
		mux := http.NewServeMux()
		ws.registerAgentRoutes(mux)
		return mux
	}
	readRegistry := func(worldDir string) characteragent.Document {
		t.Helper()
		r, err := characteragent.NewRegistry(worldDir)
		if err != nil {
			t.Fatal(err)
		}
		d, exists, err := r.Read()
		if err != nil || !exists || len(d.Agents) != 5 {
			t.Fatal("expected five persisted Agent identities")
		}
		return d
	}
	readFile := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	mux := reload()
	initial := readRegistry(dir)
	if initial.Agents[names[0]].Importance != config.ImportanceCore || initial.Agents[names[2]].Importance != config.ImportanceSupporting || initial.Agents[names[4]].Importance != config.ImportanceBackground {
		t.Fatal("startup failed to derive all three importance tiers")
	}
	privateFiles := map[string][]byte{}
	for _, name := range names {
		file := filepath.Join(dir, "agents", initial.Agents[name].AgentID, "private-canary.txt")
		data := []byte(name + "-PRIVATE-CANARY")
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		privateFiles[file] = data
	}

	u := readAgentConfig(t, mux)
	for _, tier := range []config.AgentImportance{config.ImportanceSupporting, config.ImportanceBackground} {
		policy := u.Config.Tiers[tier]
		policy.ModelRef = config.ModelRef{ProviderID: "b", Model: "m2"}
		policy.MaxTokens, policy.ContextBudgetTokens, policy.MaxParallel = 1200, 6000, 1
		u.Config.Tiers[tier] = policy
	}
	u.Config.Characters[names[2]] = config.CharacterAgentConfig{Importance: config.ImportanceSupporting, ModelOverride: &config.ModelRef{ProviderID: "a", Model: "m1"}}
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/config?world=test", u); w.Code != 200 {
		t.Fatalf("save five-character configuration: %d %s", w.Code, w.Body.String())
	}
	saved := readAgentConfig(t, mux)
	runtimePath := filepath.Join(dir, characteragent.RuntimeConfigFile)
	runtimeBytes := readFile(runtimePath)
	assertModels := func(mux *http.ServeMux) {
		t.Helper()
		w := callAgentAPI(t, mux, "GET", "/api/world/agents?world=test", nil)
		var result struct {
			Agents []struct {
				CharacterID string          `json:"character_id"`
				AgentID     string          `json:"agent_id"`
				Model       config.ModelRef `json:"model"`
				Autonomous  bool            `json:"autonomous_runtime"`
			} `json:"agents"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Agents) != 5 {
			t.Fatal("invalid five-character API view")
		}
		for _, row := range result.Agents {
			want := config.ModelRef{ProviderID: "a", Model: "m1"}
			if row.CharacterID == names[3] || row.CharacterID == names[4] {
				want = config.ModelRef{ProviderID: "b", Model: "m2"}
			}
			if row.Model != want || row.AgentID != initial.Agents[row.CharacterID].AgentID || row.Autonomous {
				t.Fatalf("incorrect persisted identity/model/autonomy flag for %s", row.CharacterID)
			}
		}
	}
	assertModels(mux)
	mux = reload()
	if !reflect.DeepEqual(readRegistry(dir), initial) || !reflect.DeepEqual(readAgentConfig(t, mux).Config, saved.Config) {
		t.Fatal("cold instance reload changed registry or saved configuration")
	}
	assertModels(mux)

	// Invalid references, credentials and stale revisions must not alter disk.
	bad := readAgentConfig(t, mux)
	bad.Config.Characters[names[1]] = config.CharacterAgentConfig{Importance: config.ImportanceCore, ModelOverride: &config.ModelRef{ProviderID: "missing", Model: "m1"}}
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/config", bad); w.Code != 400 {
		t.Fatal("invalid provider accepted")
	}
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/"+url.PathEscape(names[0]), map[string]any{"revision": saved.Config.Revision, "registry_revision": saved.RegistryRevision, "character": map[string]any{"importance": "core", "api_key": "INJECTED-PRIVATE-CANARY"}}); w.Code != 400 {
		t.Fatal("credential injection accepted")
	}
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/config", u); w.Code != 409 {
		t.Fatal("stale configuration accepted after reload")
	}
	if !bytes.Equal(runtimeBytes, readFile(runtimePath)) {
		t.Fatal("rejected save changed persisted config")
	}

	// World changes are fixed fixture snapshots, not invented model output.
	for _, alive := range []bool{false, true} {
		s := ws.inst().engine.State()
		ent := s.Entities[names[2]]
		ent.Alive = alive
		ent.Status = "dead"
		if alive {
			ent.Status = "active"
		}
		s.Entities[names[2]] = ent
		s.Revision++
		if err := ws.inst().engine.Save(filepath.Join(dir, "world_state.json")); err != nil {
			t.Fatal(err)
		}
		mux = reload()
		d := readRegistry(dir)
		wantState := "dormant"
		if alive {
			wantState = "active"
		}
		if d.Agents[names[2]].State != wantState || d.Agents[names[2]].AgentID != initial.Agents[names[2]].AgentID {
			t.Fatal("lifecycle change replaced identity or failed to change activation")
		}
		assertModels(mux)
		if !bytes.Equal(runtimeBytes, readFile(runtimePath)) {
			t.Fatal("lifecycle synchronization rewrote saved model settings")
		}
	}
	for file, want := range privateFiles {
		if !bytes.Equal(readFile(file), want) {
			t.Fatal("reload or lifecycle synchronization changed private file")
		}
	}

	// A second world with identical character names must allocate other IDs and
	// must not copy runtime configuration or private files from the first world.
	otherDir := filepath.Join(ws.baseDir, "other")
	if err := os.MkdirAll(otherDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ws.inst().engine.Save(filepath.Join(otherDir, "world_state.json")); err != nil {
		t.Fatal(err)
	}
	ws.worlds["other"] = ws.newInstance("other")
	other := readRegistry(otherDir)
	if other.WorldID == initial.WorldID {
		t.Fatal("world identity reused")
	}
	for _, name := range names {
		if other.Agents[name].AgentID == initial.Agents[name].AgentID {
			t.Fatal("cross-world character identity collision")
		}
		if _, err := os.Stat(filepath.Join(otherDir, "agents", other.Agents[name].AgentID, "private-canary.txt")); !os.IsNotExist(err) {
			t.Fatal("second world inherited private files")
		}
	}
	if _, err := os.Stat(filepath.Join(otherDir, characteragent.RuntimeConfigFile)); !os.IsNotExist(err) {
		t.Fatal("second world inherited saved runtime config")
	}

	// Blank-key provider edits must retain credentials and hidden advanced fields.
	var public struct {
		Revision string `json:"revision"`
	}
	w := callAgentAPI(t, mux, "GET", "/api/llm/providers", nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &public) != nil {
		t.Fatal("could not read public provider view")
	}
	w = callAgentAPI(t, mux, "PUT", "/api/llm/providers", map[string]any{"revision": public.Revision, "providers": map[string]any{"b": map[string]any{"name": "节省预算", "api_key": ""}}})
	if w.Code != 200 {
		t.Fatalf("provider partial edit rejected: %d", w.Code)
	}
	loaded, err := config.ReadAPIConfig(ws.apiConfigPath())
	if err != nil || loaded.Providers["b"].APIKey != "B-PRIVATE-CANARY" || loaded.Providers["b"].ExtraBody["private_setting"] != "EXTRA-PRIVATE-CANARY" {
		t.Fatal("partial provider edit lost credentials or advanced parameters")
	}
	if requests.Load() != 0 {
		t.Fatal("registration, reads, saves or reload made an implicit model request")
	}
	for _, id := range []string{"a", "b"} {
		if w := callAgentAPI(t, mux, "POST", "/api/llm/providers/"+id+"/test", map[string]any{}); w.Code != 200 {
			t.Fatalf("local provider %s test failed: %d", id, w.Code)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("explicit local tests did not issue exactly one request per provider")
	}
	// Recursively check every file under the first world for credential leakage;
	// private per-character canaries are intentional, and checked separately.
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data := readFile(path)
			for _, secret := range []string{"A-PRIVATE-CANARY", "B-PRIVATE-CANARY", "EXTRA-PRIVATE-CANARY", "INJECTED-PRIVATE-CANARY"} {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatal("provider secret leaked into world files")
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
