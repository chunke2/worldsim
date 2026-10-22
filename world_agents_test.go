package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
	"worldsim/internal/engine"
)

func agentAPIFixture(t *testing.T, register bool) (*worldServer, *http.ServeMux) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "worlds", "test")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	api := &config.APIConfig{Model: "m1", DefaultProvider: "a", ContextBudgetTokens: 10000, Providers: map[string]config.Provider{
		"a": {Name: "A", Protocol: "openai_compatible", BaseURL: "https://a.example/v1", APIKey: "A-PRIVATE-CANARY", Models: []string{"m1"}},
		"b": {Name: "B", Protocol: "openai_compatible", BaseURL: "https://b.example/v1", APIKey: "B-PRIVATE-CANARY", Models: []string{"m2"}},
	}}
	if err := config.SaveAPIConfig(api, filepath.Join(root, "api.json")); err != nil {
		t.Fatal(err)
	}
	se := engine.NewStateEngine(engine.Rules{}, "")
	se.State().Revision = 1
	se.State().WorldLevel.Protagonist = "A"
	for _, name := range []string{"A", "B", "C", "D", "E"} {
		se.State().Entities[name] = engine.Entity{Alive: true, Status: "active"}
	}
	inst := &worldInstance{name: "test", dir: dir, engine: se, apiCfg: api}
	ws := &worldServer{baseDir: filepath.Join(root, "worlds"), current: "test", worlds: map[string]*worldInstance{"test": inst}, apiCfg: api}
	if register {
		r, _ := characteragent.NewRegistry(dir)
		if _, err := r.Reconcile(se.State()); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	ws.registerAgentRoutes(mux)
	return ws, mux
}

func callAgentAPI(t *testing.T, mux *http.ServeMux, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "PRIVATE-CANARY") {
		t.Fatal("API exposed private credential")
	}
	return w
}

func readAgentConfig(t *testing.T, mux *http.ServeMux) agentConfigUpdate {
	t.Helper()
	w := callAgentAPI(t, mux, "GET", "/api/world/agents/config", nil)
	var result agentConfigUpdate
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("could not read Agent config")
	}
	return result
}

func TestAgentGETDoesNotInitializeOrWrite(t *testing.T) {
	ws, mux := agentAPIFixture(t, false)
	for _, path := range []string{"/api/llm/providers", "/api/world/agents", "/api/world/agents/config"} {
		if w := callAgentAPI(t, mux, "GET", path, nil); w.Code != 200 {
			t.Fatalf("GET %s: %d", path, w.Code)
		}
	}
	for _, name := range []string{characteragent.RegistryFile, characteragent.RuntimeConfigFile, "agents"} {
		if _, err := os.Stat(filepath.Join(ws.inst().dir, name)); !os.IsNotExist(err) {
			t.Fatal("GET initialized Agent data")
		}
	}
	if w := callAgentAPI(t, mux, "POST", "/api/world/agents/reconcile", nil); w.Code != 200 {
		t.Fatal("explicit reconcile failed")
	}
	readAgentConfig(t, mux)
	if _, err := os.Stat(filepath.Join(ws.inst().dir, characteragent.RuntimeConfigFile)); !os.IsNotExist(err) {
		t.Fatal("GET persisted derived defaults")
	}
}

func TestAgentConfigCASAndIndividualModelOverride(t *testing.T) {
	ws, mux := agentAPIFixture(t, true)
	u := readAgentConfig(t, mux)
	if len(u.Config.Characters) != 5 {
		t.Fatal("five characters not reflected")
	}
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/config", u); w.Code != 200 {
		t.Fatalf("first save: %d %s", w.Code, w.Body.String())
	}
	file := filepath.Join(ws.inst().dir, characteragent.RuntimeConfigFile)
	before, _ := os.ReadFile(file)
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/config", u); w.Code != 409 {
		t.Fatal("stale config overwrote new revision")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("conflict wrote file")
	}
	u = readAgentConfig(t, mux)
	patch := map[string]any{"revision": u.Config.Revision, "registry_revision": u.RegistryRevision, "character": config.CharacterAgentConfig{Importance: config.ImportanceCore, ModelOverride: &config.ModelRef{ProviderID: "b", Model: "m2"}}}
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/B", patch); w.Code != 200 {
		t.Fatalf("override: %d %s", w.Code, w.Body.String())
	}
	view := callAgentAPI(t, mux, "GET", "/api/world/agents", nil)
	var rows struct {
		Agents []struct {
			CharacterID string          `json:"character_id"`
			Model       config.ModelRef `json:"model"`
			Autonomous  bool            `json:"autonomous_runtime"`
		}
	}
	if json.Unmarshal(view.Body.Bytes(), &rows) != nil || len(rows.Agents) != 5 {
		t.Fatal("invalid Agent list")
	}
	for _, row := range rows.Agents {
		if row.CharacterID == "B" && row.Model.ProviderID != "b" {
			t.Fatal("individual model not resolved")
		}
		if row.Autonomous {
			t.Fatal("registration falsely advertised autonomy")
		}
	}
	u = readAgentConfig(t, mux)
	ws.inst().engine.State().Revision++
	ws.inst().engine.State().Entities["F"] = engine.Entity{Alive: true, Status: "active"}
	callAgentAPI(t, mux, "POST", "/api/world/agents/reconcile", nil)
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/config", u); w.Code != 409 {
		t.Fatal("stale registry revision accepted")
	}
}

func TestAgentConfigRejectsCredentialsUnknownModelsAndBusyWrites(t *testing.T) {
	ws, mux := agentAPIFixture(t, true)
	u := readAgentConfig(t, mux)
	bad := u
	bad.Config.Characters["A"] = config.CharacterAgentConfig{Importance: config.ImportanceCore, ModelOverride: &config.ModelRef{ProviderID: "absent", Model: "x"}}
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/config", bad); w.Code != 400 {
		t.Fatal("unknown Provider accepted")
	}
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/A", map[string]any{"revision": 0, "registry_revision": u.RegistryRevision, "character": map[string]any{"importance": "core", "api_key": "injected"}}); w.Code != 400 {
		t.Fatal("credential field accepted into world config")
	}
	end, err := config.BeginWorldActivity(filepath.Dir(ws.baseDir), ws.inst().dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []struct {
		method, path string
		body         any
	}{{"PUT", "/api/world/agents/config", u}, {"POST", "/api/world/agents/reconcile", nil}, {"PUT", "/api/llm/providers", map[string]any{}}} {
		if w := callAgentAPI(t, mux, endpoint.method, endpoint.path, endpoint.body); w.Code != 409 {
			t.Fatal("running world allowed config mutation")
		}
	}
	end()
	if _, err := os.Stat(filepath.Join(ws.inst().dir, characteragent.RuntimeConfigFile)); !os.IsNotExist(err) {
		t.Fatal("invalid/busy request created configuration")
	}
	end, err = config.BeginProviderEdit(filepath.Dir(ws.baseDir))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	ws.handleSimDay(rec, httptest.NewRequest("POST", "/api/world/sim/day", bytes.NewBufferString(`{"days":1}`)))
	end()
	if rec.Code != 409 {
		t.Fatal("config write allowed concurrent simulation start")
	}
}

func TestProviderCASPreservesKeyAndRejectsReferencedDeletion(t *testing.T) {
	ws, mux := agentAPIFixture(t, true)
	view := callAgentAPI(t, mux, "GET", "/api/llm/providers", nil)
	var result struct {
		Revision string `json:"revision"`
	}
	_ = json.Unmarshal(view.Body.Bytes(), &result)
	u := map[string]any{"revision": result.Revision, "providers": map[string]any{"b": map[string]any{"name": "updated", "api_key": ""}}}
	if w := callAgentAPI(t, mux, "PUT", "/api/llm/providers", u); w.Code != 200 {
		t.Fatalf("Provider update: %d %s", w.Code, w.Body.String())
	}
	if w := callAgentAPI(t, mux, "PUT", "/api/llm/providers", u); w.Code != 409 {
		t.Fatal("stale provider update accepted")
	}
	saved, err := config.LoadAPIConfig(ws.apiConfigPath())
	if err != nil || saved.Providers["b"].APIKey != "B-PRIVATE-CANARY" {
		t.Fatal("Provider patch lost key")
	}
	c := readAgentConfig(t, mux)
	policy := c.Config.Tiers[config.ImportanceCore]
	policy.ModelRef = config.ModelRef{ProviderID: "b", Model: "m2"}
	c.Config.Tiers[config.ImportanceCore] = policy
	if w := callAgentAPI(t, mux, "PUT", "/api/world/agents/config", c); w.Code != 200 {
		t.Fatal("could not save provider reference")
	}
	w := callAgentAPI(t, mux, "GET", "/api/llm/providers", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w := callAgentAPI(t, mux, "PUT", "/api/llm/providers", map[string]any{"revision": result.Revision, "delete": []string{"b"}}); w.Code != 409 {
		t.Fatal("referenced provider deleted")
	}
	if w := callAgentAPI(t, mux, "PUT", "/api/llm/providers", map[string]any{"revision": result.Revision, "providers": map[string]any{"b": map[string]any{"models": []string{"other-model"}}}}); w.Code == 200 {
		t.Fatal("model catalog change invalidated saved world")
	}
}

func TestProviderConnectionTestDoesNotEchoRemoteSecrets(t *testing.T) {
	ws, mux := agentAPIFixture(t, false)
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer B-PRIVATE-CANARY" {
			t.Error("stored key not used")
		}
		http.Error(w, "B-PRIVATE-CANARY", 500)
	}))
	defer remote.Close()
	api, err := config.LoadAPIConfig(ws.apiConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	p := api.Providers["b"]
	p.BaseURL = remote.URL + "/v1"
	api.Providers["b"] = p
	if err := config.SaveAPIConfig(api, ws.apiConfigPath()); err != nil {
		t.Fatal(err)
	}
	w := callAgentAPI(t, mux, "POST", "/api/llm/providers/b/test", map[string]any{"model": "m2"})
	if w.Code != 502 || calls != 1 {
		t.Fatal("connection test failed to use one local request")
	}
}
