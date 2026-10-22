package characteragent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"worldsim/internal/engine"
	"worldsim/internal/narrative"
)

// SchedulePressure is an explicit author command. Rationale stays in the author
// ledger; only the actual visible environment condition reaches characters.
func SchedulePressure(ctx context.Context, dir string, e *engine.StateEngine, p WorldPressure) error {
	state, err := e.Snapshot()
	if err != nil {
		return err
	}
	scene, err := readScene(state)
	if err != nil {
		return err
	}
	if !cognitionID(p.ID) || len(p.ID) > 128 || strings.TrimSpace(p.Description) == "" || len(p.Description) > 2000 || strings.TrimSpace(p.Rationale) == "" || len(p.Rationale) > 2000 || p.Applied || p.AppliedTurn != 0 || p.Outcome != "" || len(p.Actors) == 0 {
		return errors.New("pressure requires ID, visible condition, author rationale and affected actors")
	}
	if p.Kind != "opportunity" && p.Kind != "deadline" && p.Kind != "resource_cost" {
		return errors.New("unsupported environment pressure")
	}
	if p.DueTurn < scene.Turn+1 || p.DueTurn > scene.Turn+20 || p.Cost < 0 || p.Cost > 1000 || (p.Kind == "opportunity" && p.Cost != 0) || (p.Kind != "opportunity" && p.Cost == 0) {
		return errors.New("invalid pressure deadline or resource cost")
	}
	if scene.Game != nil && scene.Game.Winner != "" {
		return errors.New("cannot pressure a completed game")
	}
	for _, prior := range scene.Pressures {
		if prior.ID == p.ID {
			return errors.New("pressure ID already exists")
		}
	}
	if len(scene.Pressures) >= 64 {
		return errors.New("pressure ledger limit reached")
	}
	sources, err := narrative.ReadJournal(dir)
	if err != nil {
		return err
	}
	found := false
	for _, entry := range sources {
		if entry.Revision <= state.Revision && fmt.Sprintf("event:%d:%s", entry.Revision, entry.CommandID) == p.SourceID {
			found = true
		}
	}
	if !found {
		return errors.New("pressure must reference a committed source event")
	}
	seen := map[string]bool{}
	for _, id := range p.Actors {
		entity, ok := state.Entities[id]
		if !ok || !entity.Alive || entity.Status != "active" || entity.Location != scene.Focus || seen[id] || strings.Contains(id, ".") {
			return errors.New("invalid pressure audience")
		}
		seen[id] = true
	}
	sort.Strings(p.Actors)
	scene.Pressures = append(scene.Pressures, p)
	scene.StalledTurns = 0
	scene.NextWake = append([]string{}, p.Actors...)
	text := fmt.Sprintf("环境公告：%s。生效回合：%d；届时每名公告对象支付%d资源，余额不足则扣至0。", p.Description, p.DueTurn, p.Cost)
	if p.Kind == "opportunity" {
		text = "环境中出现一条可见提示：" + p.Description + "。是否采信或行动由你决定。"
	}
	announcements := observeFor(state, p.Actors, text)
	for i := range announcements {
		announcements[i].Persistent = true
	}
	return e.Submit(ctx, &engine.Proposal{CommandID: "pressure-schedule-" + p.ID, ActorID: "world_agent", BaseRevision: state.Revision, Type: "adjudication", SourceEvents: []string{p.SourceID}, Reason: p.Rationale, Changes: []engine.Change{{Path: "world_level.runtime", Op: "set", Value: sceneValue(scene)}}, Occurrences: announcements})
}

// applyDuePressures changes only announced resources and emits real receipts.
func (r *IndependentRuntime) applyDuePressures(ctx context.Context) error {
	state, err := r.engine.Snapshot()
	if err != nil {
		return err
	}
	scene, err := readScene(state)
	if err != nil {
		return err
	}
	changes := []engine.Change{}
	events := []engine.Occurrence{}
	sources := []string{}
	balances := map[string]float64{}
	applied := false
	wake := map[string]bool{}
	for _, id := range scene.NextWake {
		wake[id] = true
	}
	for i := range scene.Pressures {
		p := &scene.Pressures[i]
		if p.Applied || p.DueTurn > scene.Turn+1 {
			continue
		}
		applied = true
		p.Applied = true
		p.AppliedTurn = scene.Turn + 1
		p.Outcome = "环境条件已生效；角色行为仍由角色自主决定"
		for _, id := range p.Actors {
			entity := state.Entities[id]
			if !entity.Alive || entity.Status != "active" {
				continue
			}
			balance, ok := balances[id]
			if !ok {
				balance = entity.Money
			}
			amount := float64(p.Cost)
			if amount > balance {
				amount = balance
			}
			if amount < 0 {
				amount = 0
			}
			balances[id] = balance - amount
			events = append(events, observeFor(state, []string{id}, fmt.Sprintf("环境条件%s已生效：你支付%.0f资源，余额%.0f。", p.ID, amount, balances[id]))...)
			wake[id] = true
		}
		sources = append(sources, p.SourceID)
	}
	if !applied {
		return nil
	}
	ids := []string{}
	for id := range balances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if balances[id] != state.Entities[id].Money {
			changes = append(changes, engine.Change{Path: "entities." + id + ".money", Op: "set", Value: balances[id]})
		}
	}
	scene.NextWake = []string{}
	for id := range wake {
		scene.NextWake = append(scene.NextWake, id)
	}
	sort.Strings(scene.NextWake)
	scene.StalledTurns = 0
	changes = append(changes, engine.Change{Path: "world_level.runtime", Op: "set", Value: sceneValue(scene)})
	if err := r.engine.Submit(ctx, &engine.Proposal{CommandID: fmt.Sprintf("pressure-apply-%d", state.Revision), ActorID: "world_agent", BaseRevision: state.Revision, Type: "adjudication", SourceEvents: sources, Reason: "apply previously announced environment conditions", Changes: changes, Occurrences: events}); err != nil {
		return err
	}
	return r.reconcile()
}
