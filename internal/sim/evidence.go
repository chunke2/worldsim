package sim

import (
	"context"
	"fmt"
	"sort"
	"worldsim/internal/engine"
)

// commitEvidence records what was observed/said, without treating its content as truth.
func (s *Simulator) commitEvidence(ctx context.Context, kind, actor, content, location string, audience []string) (string, error) {
	p := &engine.Proposal{CommandID: s.nextCmd("evidence"), ActorID: "world_agent", BaseRevision: s.engine.State().Revision, Type: "adjudication", Reason: "record " + kind, Occurrences: []engine.Occurrence{{Kind: kind, Actor: actor, Content: content, Location: location, Audience: audience}}}
	if err := s.engine.Submit(ctx, p); err != nil {
		return "", err
	}
	return fmt.Sprintf("event:%d:%s", p.BaseRevision+1, p.CommandID), nil
}

func (s *Simulator) commitObservations(ctx context.Context) error {
	for i := range s.events {
		ev := &s.events[i]
		ev.ObservationSources = map[string]string{}
		for actor := range ev.Observations {
			content := s.observeEvent(actor, *ev)
			if content == "" {
				continue
			}
			id, err := s.commitEvidence(ctx, "observation", actor, content, ev.Location, []string{actor})
			if err != nil {
				return err
			}
			ev.ObservationSources[actor] = id
			s.mem.AddObservation(actor, Observation{SourceEvent: id, Content: content, VisibleTo: actor, Credibility: 0.95, ArrivalDay: s.day, WritableToMem: true})
		}
	}
	return nil
}

// eventEvidenceIDs returns committed event-log IDs, never the generator's
// provisional EventCard ID. State changes may cite only these durable IDs.
func eventEvidenceIDs(events []EventCard) []string {
	seen := map[string]bool{}
	for _, ev := range events {
		for _, id := range ev.ObservationSources {
			if id != "" {
				seen[id] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func actorEventEvidence(actor string, events []EventCard) []string {
	seen := map[string]bool{}
	for _, ev := range events {
		if id := ev.ObservationSources[actor]; id != "" {
			seen[id] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
