package characteragent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"worldsim/internal/engine"
)

type LocalObservationPacket struct {
	Owner         CognitionOwner `json:"owner"`
	WorldRevision int            `json:"world_revision"`
	Observations  []Observation  `json:"observations"`
}

type DeliveryReport struct {
	Added          int  `json:"added"`
	Unchanged      int  `json:"unchanged"`
	Recipients     int  `json:"recipients"`
	IgnoredEntries int  `json:"ignored_entries"`
	MissingJournal bool `json:"missing_journal"`
}

// VisibilityProjector is trusted world-side code. Only its actor-scoped packets
// may be passed to characters; its input journal is never character context.
type VisibilityProjector struct{ registry *AgentRegistry }

func NewVisibilityProjector(worldDir string) (*VisibilityProjector, error) {
	r, err := NewRegistry(worldDir)
	if err != nil {
		return nil, err
	}
	return &VisibilityProjector{registry: r}, nil
}

func (p *VisibilityProjector) packetsLocked() (map[string]LocalObservationPacket, DeliveryReport, error) {
	bad := errors.New("cannot project journal: invalid metadata, evidence kind or explicit audience")
	d, exists, err := p.registry.load()
	if err != nil || !exists {
		return nil, DeliveryReport{}, errors.New("observation projection requires registered world")
	}
	packets := map[string]LocalObservationPacket{}
	for id, a := range d.Agents {
		packets[id] = LocalObservationPacket{Owner: CognitionOwner{WorldID: d.WorldID, AgentID: a.AgentID, CharacterID: id}, WorldRevision: d.WorldRevision, Observations: []Observation{}}
	}
	report := DeliveryReport{}
	data, err := readPrivateFile(p.registry.dir, "event_log.jsonl") // bounded, path-checked file read only
	if os.IsNotExist(err) {
		report.MissingJournal = true
		return packets, report, nil
	}
	if err != nil {
		return nil, report, errors.New("cannot read committed observation journal")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		return nil, report, errors.New("journal has incomplete tail; preserve for recovery")
	}
	last, through := 0, 0
	commands := map[string]bool{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// Parse only journal records, never character memory or author documents.
		if checkUniqueJSON(line) != nil {
			return nil, report, bad
		}
		var e engine.LogEntry
		if json.Unmarshal(line, &e) != nil || e.Revision < 1 || !cognitionID(e.CommandID) || commands[e.CommandID] || (last > 0 && e.Revision != last+1) {
			return nil, report, bad
		}
		last = e.Revision
		commands[e.CommandID] = true
		if e.Revision > d.WorldRevision {
			continue
		} // journal append may precede state publication
		through = e.Revision
		if len(e.Occurrences) == 0 {
			report.IgnoredEntries++
			continue
		}
		for index, occurrence := range e.Occurrences {
			if !cognitionText(occurrence.Content) || !cognitionText(occurrence.Location) || len(occurrence.Audience) == 0 {
				return nil, report, bad
			}
			if _, ok := d.Agents[occurrence.Actor]; !ok {
				return nil, report, bad
			}
			kind := ""
			switch occurrence.Kind {
			case "observation":
				if len(occurrence.Audience) != 1 || occurrence.Audience[0] != occurrence.Actor {
					return nil, report, bad
				}
				kind = "direct_observation"
			case "utterance":
				kind = "public_statement" // a statement to named listeners, not globally public truth
			default:
				return nil, report, bad
			}
			audience := map[string]bool{}
			for _, actor := range occurrence.Audience {
				packet, ok := packets[actor]
				if !ok || audience[actor] {
					return nil, report, bad
				}
				audience[actor] = true
				eventID := fmt.Sprintf("event:%d:%s", e.Revision, e.CommandID)
				if !cognitionID(eventID) || !cognitionID(occurrence.Actor) {
					return nil, report, bad
				}
				id := "obs-" + privateHash([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", d.WorldID, eventID, index, actor)))
				observation := Observation{ID: id, Recipient: packet.Owner, Kind: kind, Text: occurrence.Content, EventID: eventID, SourceID: fmt.Sprintf("journal:%s:%d", privateHash(line), index), WorldRevision: e.Revision, Location: occurrence.Location}
				if occurrence.Kind == "utterance" {
					observation.Speaker = occurrence.Actor
				}
				packet.Observations = append(packet.Observations, observation)
				packets[actor] = packet
			}
		}
	}
	if last > 0 && through != d.WorldRevision {
		return nil, report, errors.New("journal does not cover registered world revision")
	}
	if last == 0 && d.WorldRevision > 0 {
		report.MissingJournal = true
	}
	return packets, report, nil
}

// Preview reads only; it returns exactly one actor's explicit receipts.
func (p *VisibilityProjector) Preview(characterID string) (LocalObservationPacket, error) {
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	packets, _, err := p.packetsLocked()
	if err != nil {
		return LocalObservationPacket{}, err
	}
	packet, ok := packets[characterID]
	if !ok {
		return LocalObservationPacket{}, errors.New("unknown observation recipient")
	}
	return packet, nil
}

type pendingDelivery struct {
	store        *CognitionStore
	revision     int
	observations []Observation
}

func (p *VisibilityProjector) deliver(characterID string, all bool) (DeliveryReport, error) {
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	packets, report, err := p.packetsLocked()
	if err != nil {
		return report, err
	}
	ids := []string{}
	if all {
		for id := range packets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	} else {
		if _, ok := packets[characterID]; !ok {
			return report, errors.New("unknown observation recipient")
		}
		ids = append(ids, characterID)
	}
	pending := []pendingDelivery{}
	for _, id := range ids {
		packet := packets[id]
		s := &CognitionStore{registry: p.registry, owner: packet.Owner}
		state, err := s.readLocked()
		if err != nil {
			return report, err
		}
		expected := map[string]Observation{}
		for _, o := range packet.Observations {
			expected[o.ID] = o
		}
		existing := map[string]Observation{}
		for _, o := range state.Observations {
			if strings.HasPrefix(o.SourceID, "journal:") {
				want, ok := expected[o.ID]
				o.AddedRevision = 0
				if !ok || !reflect.DeepEqual(o, want) {
					return report, errors.New("projected observation source changed or left current journal branch; preserve cognition for explicit recovery")
				}
			}
			existing[o.ID] = o
		}
		missing := []Observation{}
		for _, o := range packet.Observations {
			if old, ok := existing[o.ID]; ok {
				// Also reject a non-journal record trying to occupy a deterministic ID.
				old.AddedRevision = 0
				if !reflect.DeepEqual(old, o) {
					return report, errors.New("observation identifier collision")
				}
				report.Unchanged++
			} else {
				missing = append(missing, o)
			}
		}
		if len(missing) == 0 {
			continue
		}
		// Validate every actor before any actor is written; remaining failures can
		// only be I/O failures, recoverable by re-delivering missing receipts.
		candidate := state
		candidate.Revision++
		candidate.Observations = append([]Observation{}, state.Observations...)
		for _, o := range missing {
			o.AddedRevision = candidate.Revision
			candidate.Observations = append(candidate.Observations, o)
		}
		if err := s.validateLocked(candidate); err != nil {
			return report, err
		}
		encoded, err := json.MarshalIndent(candidate, "", "  ")
		if err != nil || len(encoded) > maxCognitionBytes {
			return report, errors.New("projected cognition exceeds storage limits")
		}
		pending = append(pending, pendingDelivery{store: s, revision: state.Revision, observations: missing})
	}
	for _, next := range pending {
		if _, err := next.store.appendLocked(next.revision, next.observations, nil); err != nil {
			return report, err
		}
		report.Added += len(next.observations)
		report.Recipients++
	}
	return report, nil
}

func (p *VisibilityProjector) Deliver(characterID string) (DeliveryReport, error) {
	return p.deliver(characterID, false)
}
func (p *VisibilityProjector) DeliverAll() (DeliveryReport, error) { return p.deliver("", true) }
