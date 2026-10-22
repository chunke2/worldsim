package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/http"
	"path/filepath"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
	"worldsim/internal/sim"
)

func (ws *worldServer) handleAgentScenario(w http.ResponseWriter, r *http.Request) {
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
	var input struct {
		Kind  string `json:"kind"`
		Seed  *int64 `json:"seed"`
		Lives int    `json:"lives"`
	}
	if decodeAgentRequest(w, r, &input) != nil || input.Kind != "liars_room" {
		ws.writeJSON(w, 400, map[string]any{"error": "当前支持 liars_room 五人牌局"})
		return
	}
	_, settings, _, api, err := ws.loadAgentView(inst)
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	if settings.ExecutionMode != "independent" {
		ws.writeJSON(w, 400, map[string]any{"error": "请先保存独立角色运行模式"})
		return
	}
	if input.Lives == 0 {
		input.Lives = 1
	}
	seed := int64(0)
	if input.Seed != nil {
		seed = *input.Seed
	} else {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			ws.writeJSON(w, 500, map[string]any{"error": "无法生成牌局随机种子"})
			return
		}
		seed = int64(binary.LittleEndian.Uint64(random[:]))
	}
	// Initialization commits deterministic setup only; no API request is made.
	client := characteragent.CompletionFunc(func(_ context.Context, _ characteragent.ModelRequest) (characteragent.ModelReply, error) {
		return characteragent.ModelReply{}, errors.New("setup does not call models")
	})
	runtime, err := characteragent.NewIndependentRuntime(inst.dir, inst.engine, settings, config.ProviderRegistry{DefaultProvider: api.DefaultProvider, Providers: api.Providers}, client)
	if err == nil {
		err = runtime.InitializeCards(r.Context(), seed, input.Lives)
	}
	if err != nil {
		ws.agentConfigError(w, err)
		return
	}
	if err := inst.engine.Save(filepath.Join(inst.dir, "world_state.json")); err != nil {
		ws.writeJSON(w, 500, map[string]any{"error": "牌局已提交，保存快照失败，请从日志恢复", "world_committed": true})
		return
	}
	if inst.sim == nil {
		inst.sim = sim.NewSimulator(inst.engine, inst.dir)
	}
	ws.writeJSON(w, 200, map[string]any{"ok": true, "revision": inst.engine.State().Revision, "model_calls": 0})
}
