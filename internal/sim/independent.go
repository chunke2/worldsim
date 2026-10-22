package sim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
	"worldsim/internal/narrative"
)

func (s *Simulator) IndependentMode() (bool, error) {
	_, _, enabled, err := s.independentSettings()
	return enabled, err
}

// Includes worst-case serialized Provider queueing across the active cast.
func (s *Simulator) TurnTimeout() (time.Duration, error) {
	_, providers, enabled, err := s.independentSettings()
	if err != nil {
		return 0, err
	}
	if !enabled {
		return 170 * time.Second, nil
	}
	state, err := s.engine.Snapshot()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entity := range state.Entities {
		if entity.Alive && entity.Status == "active" {
			count++
		}
	}
	if scene, err := characteragent.PublicSceneStatus(state); err == nil && scene.Kind == "liars_room" {
		count = 1
	}
	if count < 1 {
		count = 1
	}
	seconds := 120
	for _, p := range providers.Providers {
		if p.TimeoutSeconds > seconds {
			seconds = p.TimeoutSeconds
		}
	}
	return time.Duration(count)*time.Duration(seconds)*time.Second + 30*time.Second, nil
}

// SetCharacterClient injects a stateless test/local transport without touching API configuration.
func (s *Simulator) SetCharacterClient(client characteragent.CompletionClient, providers config.ProviderRegistry) {
	s.characterClient = client
	s.characterProviders = &providers
}

func (s *Simulator) independentSettings() (config.AgentRuntimeConfig, config.ProviderRegistry, bool, error) {
	if _, err := os.Stat(filepath.Join(s.worldDir, characteragent.RuntimeConfigFile)); os.IsNotExist(err) {
		return config.AgentRuntimeConfig{}, config.ProviderRegistry{}, false, nil
	} else if err != nil {
		return config.AgentRuntimeConfig{}, config.ProviderRegistry{}, false, err
	}
	r, err := characteragent.NewRegistry(s.worldDir)
	if err != nil {
		return config.AgentRuntimeConfig{}, config.ProviderRegistry{}, false, err
	}
	c, _, err := r.ConfigView(config.AgentRuntimeConfig{SchemaVersion: 1})
	if err != nil {
		return c, config.ProviderRegistry{}, false, err
	}
	if c.ExecutionMode != "independent" {
		return c, config.ProviderRegistry{}, false, nil
	}
	providers := config.ProviderRegistry{}
	if s.characterProviders != nil {
		providers = *s.characterProviders
	} else {
		path := filepath.Join(s.worldDir, "..", "..", "api.json")
		unlock := config.LockAPIFile(path)
		api, err := config.ReadAPIConfig(path)
		unlock()
		if err != nil {
			return c, providers, true, err
		}
		providers = config.ProviderRegistry{DefaultProvider: api.DefaultProvider, Providers: api.Providers}
	}
	if err := c.Validate(providers); err != nil {
		return c, providers, true, err
	}
	return c, providers, true, nil
}

func (s *Simulator) IndependentRuntime() (*characteragent.IndependentRuntime, error) {
	c, providers, enabled, err := s.independentSettings()
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, errors.New("select independent execution mode in character settings first")
	}
	client := s.characterClient
	if client == nil {
		if s.modelMode == "off" || s.modelMode == "mock" {
			return nil, errors.New("独立角色模式不能使用兼容管线的 off/mock；请启用 real 或使用专用本地测试客户端")
		}
		router, err := characteragent.NewModelRouter(s.worldDir, providers)
		if err != nil {
			return nil, err
		}
		client = router
	}
	return characteragent.NewIndependentRuntime(s.worldDir, s.engine, c, providers, client)
}

func (s *Simulator) runIndependentDay(ctx context.Context) (*DayResult, error) {
	r, err := s.IndependentRuntime()
	if err != nil {
		return nil, err
	}
	turn, runErr := r.RunTurn(ctx)
	state, err := s.engine.Snapshot()
	if err != nil {
		return nil, err
	}
	s.day = state.Day
	result := &DayResult{Day: s.day, Mode: "independent", AgentTurn: &turn, Events: []EventCard{}, Chronicle: []ChronicleEntry{}, Dialogue: []DialogueTurn{}}
	if err := s.engine.Save(filepath.Join(s.worldDir, "world_state.json")); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("world journal may be committed; snapshot save failed: %w", err))
	}
	entries, err := narrative.ReadJournal(s.worldDir)
	if err != nil {
		return result, errors.Join(runErr, err)
	}
	seen := map[string]bool{}
	for _, entry := range s.chronicle {
		if entry.OccurrenceID != "" {
			seen[entry.OccurrenceID] = true
		}
	}
	for _, entry := range entries {
		if entry.Revision > state.Revision {
			continue
		}
		eventID := fmt.Sprintf("event:%d:%s", entry.Revision, entry.CommandID)
		for index, o := range entry.Occurrences {
			occurrenceID := fmt.Sprintf("%s:%d", eventID, index)
			if seen[occurrenceID] {
				continue
			}
			kind := "OBSERVATION"
			if o.Kind == "utterance" {
				kind = "SAID"
			}
			material := ChronicleEntry{OccurrenceID: occurrenceID, SourceEvents: []string{eventID}, Audience: append([]string{}, o.Audience...), Day: entry.Day, Kind: kind, Time: entry.Timestamp, Content: o.Content, Visibility: "restricted", Source: o.Actor, Credibility: 1, Weight: 0.5, Tags: []string{"independent", o.Kind}}
			s.chronicle = append(s.chronicle, material)
			result.Chronicle = append(result.Chronicle, material)
			seen[occurrenceID] = true
			if o.Kind == "utterance" {
				result.Dialogue = append(result.Dialogue, DialogueTurn{Speaker: o.Actor, Speech: o.Content, Audience: o.Audience, SourceEvent: eventID})
			}
		}
	}
	s.saveChronicle()
	if err := characteragent.SaveRuntimeAssessment(s.worldDir, state); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if turn.Winner != "" {
		result.Paused = true
		result.PauseMsg = "牌局结束，唯一幸存者：" + turn.Winner
	}
	if status, err := characteragent.PublicSceneStatus(state); err == nil && status.Stalled {
		result.Paused = true
		result.PauseMsg = "连续三个回合没有有效变化，已停止重复请求。请审阅世界内压力候选。"
	}
	return result, runErr
}
