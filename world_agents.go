package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sort"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
)

func (ws *worldServer) registerAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/world/agents/scenario", ws.handleAgentScenario)
	mux.HandleFunc("GET /api/world/agents/runtime", ws.handleAgentRuntimeGet)
	mux.HandleFunc("POST /api/world/agents/pressures", ws.handleAgentPressure)
	mux.HandleFunc("GET /api/world/agents/{character_id}/cognition", ws.handleAgentCognition)
	mux.HandleFunc("GET /api/world/agents/{character_id}/audit", ws.handleAgentAudit)
	mux.HandleFunc("GET /api/llm/providers", ws.handleProvidersGet)
	mux.HandleFunc("PUT /api/llm/providers", ws.handleProvidersPut)
	mux.HandleFunc("POST /api/llm/providers/{id}/test", ws.handleProviderTest)
	mux.HandleFunc("GET /api/world/agents", ws.handleAgentsGet)
	mux.HandleFunc("GET /api/world/agents/config", ws.handleAgentConfigGet)
	mux.HandleFunc("PUT /api/world/agents/config", ws.handleAgentConfigPut)
	mux.HandleFunc("PUT /api/world/agents/{character_id}", ws.handleAgentPut)
	mux.HandleFunc("POST /api/world/agents/reconcile", ws.handleAgentsReconcile)
}

func decodeAgentRequest(w http.ResponseWriter, r *http.Request, target any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func (ws *worldServer) agentConfigError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, config.ErrRevisionConflict) || errors.Is(err, config.ErrBusy) {
		status = http.StatusConflict
	}
	ws.writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}

func (ws *worldServer) apiConfigPath() string { return filepath.Join(ws.baseDir, "..", "api.json") }

// Bind requests from configuration panels to the world whose draft they loaded.
// Resolve once so a later global selection change cannot redirect a write.
func (ws *worldServer) agentWorld(w http.ResponseWriter, r *http.Request) *worldInstance {
	inst := ws.inst()
	if inst == nil {
		ws.writeJSON(w, 400, map[string]any{"error": "没有可用世界"})
		return nil
	}
	if expected := r.URL.Query().Get("world"); expected != "" && expected != inst.name {
		ws.writeJSON(w, http.StatusConflict, map[string]any{"error": "世界已切换，请重新读取人物配置"})
		return nil
	}
	return inst
}

func defaultAgentConfig(c *config.APIConfig) config.AgentRuntimeConfig {
	result := config.AgentRuntimeConfig{SchemaVersion: 1, Tiers: map[config.AgentImportance]config.TierPolicy{}, Characters: map[string]config.CharacterAgentConfig{}}
	for tier, modelTier := range map[config.AgentImportance]string{config.ImportanceCore: "premium", config.ImportanceSupporting: "normal", config.ImportanceBackground: "fast"} {
		policy := config.TierPolicy{ModelRef: config.ModelRef{ProviderID: c.DefaultProvider, Model: c.TierModel(modelTier)}}
		switch tier {
		case config.ImportanceCore:
			policy.WakePolicy = "every_relevant_turn"
		case config.ImportanceSupporting:
			policy.WakePolicy = "on_involvement"
		default:
			policy.WakePolicy = "on_direct_event"
		}
		result.Tiers[tier] = policy
	}
	return result
}

func (ws *worldServer) loadAgentView(inst *worldInstance) (*characteragent.AgentRegistry, config.AgentRuntimeConfig, characteragent.Document, *config.APIConfig, error) {
	r, err := characteragent.NewRegistry(inst.dir)
	if err != nil {
		return nil, config.AgentRuntimeConfig{}, characteragent.Document{}, nil, err
	}
	unlock := config.LockAPIFile(ws.apiConfigPath())
	c, err := config.ReadAPIConfig(ws.apiConfigPath())
	unlock()
	if err != nil {
		return nil, config.AgentRuntimeConfig{}, characteragent.Document{}, nil, err
	}
	view, d, err := r.ConfigView(defaultAgentConfig(c))
	return r, view, d, c, err
}

func (ws *worldServer) handleAgentConfigGet(w http.ResponseWriter, r *http.Request) {
	inst := ws.agentWorld(w, r)
	if inst == nil {
		return
	}
	_, c, d, api, err := ws.loadAgentView(inst)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	validation := ""
	if err := c.Validate(config.ProviderRegistry{DefaultProvider: api.DefaultProvider, Providers: api.Providers}); err != nil {
		validation = err.Error()
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "config": c, "registry_revision": d.Revision, "world_revision": d.WorldRevision, "validation_error": validation})
}

func (ws *worldServer) handleAgentsGet(w http.ResponseWriter, r *http.Request) {
	inst := ws.agentWorld(w, r)
	if inst == nil {
		return
	}
	_, c, d, api, err := ws.loadAgentView(inst)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	ids := make([]string, 0, len(d.Agents))
	for id := range d.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]map[string]any, 0, len(ids))
	providers := config.ProviderRegistry{DefaultProvider: api.DefaultProvider, Providers: api.Providers}
	for _, id := range ids {
		a := d.Agents[id]
		row := map[string]any{"agent_id": a.AgentID, "character_id": id, "state": a.State, "dormant_reason": a.DormantReason, "importance": c.Characters[id].Importance, "autonomous_runtime": c.ExecutionMode == "independent"}
		model, err := c.ResolveModel(id, providers)
		if err != nil {
			row["model_error"] = err.Error()
		} else {
			row["model"] = model
		}
		rows = append(rows, row)
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "agents": rows, "count": len(rows), "registry_revision": d.Revision, "config_revision": c.Revision})
}

type agentConfigUpdate struct {
	RegistryRevision *int                      `json:"registry_revision"`
	Config           config.AgentRuntimeConfig `json:"config"`
}

func (ws *worldServer) handleAgentConfigPut(w http.ResponseWriter, r *http.Request) {
	ws.saveAgentConfig(w, r, false)
}

func (ws *worldServer) handleAgentPut(w http.ResponseWriter, r *http.Request) {
	ws.saveAgentConfig(w, r, true)
}

func (ws *worldServer) saveAgentConfig(w http.ResponseWriter, r *http.Request, individual bool) {
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
	// Lock the API document through validation/write so Provider replacement
	// cannot race with saving a reference to it.
	unlock := config.LockAPIFile(ws.apiConfigPath())
	defer unlock()
	api, err := config.ReadAPIConfig(ws.apiConfigPath())
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	registry, err := characteragent.NewRegistry(inst.dir)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	var update agentConfigUpdate
	if individual {
		var patch struct {
			RegistryRevision *int                        `json:"registry_revision"`
			Revision         *int                        `json:"revision"`
			Character        config.CharacterAgentConfig `json:"character"`
		}
		if decodeAgentRequest(w, r, &patch) != nil || patch.Revision == nil {
			ws.writeJSON(w, 400, map[string]any{"error": "invalid character config request"})
			return
		}
		current, d, err := registry.ConfigView(defaultAgentConfig(api))
		if err != nil {
			ws.agentConfigError(w, err)
			return
		}
		id := r.PathValue("character_id")
		if _, ok := d.Agents[id]; !ok {
			ws.writeJSON(w, 404, map[string]any{"error": "unknown character"})
			return
		}
		current.Revision = *patch.Revision
		current.Characters[id] = patch.Character
		update = agentConfigUpdate{RegistryRevision: patch.RegistryRevision, Config: current}
	} else if decodeAgentRequest(w, r, &update) != nil {
		ws.writeJSON(w, 400, map[string]any{"error": "invalid Agent config request"})
		return
	}
	if update.RegistryRevision == nil {
		ws.writeJSON(w, 400, map[string]any{"error": "registry_revision is required"})
		return
	}
	saved, err := registry.SaveConfig(update.Config, *update.RegistryRevision, config.ProviderRegistry{DefaultProvider: api.DefaultProvider, Providers: api.Providers})
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "config": saved, "registry_revision": *update.RegistryRevision})
}

func (ws *worldServer) handleAgentsReconcile(w http.ResponseWriter, r *http.Request) {
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
	registry, err := characteragent.NewRegistry(inst.dir)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	snapshot, err := inst.engine.Snapshot()
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	d, err := registry.Reconcile(snapshot)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "registry_revision": d.Revision, "count": len(d.Agents)})
}
