package main

import (
	"context"
	"net/http"
	"path/filepath"
	"time"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
	"worldsim/internal/llm"
)

func (ws *worldServer) providerView(w http.ResponseWriter, c *config.APIConfig) {
	ws.writeJSON(w, 200, map[string]any{"ok": true, "revision": config.APIRevision(c), "default_provider": c.DefaultProvider, "providers": (config.ProviderRegistry{Providers: c.Providers}).PublicView()})
}

func (ws *worldServer) handleProvidersGet(w http.ResponseWriter, r *http.Request) {
	unlock := config.LockAPIFile(ws.apiConfigPath())
	defer unlock()
	c, err := config.ReadAPIConfig(ws.apiConfigPath())
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	ws.providerView(w, c)
}

func (ws *worldServer) handleProvidersPut(w http.ResponseWriter, r *http.Request) {
	end, err := config.BeginProviderEdit(filepath.Dir(ws.baseDir))
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	defer end()
	var update config.ProviderUpdate
	if decodeAgentRequest(w, r, &update) != nil {
		ws.writeJSON(w, 400, map[string]any{"error": "invalid Provider request"})
		return
	}
	// Never remove a provider referenced by a saved world configuration.
	for _, id := range update.Delete {
		paths, err := filepath.Glob(filepath.Join(ws.baseDir, "*", characteragent.RuntimeConfigFile))
		if err != nil {
			ws.agentConfigError(w, err)
			return
		}
		for _, path := range paths {
			registry, err := characteragent.NewRegistry(filepath.Dir(path))
			if err != nil {
				ws.agentConfigError(w, err)
				return
			}
			c, _, err := registry.ConfigView(config.AgentRuntimeConfig{SchemaVersion: 1})
			if err != nil {
				ws.agentConfigError(w, err)
				return
			}
			for _, tier := range c.Tiers {
				if tier.ProviderID == id {
					ws.writeJSON(w, 409, map[string]any{"error": "Provider is referenced by a world tier"})
					return
				}
			}
			for _, actor := range c.Characters {
				if actor.ModelOverride != nil && actor.ModelOverride.ProviderID == id {
					ws.writeJSON(w, 409, map[string]any{"error": "Provider is referenced by a character"})
					return
				}
			}
		}
	}
	c, err := config.UpdateProviders(ws.apiConfigPath(), update)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	ws.apiCfg = c
	for _, inst := range ws.worlds {
		inst.apiCfg = c
		if inst.novelW != nil {
			inst.novelW.APICfg = c
		}
		if inst.llm != nil && inst.llm.Mock == nil {
			inst.llm.Cfg = c
		}
	}
	ws.providerView(w, c)
}

func (ws *worldServer) handleProviderTest(w http.ResponseWriter, r *http.Request) {
	end, err := config.BeginWorldActivity(filepath.Dir(ws.baseDir), filepath.Join(filepath.Dir(ws.baseDir), "provider-test"))
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	defer end()
	var req struct {
		Model string `json:"model"`
	}
	if decodeAgentRequest(w, r, &req) != nil {
		ws.writeJSON(w, 400, map[string]any{"error": "invalid connection test request"})
		return
	}
	unlock := config.LockAPIFile(ws.apiConfigPath())
	c, err := config.ReadAPIConfig(ws.apiConfigPath())
	unlock()
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	p, ok := c.Providers[r.PathValue("id")]
	if !ok {
		ws.writeJSON(w, 404, map[string]any{"error": "unknown Provider"})
		return
	}
	if req.Model == "" && len(p.Models) > 0 {
		req.Model = p.Models[0]
	}
	if err := (config.ModelRef{ProviderID: r.PathValue("id"), Model: req.Model}).Validate(config.ProviderRegistry{Providers: c.Providers}); err != nil {
		ws.agentConfigError(w, err)
		return
	}
	// Do not return remote error bodies or response samples; a provider can echo
	// credentials or other private configuration in either.
	test := &config.APIConfig{BaseURL: p.BaseURL, APIKey: p.APIKey, URLStrict: p.URLStrict, Model: req.Model, MaxTokens: 32, HTTPTimeoutSeconds: 15, ContextBudgetTokens: 4096}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	start := time.Now()
	_, err = llm.CallAPIMessagesSync(ctx, test, []llm.Message{{Role: "user", Content: "Reply OK."}})
	if err != nil {
		ws.writeJSON(w, 502, map[string]any{"ok": false, "error": "Provider connection test failed", "timeout": ctx.Err() != nil})
		return
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "model": req.Model, "latency_ms": time.Since(start).Milliseconds()})
}
