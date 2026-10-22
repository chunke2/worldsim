package characteragent

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"worldsim/internal/engine"
)

type SceneState struct {
	SchemaVersion    int             `json:"schema_version"`
	Kind             string          `json:"kind"` // free | liars_room
	Turn             int             `json:"turn"`
	Focus            string          `json:"focus"`
	LastTurnID       string          `json:"last_turn_id,omitempty"`
	LastTurnRevision int             `json:"last_turn_revision"`
	Game             *CardGame       `json:"game,omitempty"`
	Pressures        []WorldPressure `json:"pressures,omitempty"`
	NextWake         []string        `json:"next_wake,omitempty"`
	StalledTurns     int             `json:"stalled_turns"`
	LastOutcomes     []TurnOutcome   `json:"last_outcomes,omitempty"`
}

type CardPlay struct {
	Actor string   `json:"actor"`
	Cards []string `json:"cards"`
	Claim string   `json:"claim"`
}
type CardGame struct {
	Players                  []string            `json:"players"`
	Hands                    map[string][]string `json:"hands"`
	Lives                    map[string]int      `json:"lives"`
	Current                  int                 `json:"current"`
	Pending                  *CardPlay           `json:"pending,omitempty"`
	TableRank                string              `json:"table_rank"`
	Round                    int                 `json:"round"`
	Seed                     int64               `json:"seed,string"`
	AcceptedWithoutChallenge int                 `json:"accepted_without_challenge"`
	MaxAccepts               int                 `json:"max_accepts"`
	Winner                   string              `json:"winner,omitempty"`
}

// Pressures are environment constraints, never requested character decisions.
type WorldPressure struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	DueTurn     int      `json:"due_turn"`
	Cost        int      `json:"cost"`
	SourceID    string   `json:"source_id"`
	Applied     bool     `json:"applied"`
	Rationale   string   `json:"rationale,omitempty"`
	Actors      []string `json:"actors,omitempty"`
	AppliedTurn int      `json:"applied_turn,omitempty"`
	Outcome     string   `json:"outcome,omitempty"`
}

func readScene(state *engine.WorldState) (SceneState, error) {
	if len(state.WorldLevel.Runtime) == 0 {
		return SceneState{}, errors.New("independent scene not initialized")
	}
	b, err := json.Marshal(state.WorldLevel.Runtime)
	if err != nil {
		return SceneState{}, errors.New("invalid independent scene")
	}
	var scene SceneState
	if !decodeLegacyArtifact(b, &scene) || scene.SchemaVersion != 1 || (scene.Kind != "free" && scene.Kind != "liars_room") || scene.Turn < 0 {
		return scene, errors.New("invalid independent scene state")
	}
	if scene.Kind == "liars_room" && (scene.Game == nil || len(scene.Game.Players) != 5 || scene.Game.Current < 0 || scene.Game.Current >= 5 || scene.Game.MaxAccepts < 1) {
		return scene, errors.New("invalid card game state")
	}
	if scene.Kind == "liars_room" {
		g := scene.Game
		seen, cards := map[string]bool{}, map[string]bool{}
		if g.Round < 1 || g.MaxAccepts != 3 || g.AcceptedWithoutChallenge < 0 || g.AcceptedWithoutChallenge >= g.MaxAccepts || (g.TableRank != "J" && g.TableRank != "Q" && g.TableRank != "K") || len(g.Lives) != 5 {
			return scene, errors.New("invalid card counters")
		}
		checkCard := func(card string) bool {
			parts := strings.Split(card, "-")
			if len(parts) != 3 || parts[1] != strconv.Itoa(g.Round) || cards[card] {
				return false
			}
			index, err := strconv.Atoi(parts[2])
			if err != nil || index < 0 || index >= 6 || (parts[0] == "A" && index >= 2) || !strings.Contains("|J|Q|K|A|", "|"+parts[0]+"|") {
				return false
			}
			cards[card] = true
			return true
		}
		for _, id := range g.Players {
			e, ok := state.Entities[id]
			life, hasLife := g.Lives[id]
			if seen[id] || !ok || !hasLife || life < 0 || life > 6 || (life > 0 && (!e.Alive || e.Status != "active" || e.Location != scene.Focus)) || (life == 0 && e.Alive) {
				return scene, errors.New("card participant state mismatch")
			}
			seen[id] = true
			if len(g.Hands[id]) > 4 {
				return scene, errors.New("invalid card hand size")
			}
			for _, card := range g.Hands[id] {
				if !checkCard(card) {
					return scene, errors.New("invalid or duplicated card")
				}
			}
		}
		for id := range g.Hands {
			if !seen[id] {
				return scene, errors.New("unknown hand owner")
			}
		}
		if g.Pending != nil {
			p := g.Pending
			if !seen[p.Actor] || g.Lives[p.Actor] == 0 || len(p.Cards) < 1 || len(p.Cards) > 3 || p.Claim != g.TableRank || p.Actor == g.Players[g.Current] {
				return scene, errors.New("invalid pending card play")
			}
			for _, card := range p.Cards {
				if !checkCard(card) {
					return scene, errors.New("invalid pending card")
				}
			}
		}
		alive := gameAlive(g)
		if len(alive) == 0 || g.Lives[g.Players[g.Current]] == 0 || (len(alive) == 1 && g.Winner != alive[0]) || (len(alive) > 1 && g.Winner != "") {
			return scene, errors.New("invalid card winner or current actor")
		}
	}
	return scene, nil
}

func sceneValue(scene SceneState) map[string]any {
	b, _ := json.Marshal(scene)
	var result map[string]any
	_ = json.Unmarshal(b, &result)
	return result
}

func occupants(state *engine.WorldState, location string) []string {
	ids := []string{}
	for id, e := range state.Entities {
		if e.Alive && e.Status == "active" && e.Location == location {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func observeFor(state *engine.WorldState, actors []string, text string) []engine.Occurrence {
	result := []engine.Occurrence{}
	for _, id := range actors {
		e, ok := state.Entities[id]
		if ok && e.Alive && e.Status == "active" && e.Location != "" {
			result = append(result, engine.Occurrence{Kind: "observation", Actor: id, Content: text, Location: e.Location, Audience: []string{id}})
		}
	}
	return result
}

func deal(game *CardGame) {
	game.Round++
	ranks := []string{"J", "Q", "K"}
	game.TableRank = ranks[(game.Round-1)%len(ranks)]
	deck := []string{}
	for _, rank := range ranks {
		for i := 0; i < 6; i++ {
			deck = append(deck, fmt.Sprintf("%s-%d-%d", rank, game.Round, i))
		}
	}
	for i := 0; i < 2; i++ {
		deck = append(deck, fmt.Sprintf("A-%d-%d", game.Round, i))
	}
	r := rand.New(rand.NewSource(game.Seed + int64(game.Round)*7919))
	r.Shuffle(len(deck), func(i, j int) { deck[i], deck[j] = deck[j], deck[i] })
	game.Hands = map[string][]string{}
	offset := 0
	for _, id := range game.Players {
		if game.Lives[id] > 0 {
			game.Hands[id] = append([]string{}, deck[offset:offset+4]...)
			offset += 4
		}
	}
}

func nextAlive(game *CardGame, start int) int {
	for i := 1; i <= len(game.Players); i++ {
		index := (start + i) % len(game.Players)
		if game.Lives[game.Players[index]] > 0 {
			return index
		}
	}
	return start
}
func gameAlive(game *CardGame) []string {
	ids := []string{}
	for _, id := range game.Players {
		if game.Lives[id] > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func gameObservations(state *engine.WorldState, game *CardGame, publicText string) []engine.Occurrence {
	result := observeFor(state, game.Players, publicText)
	for _, id := range game.Players {
		if game.Lives[id] <= 0 {
			continue
		}
		data, _ := json.Marshal(map[string]any{"table_rank": game.TableRank, "hand": game.Hands[id], "lives": game.Lives[id], "next_actor": game.Players[game.Current], "pending": game.Pending != nil, "round": game.Round})
		frames := observeFor(state, []string{id}, "你的牌局可见状态："+string(data))
		for i := range frames {
			frames[i].ContextSlot = "game_state"
		}
		result = append(result, frames...)
	}
	return result
}

// Card adjudication consumes only world rules/state and submitted intentions.
// It never opens character cognition, creates beliefs, or calls a model.
func adjudicateCards(state *engine.WorldState, scene *SceneState, intent CharacterIntent) ([]engine.Change, []engine.Occurrence, error) {
	g := scene.Game
	if g.Winner != "" {
		return nil, nil, errors.New("card game already ended")
	}
	actor := g.Players[g.Current]
	if intent.ActorID != actor || g.Lives[actor] <= 0 {
		return nil, nil, errors.New("not the current card player")
	}
	public := ""
	changes := []engine.Change{}
	lose := func(id string) {
		g.Lives[id]--
		if g.Lives[id] == 0 {
			changes = append(changes, engine.Change{Path: "entities." + id + ".alive", Op: "set", Value: false}, engine.Change{Path: "entities." + id + ".status", Op: "set", Value: "dead"})
		}
	}
	switch intent.Action {
	case "play":
		if g.Pending != nil || intent.Claim != g.TableRank || len(intent.Cards) < 1 || len(intent.Cards) > 3 {
			return nil, nil, errors.New("play requires 1-3 owned cards and current table claim")
		}
		hand := append([]string{}, g.Hands[actor]...)
		for _, card := range intent.Cards {
			found := -1
			for i, value := range hand {
				if value == card {
					found = i
					break
				}
			}
			if found < 0 {
				return nil, nil, errors.New("played card is not owned")
			}
			hand = append(hand[:found], hand[found+1:]...)
		}
		g.Hands[actor] = hand
		g.Pending = &CardPlay{Actor: actor, Cards: append([]string{}, intent.Cards...), Claim: intent.Claim}
		g.Current = nextAlive(g, g.Current)
		public = fmt.Sprintf("%s盖下%d张牌，宣称均为%s；现在由%s选择accept或challenge。", actor, len(intent.Cards), intent.Claim, g.Players[g.Current])
	case "accept":
		if g.Pending == nil {
			return nil, nil, errors.New("no pending play to accept")
		}
		prior := g.Pending.Actor
		g.Pending = nil
		g.AcceptedWithoutChallenge++
		public = actor + "接受了上一手牌，未查看牌面。"
		if g.AcceptedWithoutChallenge >= g.MaxAccepts {
			lose(prior)
			g.AcceptedWithoutChallenge = 0
			public += fmt.Sprintf("连续%d次无人质疑，按公开规则%s失去一条生命。", g.MaxAccepts, prior)
		}
		if len(g.Hands[prior]) == 0 || g.Lives[prior] == 0 {
			deal(g)
			public += "进入新一轮并重新发牌。"
		}
	case "challenge":
		if g.Pending == nil {
			return nil, nil, errors.New("no pending play to challenge")
		}
		prior := g.Pending
		lie := false
		for _, card := range prior.Cards {
			rank := strings.Split(card, "-")[0]
			if rank != prior.Claim && rank != "A" {
				lie = true
			}
		}
		loser := actor
		if lie {
			loser = prior.Actor
		}
		lose(loser)
		g.Pending = nil
		g.AcceptedWithoutChallenge = 0
		public = fmt.Sprintf("%s质疑%s；公开翻牌为%s，宣称%s，质疑判定为%t。%s失去一条生命。", actor, prior.Actor, strings.Join(prior.Cards, ","), prior.Claim, lie, loser)
		g.Current = nextAlive(g, g.Current)
		deal(g)
		public += "重新发牌。"
	default:
		return nil, nil, errors.New("current card turn requires play, accept or challenge")
	}
	alive := gameAlive(g)
	if len(alive) == 1 {
		g.Winner = alive[0]
		public += "唯一幸存者是" + g.Winner + "。"
	} else if g.Lives[g.Players[g.Current]] == 0 {
		g.Current = nextAlive(g, g.Current)
	}
	return changes, gameObservations(state, g, public), nil
}
