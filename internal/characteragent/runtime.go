package characteragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"

	"worldsim/internal/config"
	"worldsim/internal/engine"
)

type TurnOutcome struct {
	ActorID  string `json:"actor_id"`
	Action   string `json:"action"`
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}
type TurnResult struct {
	Snapshot         TurnSnapshot   `json:"snapshot"`
	Outcomes         []TurnOutcome  `json:"outcomes"`
	WorldRevision    int            `json:"world_revision"`
	Delivery         DeliveryReport `json:"delivery"`
	Winner           string         `json:"winner,omitempty"`
	AlreadyCommitted bool           `json:"already_committed"`
}

// IndependentRuntime orchestrates characters. Adjudicate below receives no
// client, store, author plan or character cognition handle.
type IndependentRuntime struct {
	dir       string
	engine    *engine.StateEngine
	policies  config.AgentRuntimeConfig
	providers config.ProviderRegistry
	client    CompletionClient
}

func NewIndependentRuntime(dir string, e *engine.StateEngine, policies config.AgentRuntimeConfig, providers config.ProviderRegistry, client CompletionClient) (*IndependentRuntime, error) {
	if e == nil || client == nil {
		return nil, errors.New("independent runtime requires engine and client")
	}
	if err := policies.Validate(providers); err != nil {
		return nil, err
	}
	return &IndependentRuntime{dir: dir, engine: e, policies: policies, providers: providers, client: client}, nil
}

func (r *IndependentRuntime) reconcile() error {
	state, err := r.engine.Snapshot()
	if err != nil {
		return err
	}
	registry, err := NewRegistry(r.dir)
	if err != nil {
		return err
	}
	_, err = registry.Reconcile(state)
	return err
}

func (r *IndependentRuntime) EnsureProfiles() error {
	if err := r.reconcile(); err != nil {
		return err
	}
	state, err := r.engine.Snapshot()
	if err != nil {
		return err
	}
	for id, entity := range state.Entities {
		if strings.Contains(id, ".") {
			return errors.New("character ID contains a path separator unsupported by state engine")
		}
		if !entity.Alive || entity.Status != "active" {
			continue
		}
		s, err := NewCognitionStore(r.dir, id)
		if err != nil {
			return err
		}
		c, err := s.Read()
		if err != nil {
			return err
		}
		exists := false
		for _, o := range c.Observations {
			if o.SourceID == "bootstrap:self-profile" {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		profile := map[string]any{"name": id, "location": entity.Location, "health": entity.Health, "money": entity.Money, "job": entity.Job}
		for _, key := range []string{"persona", "personality", "appearance", "background", "motivation", "goal", "fear", "bias", "abilities"} {
			if value, ok := entity.Extra[key].(string); ok {
				profile[key] = value
			}
		}
		if raw, ok := entity.Extra["persona_sheet"].(string); ok && raw != "" {
			var sheet map[string]json.RawMessage
			if checkUniqueJSON([]byte(raw)) != nil || json.Unmarshal([]byte(raw), &sheet) != nil {
				return errors.New("character self-profile is malformed; preserve it for repair")
			}
			var name string
			if json.Unmarshal(sheet["name"], &name) != nil || name != id {
				return errors.New("character self-profile identity does not match entity")
			}
			for _, key := range []string{"age", "identity", "secret"} {
				var value string
				if json.Unmarshal(sheet[key], &value) == nil && value != "" {
					profile["self_"+key] = value
				}
			}
			for _, key := range []string{"personality", "habits", "social", "behavior", "thinking", "motives", "fears"} {
				var value []string
				if json.Unmarshal(sheet[key], &value) == nil {
					profile["self_"+key] = value
				}
			}
		}
		b, _ := json.Marshal(profile)
		if _, err := s.Append(c.Revision, []Observation{{ID: "self-" + privateHash(b), Recipient: s.Owner(), Kind: "profile", Text: string(b), SourceID: "bootstrap:self-profile", WorldRevision: state.Revision}}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (r *IndependentRuntime) InitializeFree(ctx context.Context) error {
	state, err := r.engine.Snapshot()
	if err != nil {
		return err
	}
	if len(state.WorldLevel.Runtime) > 0 {
		_, err := readScene(state)
		return err
	}
	hero := state.WorldLevel.Protagonist
	entity, ok := state.Entities[hero]
	if !ok || entity.Location == "" {
		return errors.New("independent scene requires a stable protagonist and location")
	}
	scene := SceneState{SchemaVersion: 1, Kind: "free", Focus: entity.Location}
	occurrences := []engine.Occurrence{}
	ids := make([]string, 0, len(state.Entities))
	for id := range state.Entities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := state.Entities[id]
		if !e.Alive || e.Status != "active" {
			continue
		}
		text := fmt.Sprintf("你位于%s；同处此地的人物有%s。你可以自主观察、等待、说话、移动到已知且开放的地点，或向在场人物赠予自己的钱。", e.Location, strings.Join(occupants(state, e.Location), "、"))
		occurrences = append(occurrences, observeFor(state, []string{id}, text)...)
	}
	if err := r.engine.Submit(ctx, &engine.Proposal{CommandID: "independent-scene-init", ActorID: "world_agent", BaseRevision: state.Revision, Type: "adjudication", Changes: []engine.Change{{Path: "world_level.runtime", Op: "set", Value: sceneValue(scene)}}, Occurrences: occurrences}); err != nil {
		return err
	}
	return r.reconcile()
}

func (r *IndependentRuntime) InitializeCards(ctx context.Context, seed int64, lives int) error {
	state, err := r.engine.Snapshot()
	if err != nil {
		return err
	}
	if len(state.WorldLevel.Runtime) > 0 {
		return errors.New("scene already initialized; create a new world for a different scenario")
	}
	if lives < 1 || lives > 6 {
		return errors.New("card lives must be within 1..6")
	}
	ids := occupants(state, state.Entities[state.WorldLevel.Protagonist].Location)
	if len(ids) != 5 || state.Entities[state.WorldLevel.Protagonist].Location == "" {
		return errors.New("card scenario requires five active people in the protagonist room")
	}
	g := &CardGame{Players: ids, Lives: map[string]int{}, Seed: seed, MaxAccepts: 3}
	for _, id := range ids {
		g.Lives[id] = lives
	}
	deal(g)
	scene := SceneState{SchemaVersion: 1, Kind: "liars_room", Focus: state.Entities[ids[0]].Location, Game: g}
	rules := fmt.Sprintf("五人死亡牌局开始。每人%d条生命；桌面牌型%s，A为万能牌。当前行动者没有待质疑牌时须play自己手中1-3张牌，claim必须等于桌面牌型，可以说谎；下一位须accept或challenge。质疑时公开翻牌：出牌者说谎则出牌者扣命，否则质疑者扣命；0命出局。连续3次accept，最近出牌者扣命。一次质疑或有人出完牌后重新发牌。最后唯一活着的人获胜。", lives, g.TableRank)
	initialEvents := gameObservations(state, g, rules)
	for i := range initialEvents {
		if initialEvents[i].Content == rules {
			initialEvents[i].Persistent = true
		}
	}
	if err := r.engine.Submit(ctx, &engine.Proposal{CommandID: "independent-card-init", ActorID: "world_agent", BaseRevision: state.Revision, Type: "adjudication", Changes: []engine.Change{{Path: "world_level.runtime", Op: "set", Value: sceneValue(scene)}}, Occurrences: initialEvents}); err != nil {
		return err
	}
	return r.reconcile()
}

func selectActors(state *engine.WorldState, scene SceneState, c config.AgentRuntimeConfig, registry Document) ([]string, error) {
	if scene.Kind == "liars_room" {
		if scene.Game.Winner != "" {
			return []string{}, nil
		}
		id := scene.Game.Players[scene.Game.Current]
		if !state.Entities[id].Alive || registry.Agents[id].State != "active" {
			return nil, errors.New("invalid current game actor")
		}
		return []string{id}, nil
	}
	ids := []string{}
	for id, e := range state.Entities {
		if !e.Alive || e.Status != "active" || e.Location != scene.Focus {
			continue
		}
		if _, ok := c.Characters[id]; !ok {
			return nil, errors.New("active character is missing runtime policy")
		}
		policy := c.Tiers[c.Characters[id].Importance]
		// All present people observe this initial scene. Lower tiers subsequently
		// wake only when a direct receipt arrived since the preceding turn.
		if policy.WakePolicy == "on_direct_event" || c.Characters[id].Importance == config.ImportanceBackground {
			if scene.Turn > 0 {
				direct := false
				for _, recipient := range scene.NextWake {
					if recipient == id {
						direct = true
					}
				}
				if !direct {
					continue
				}
			}
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (r *IndependentRuntime) RunTurn(ctx context.Context) (TurnResult, error) {
	if err := r.recoverTail(); err != nil {
		return TurnResult{}, err
	}
	if err := r.InitializeFree(ctx); err != nil {
		return TurnResult{}, err
	}
	if err := r.applyDuePressures(ctx); err != nil {
		return TurnResult{}, err
	}
	if err := r.EnsureProfiles(); err != nil {
		return TurnResult{}, err
	}
	p, err := NewVisibilityProjector(r.dir)
	if err != nil {
		return TurnResult{}, err
	}
	if _, err := p.DeliverAll(); err != nil {
		return TurnResult{}, err
	}
	state, err := r.engine.Snapshot()
	if err != nil {
		return TurnResult{}, err
	}
	scene, err := readScene(state)
	if err != nil {
		return TurnResult{}, err
	}
	registry, _ := NewRegistry(r.dir)
	if scene.StalledTurns >= 3 {
		return TurnResult{}, errors.New("narrative stagnation: three turns without effective world change; add an evidenced world pressure before continuing")
	}
	doc, _, err := registry.Read()
	if err != nil {
		return TurnResult{}, err
	}
	for id, entry := range doc.Agents {
		if _, exists := r.policies.Characters[id]; !exists {
			r.policies.Characters[id] = config.CharacterAgentConfig{Importance: entry.Importance}
		}
	}
	actors, err := selectActors(state, scene, r.policies, doc)
	if err != nil {
		return TurnResult{}, err
	}
	if len(actors) == 0 {
		winner := ""
		if scene.Game != nil {
			winner = scene.Game.Winner
		}
		if winner != "" {
			return TurnResult{Winner: winner, WorldRevision: state.Revision}, nil
		}
		return TurnResult{}, errors.New("no relevant active characters")
	}
	turn := TurnSnapshot{ID: "turn-" + thoughtID(CognitionOwner{WorldID: doc.WorldID, AgentID: "world"}, fmt.Sprintf("%d:%d", state.Revision, scene.Turn)), WorldRevision: state.Revision, Mode: "free", Actors: actors}
	if scene.Kind == "liars_room" {
		turn.Mode = "turn_based"
	}
	intents := make([]CharacterIntent, len(actors))
	evidence := make([][]string, len(actors))
	failures := make([]error, len(actors))
	workers := make(chan struct{}, 2)
	tierGates := map[config.AgentImportance]chan struct{}{}
	for tier, policy := range r.policies.Tiers {
		limit := policy.MaxParallel
		if limit <= 0 || limit > 2 {
			limit = 2
		}
		tierGates[tier] = make(chan struct{}, limit)
	}
	var wg sync.WaitGroup
	for index, id := range actors {
		wg.Add(1)
		go func(index int, id string) {
			defer wg.Done()
			gate := tierGates[r.policies.Characters[id].Importance]
			select {
			case gate <- struct{}{}:
				defer func() { <-gate }()
			case <-ctx.Done():
				failures[index] = ctx.Err()
				return
			}
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				failures[index] = ctx.Err()
				return
			}
			spec, err := ResolveCharacterModel(r.policies, r.providers, id)
			if err != nil {
				failures[index] = err
				return
			}
			spec.RequireIntent = true
			agent, err := NewCharacterAgent(r.dir, id, r.client)
			if err != nil {
				failures[index] = err
				return
			}
			result, err := agent.Think(ctx, turn.ID, spec)
			if err != nil {
				failures[index] = err
				return
			}
			if result.Receipt.Intent == nil {
				failures[index] = errors.New("character returned no explicit action intent")
				return
			}
			intents[index] = *result.Receipt.Intent
			evidence[index] = result.Receipt.EvidenceEventIDs
		}(index, id)
	}
	wg.Wait()
	for _, err := range failures {
		if err != nil {
			return TurnResult{Snapshot: turn}, fmt.Errorf("independent turn not committed: %w", err)
		}
	}
	proposal, outcomes, err := Adjudicate(state, turn, intents)
	if err != nil {
		return TurnResult{Snapshot: turn}, err
	}
	sources := map[string]bool{}
	for _, ids := range evidence {
		for _, id := range ids {
			sources[id] = true
		}
	}
	for id := range sources {
		proposal.SourceEvents = append(proposal.SourceEvents, id)
	}
	sort.Strings(proposal.SourceEvents)
	if err := r.engine.Submit(ctx, proposal); err != nil {
		return TurnResult{Snapshot: turn}, err
	}
	if err := r.reconcile(); err != nil {
		return TurnResult{Snapshot: turn}, err
	}
	delivery, err := p.DeliverAll()
	if err != nil {
		return TurnResult{Snapshot: turn, WorldRevision: state.Revision + 1}, fmt.Errorf("world committed; observation delivery needs retry: %w", err)
	}
	updated, _ := r.engine.Snapshot()
	after, _ := readScene(updated)
	winner := ""
	if after.Game != nil {
		winner = after.Game.Winner
	}
	return TurnResult{Snapshot: turn, Outcomes: outcomes, WorldRevision: updated.Revision, Delivery: delivery, Winner: winner}, nil
}

func (r *IndependentRuntime) recoverTail() error {
	b, err := readPrivateFile(r.dir, "event_log.jsonl")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("cannot inspect committed runtime journal")
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		return errors.New("runtime journal has incomplete tail")
	}
	previous := 0
	commands := map[string]bool{}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e engine.LogEntry
		if checkUniqueJSON(line) != nil || json.Unmarshal(line, &e) != nil || e.Revision < 1 || (previous > 0 && e.Revision != previous+1) || commands[e.CommandID] {
			return errors.New("runtime journal is invalid; preserve files for recovery")
		}
		previous = e.Revision
		commands[e.CommandID] = true
	}
	return r.engine.RecoverCommittedJournal()
}

// Adjudicate is deterministic: it accepts intents, never writes their words for
// them, and exposes no model/cognition dependency.
func Adjudicate(state *engine.WorldState, turn TurnSnapshot, intents []CharacterIntent) (*engine.Proposal, []TurnOutcome, error) {
	if state.Revision != turn.WorldRevision || len(intents) != len(turn.Actors) {
		return nil, nil, errors.New("stale or incomplete intent batch")
	}
	scene, err := readScene(state)
	if err != nil {
		return nil, nil, err
	}
	if scene.LastTurnID == turn.ID {
		return nil, nil, errors.New("turn already adjudicated")
	}
	changes := []engine.Change{}
	occurrences := []engine.Occurrence{}
	outcomes := []TurnOutcome{}
	seen := map[string]bool{}
	wake := map[string]bool{}
	money := map[string]float64{}
	for id, e := range state.Entities {
		money[id] = e.Money
	}
	for index, intent := range intents {
		id := turn.Actors[index]
		e, ok := state.Entities[id]
		if !ok || !e.Alive || e.Status != "active" || intent.ActorID != id || intent.TurnID != turn.ID || seen[id] {
			return nil, nil, errors.New("intent actor does not match activation")
		}
		seen[id] = true
		accepted := true
		visible := map[string]bool{}
		for _, source := range intent.Sources {
			visible[source] = true
		}
		if err := validateIntent(intent, CognitionOwner{CharacterID: id}, turn.ID, visible); err != nil {
			return nil, nil, err
		}
		reason := ""
		localChanges := []engine.Change{}
		localEvents := []engine.Occurrence{}
		speechAudience := append([]string{}, intent.Audience...)
		if intent.Speech != "" {
			if len(speechAudience) == 0 {
				speechAudience = occupants(state, e.Location)
			}
			selfIncluded := false
			for _, recipient := range speechAudience {
				if recipient == id {
					selfIncluded = true
				}
				target, ok := state.Entities[recipient]
				if !ok || !target.Alive || target.Status != "active" || target.Location != e.Location {
					accepted = false
					reason = "交谈对象不在场"
				}
			}
			if !selfIncluded {
				speechAudience = append(speechAudience, id)
			}
		}
		if intent.Preconditions.Location != "" && intent.Preconditions.Location != e.Location {
			accepted = false
			reason = "行动所需位置不满足"
		}
		if minimum := intent.Preconditions.MinimumMoney; minimum != nil && (math.IsNaN(*minimum) || money[id] < *minimum) {
			accepted = false
			reason = "行动所需资源不满足"
		}
		if intent.Preconditions.TargetAlive && !state.Entities[intent.Target].Alive {
			accepted = false
			reason = "目标已不可行动"
		}
		if accepted && scene.Kind == "liars_room" {
			localChanges, localEvents, err = adjudicateCards(state, &scene, intent)
			if err != nil {
				accepted = false
				reason = err.Error()
			}
		}
		if accepted && scene.Kind == "free" {
			switch intent.Action {
			case "wait":
				localEvents = observeFor(state, []string{id}, "你选择等待，本轮未执行其他动作。")
			case "observe":
				localEvents = observeFor(state, []string{id}, fmt.Sprintf("你观察%s，在场者为%s。", e.Location, strings.Join(occupants(state, e.Location), "、")))
			case "speak":
				audience := speechAudience
				if len(audience) == 0 {
					audience = occupants(state, e.Location)
				}
				for _, recipient := range audience {
					target, ok := state.Entities[recipient]
					if !ok || !target.Alive || target.Location != e.Location {
						accepted = false
						reason = "交谈对象不在场"
					}
				}
				if strings.TrimSpace(intent.Speech) == "" {
					accepted = false
					reason = "没有提交发言"
				}
				if accepted {
					localEvents = []engine.Occurrence{{Kind: "utterance", Actor: id, Content: intent.Speech, Location: e.Location, Audience: audience}}
				}
			case "move":
				location, ok := state.WorldLevel.Locations[intent.Destination]
				if !ok || intent.Destination == e.Location || location.State == "封禁" || location.State == "毁坏" {
					accepted = false
					reason = "目的地未知或不可进入"
				} else {
					localChanges = append(localChanges, engine.Change{Path: "entities." + id + ".location", Op: "set", Value: intent.Destination})
					localEvents = observeFor(state, occupants(state, e.Location), id+"离开了此地。")
					localEvents = append(localEvents, observeFor(state, []string{id}, "你已完成移动，当前位置是"+intent.Destination+"。")...)
					if id == state.WorldLevel.Protagonist {
						scene.Focus = intent.Destination
					}
				}
			case "give":
				target, ok := state.Entities[intent.Target]
				amount := float64(intent.Quantity)
				if !ok || !target.Alive || target.Location != e.Location || intent.Target == id || intent.Quantity < 1 || money[id] < amount {
					accepted = false
					reason = "赠予的对象或资源不合法"
				} else {
					money[id] -= amount
					money[intent.Target] += amount
					localEvents = observeFor(state, []string{id, intent.Target}, fmt.Sprintf("%s将%d交给%s。", id, intent.Quantity, intent.Target))
				}
			default:
				accepted = false
				reason = "该场景不支持此行动"
			}
		}
		if accepted && intent.Action != "speak" && intent.Speech != "" {
			localEvents = append(localEvents, engine.Occurrence{Kind: "utterance", Actor: id, Content: intent.Speech, Location: e.Location, Audience: speechAudience})
		}
		if !accepted {
			localChanges = nil
			localEvents = observeFor(state, []string{id}, "你的行动未能执行："+reason)
		}
		changes = append(changes, localChanges...)
		occurrences = append(occurrences, localEvents...)
		if accepted && intent.Action != "wait" && intent.Action != "observe" {
			for _, event := range localEvents {
				for _, recipient := range event.Audience {
					if event.Kind != "utterance" || recipient != id {
						wake[recipient] = true
					}
				}
			}
		}
		outcomes = append(outcomes, TurnOutcome{ActorID: id, Action: intent.Action, Accepted: accepted, Reason: reason})
	}
	moneyIDs := []string{}
	for id := range money {
		moneyIDs = append(moneyIDs, id)
	}
	sort.Strings(moneyIDs)
	for _, id := range moneyIDs {
		value := money[id]
		if value != state.Entities[id].Money {
			changes = append(changes, engine.Change{Path: "entities." + id + ".money", Op: "set", Value: value})
		}
	}
	scene.NextWake = []string{}
	for id := range wake {
		scene.NextWake = append(scene.NextWake, id)
	}
	sort.Strings(scene.NextWake)
	scene.Turn++
	effective := false
	for _, outcome := range outcomes {
		if outcome.Accepted && (outcome.Action == "give" || outcome.Action == "move" || outcome.Action == "play" || outcome.Action == "challenge" || outcome.Action == "accept") {
			effective = true
		}
	}
	if effective {
		scene.StalledTurns = 0
	} else {
		scene.StalledTurns++
	}
	scene.LastOutcomes = outcomes
	scene.LastTurnID = turn.ID
	scene.LastTurnRevision = state.Revision + 1
	changes = append(changes, engine.Change{Path: "world_level.runtime", Op: "set", Value: sceneValue(scene)}, engine.Change{Path: "world_level.day", Op: "set", Value: state.Day + 1})
	return &engine.Proposal{CommandID: turn.ID, ActorID: "world_agent", BaseRevision: state.Revision, Type: "adjudication", Changes: changes, Occurrences: occurrences, Reason: "deterministic character-intent adjudication"}, outcomes, nil
}
