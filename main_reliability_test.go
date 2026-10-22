package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"worldsim/internal/config"
	"worldsim/internal/engine"
	"worldsim/internal/novel"
	"worldsim/internal/sim"
)

func TestNovelListIsPureRead(t *testing.T) {
	dir := t.TempDir()
	se := engine.NewStateEngine(engine.Rules{}, "")
	se.State().Entities = map[string]engine.Entity{"A": {}}
	se.State().WorldLevel.Protagonist = "A"
	se.State().WorldLevel.NarrativePOV = "A"
	calls := 0
	client := &sim.LLMClient{Mock: func(system, user string) string { calls++; return "[]" }}
	w := novel.NewWriter(nil, "book", filepath.Join(dir, "novel"), nil, "A")
	inst := &worldInstance{name: "w", dir: dir, engine: se, sim: sim.NewSimulator(se, dir), llm: client, novelW: w}
	ws := &worldServer{current: "w", worlds: map[string]*worldInstance{"w": inst}}
	req := httptest.NewRequest("GET", "/api/world/novel", nil)
	rec := httptest.NewRecorder()
	ws.handleNovelList(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("GET invoked model %d times", calls)
	}
	if _, err := os.Stat(filepath.Join(w.BookDir, "chapter_plans.json")); !os.IsNotExist(err) {
		t.Fatal("GET created a chapter plan")
	}
}

func TestWorldConfigEndpointPreservesAdvancedFields(t *testing.T) {
	root := t.TempDir()
	worlds := filepath.Join(root, "worlds")
	if err := os.MkdirAll(worlds, 0755); err != nil {
		t.Fatal(err)
	}
	original := &config.APIConfig{APIKey: "old", BaseURL: "http://old", Model: "old", MaxTokens: 9000, HTTPTimeoutSeconds: 333, ExtraBody: map[string]any{"x": true}, ModelTiers: map[string]string{"premium": "writer"}, Novel: &config.GenerationConfig{Model: "novel", MaxTokens: 18000, HTTPTimeoutSeconds: 700}}
	if err := config.SaveAPIConfig(original, filepath.Join(root, "api.json")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(worlds, "w")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	se := engine.NewStateEngine(engine.Rules{}, "")
	se.State().Entities = map[string]engine.Entity{"A": {}}
	se.State().WorldLevel.Protagonist = "A"
	inst := &worldInstance{name: "w", dir: dir, engine: se, apiCfg: original}
	ws := &worldServer{baseDir: worlds, current: "w", worlds: map[string]*worldInstance{"w": inst}, apiCfg: original}
	body := []byte(`{"mode":"real","base_url":"http://new","model":"new","api_key":"changed"}`)
	req := httptest.NewRequest("POST", "/api/world/sim/llm", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	ws.handleSetLLM(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := config.LoadAPIConfig(filepath.Join(root, "api.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if got.MaxTokens != 9000 || got.HTTPTimeoutSeconds != 333 || got.ExtraBody["x"] != true || got.ModelTiers["premium"] != "writer" || got.Novel == nil || got.Novel.MaxTokens != 18000 || got.Novel.HTTPTimeoutSeconds != 700 {
		t.Fatalf("fields lost: %s", b)
	}
}

func TestNarrativeGetIsReadOnlyAndPutValidates(t *testing.T) {
	dir := t.TempDir()
	se := engine.NewStateEngine(engine.Rules{}, filepath.Join(dir, "event_log.jsonl"))
	se.State().Entities = map[string]engine.Entity{"A": {Location: "room", Relationship: map[string]float64{}}}
	se.State().WorldLevel.Protagonist = "A"
	se.State().WorldLevel.NarrativePOV = "A"
	se.State().WorldLevel.Locations = map[string]engine.Location{"room": {Name: "room"}}
	if err := se.Save(filepath.Join(dir, "world_state.json")); err != nil {
		t.Fatal(err)
	}
	w := novel.NewWriter(nil, "book", filepath.Join(dir, "novel"), nil, "A")
	inst := &worldInstance{name: "w", dir: dir, engine: se, sim: sim.NewSimulator(se, dir), novelW: w}
	ws := &worldServer{current: "w", worlds: map[string]*worldInstance{"w": inst}}
	req := httptest.NewRequest("GET", "/api/world/narrative", nil)
	rec := httptest.NewRecorder()
	ws.handleNarrativeGet(rec, req)
	if rec.Code != 200 {
		t.Fatalf("get status=%d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "narrative.json")); !os.IsNotExist(err) {
		t.Fatal("GET created narrative state")
	}
	put := httptest.NewRequest("PUT", "/api/world/narrative", bytes.NewReader([]byte(`{"version":1,"theme":"trust","threads":[],"pressures":[]}`)))
	putRec := httptest.NewRecorder()
	ws.handleNarrativePut(putRec, put)
	if putRec.Code != 200 {
		t.Fatalf("put status=%d %s", putRec.Code, putRec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "narrative.json")); err != nil {
		t.Fatal(err)
	}
}
