package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"worldsim/internal/diagnostic"
	"worldsim/internal/narrative"
)

// GET is observational: it never creates a plan, progress file, or model call.
func (ws *worldServer) handleNarrativeGet(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"error": "没有可用世界"})
		return
	}
	plan, err := narrative.Load(inst.dir)
	if err != nil {
		ws.writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	journal, err := narrative.ReadJournal(inst.dir)
	if err != nil {
		ws.writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	report, err := diagnostic.Analyze(inst.dir)
	if err != nil {
		ws.writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"plan": plan, "progress": narrative.AssessAfterReview(journal, inst.engine.State().Day, plan), "diagnosis": report})
}

func (ws *worldServer) handleNarrativePut(w http.ResponseWriter, r *http.Request) {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"error": "没有可用世界"})
		return
	}
	ws.loopMu.Lock()
	busy := ws.loopRunning && ws.loopWorld == inst.name
	ws.loopMu.Unlock()
	if busy {
		ws.writeJSON(w, 409, map[string]any{"error": "世界运行中，不能同时修改作者计划"})
		return
	}
	var plan narrative.State
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&plan); err != nil {
		ws.writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	journal, err := narrative.ReadJournal(inst.dir)
	if err != nil {
		ws.writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	chapters := map[int]bool{}
	paths, _ := filepath.Glob(filepath.Join(inst.novelW.BookDir, "published", "*.json"))
	for _, path := range paths {
		var n int
		if _, err := fmt.Sscanf(filepath.Base(path), "%03d.json", &n); err == nil {
			chapters[n] = true
		}
	}
	ws.narrativeMu.Lock()
	defer ws.narrativeMu.Unlock()
	if err := narrative.SavePlan(inst.dir, plan, inst.engine.State(), narrative.EventIndex(journal), chapters); err != nil {
		ws.writeJSON(w, 409, map[string]any{"error": err.Error()})
		return
	}
	// Verify persisted bytes; this also makes storage failure visible to callers.
	if _, err := os.Stat(filepath.Join(inst.dir, "narrative.json")); err != nil {
		ws.writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "plan": plan})
}
