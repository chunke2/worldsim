package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"sort"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
	"worldsim/internal/narrative"
)

func (ws *worldServer) handleAgentRuntimeGet(w http.ResponseWriter, r *http.Request) {
	inst := ws.agentWorld(w, r)
	if inst == nil {
		return
	}
	state, err := inst.engine.Snapshot()
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	scene, err := characteragent.PublicSceneStatus(state)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	registry, err := characteragent.NewRegistry(inst.dir)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	doc, _, err := registry.Read()
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	ids := []string{}
	for id := range doc.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	actors := []characteragent.AgentInspection{}
	diagnostics := map[string]string{}
	for _, id := range ids {
		actor, err := characteragent.InspectAgent(inst.dir, id)
		if err != nil {
			diagnostics[id] = err.Error()
		} else {
			actors = append(actors, actor)
		}
	}
	lastEvent := ""
	progress := []characteragent.TurnProgress{}
	if scene.Initialized {
		if report, reportErr := characteragent.AssessRuntime(inst.dir, state); reportErr == nil {
			progress = report.Turns
			if len(progress) > 8 {
				progress = progress[len(progress)-8:]
			}
		} else {
			diagnostics["narrative"] = reportErr.Error()
		}
	}
	if journal, journalErr := narrative.ReadJournal(inst.dir); journalErr == nil {
		for _, event := range journal {
			if event.Revision <= state.Revision {
				lastEvent = fmt.Sprintf("event:%d:%s", event.Revision, event.CommandID)
			}
		}
	} else {
		diagnostics["journal"] = journalErr.Error()
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "scene": scene, "agents": actors, "world_revision": state.Revision, "diagnostics": diagnostics, "last_event_id": lastEvent, "progress": progress})
}

func (ws *worldServer) handleAgentCognition(w http.ResponseWriter, r *http.Request) {
	inst := ws.agentWorld(w, r)
	if inst == nil {
		return
	}
	store, err := characteragent.NewCognitionStore(inst.dir, r.PathValue("character_id"))
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	state, err := store.Read()
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	// This endpoint is for the local author only. Runtime never calls it.
	ws.writeJSON(w, 200, map[string]any{"ok": true, "cognition": state})
}
func (ws *worldServer) handleAgentAudit(w http.ResponseWriter, r *http.Request) {
	inst := ws.agentWorld(w, r)
	if inst == nil {
		return
	}
	entries, err := characteragent.ReadCallAudit(inst.dir, r.PathValue("character_id"))
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "audit": entries})
}
func (ws *worldServer) handleAgentPressure(w http.ResponseWriter, r *http.Request) {
	inst := ws.agentWorld(w, r)
	if inst == nil {
		return
	}
	end, err := config.BeginWorldActivity(filepath.Dir(ws.baseDir), inst.dir)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	defer end()
	_, settings, _, _, settingsErr := ws.loadAgentView(inst)
	if settingsErr != nil {
		ws.agentConfigError(w, settingsErr)
		return
	}
	if settings.ExecutionMode != "independent" {
		ws.writeJSON(w, 400, map[string]any{"error": "请先保存独立角色运行模式"})
		return
	}
	var input characteragent.WorldPressure
	if err := decodeAgentRequest(w, r, &input); err != nil {
		ws.agentConfigError(w, err)
		return
	}
	if err := characteragent.SchedulePressure(r.Context(), inst.dir, inst.engine, input); err != nil {
		ws.agentConfigError(w, err)
		return
	}
	registry, err := characteragent.NewRegistry(inst.dir)
	if err == nil {
		state, snapshotErr := inst.engine.Snapshot()
		err = snapshotErr
		if err == nil {
			_, err = registry.Reconcile(state)
		}
	}
	if err == nil {
		err = inst.engine.Save(filepath.Join(inst.dir, "world_state.json"))
	}
	if err != nil {
		ws.writeJSON(w, 500, map[string]any{"error": "环境条件已提交；派生记录或快照需要恢复", "world_committed": true})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "model_calls": 0})
}
