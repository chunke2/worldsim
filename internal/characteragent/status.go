package characteragent

import "worldsim/internal/engine"

// SceneStatus is safe for the operation console: no hands, deck seed, hidden
// cards, private cognition or provider credentials.
type SceneStatus struct {
	Initialized        bool            `json:"initialized"`
	Kind               string          `json:"kind"`
	Turn               int             `json:"turn"`
	Stalled            bool            `json:"stalled"`
	StalledTurns       int             `json:"stalled_turns"`
	Focus              string          `json:"focus"`
	Actors             []string        `json:"actors"`
	Round              int             `json:"round,omitempty"`
	TableRank          string          `json:"table_rank,omitempty"`
	NextActor          string          `json:"next_actor,omitempty"`
	PendingCount       int             `json:"pending_count,omitempty"`
	Winner             string          `json:"winner,omitempty"`
	Lives              map[string]int  `json:"lives,omitempty"`
	LastOutcomes       []TurnOutcome   `json:"last_outcomes,omitempty"`
	PressureCandidates []WorldPressure `json:"pressure_candidates,omitempty"`
}

func PublicSceneStatus(state *engine.WorldState) (SceneStatus, error) {
	if len(state.WorldLevel.Runtime) == 0 {
		return SceneStatus{}, nil
	}
	scene, err := readScene(state)
	if err != nil {
		return SceneStatus{}, err
	}
	status := SceneStatus{Initialized: true, Kind: scene.Kind, Turn: scene.Turn, Stalled: scene.StalledTurns >= 3, StalledTurns: scene.StalledTurns, Focus: scene.Focus, LastOutcomes: scene.LastOutcomes}
	status.Actors = occupants(state, scene.Focus)
	if scene.Game != nil {
		g := scene.Game
		status.Round = g.Round
		status.TableRank = g.TableRank
		status.NextActor = g.Players[g.Current]
		status.Winner = g.Winner
		status.Lives = map[string]int{}
		for id, life := range g.Lives {
			status.Lives[id] = life
		}
		if g.Pending != nil {
			status.PendingCount = len(g.Pending.Cards)
		}
	}
	if status.Stalled {
		status.PressureCandidates = []WorldPressure{{ID: "stagnation-opportunity", Kind: "opportunity", Description: "提供符合既有世界规则的可见线索或有代价的机会，不能指定角色的回答或行动。", DueTurn: scene.Turn + 1, SourceID: "narrative:stagnation"}}
	}
	return status, nil
}
