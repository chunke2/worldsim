package characteragent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"worldsim/internal/fsutil"
)

const CognitionFile = "cognition.json"
const maxCognitionBytes = 8 << 20

var ErrCognitionConflict = errors.New("cognition revision conflict; reload before retrying")

type CognitionOwner struct {
	WorldID     string `json:"world_id"`
	AgentID     string `json:"agent_id"`
	CharacterID string `json:"character_id"`
}

// Observation is a receipt, not a claim that the reported content is true.
// Trusted delivery code must verify audience and event content before appending.
type Observation struct {
	ID            string         `json:"id"`
	Recipient     CognitionOwner `json:"recipient"`
	Kind          string         `json:"kind"`
	Text          string         `json:"text"`
	SourceID      string         `json:"source_id"`
	EventID       string         `json:"event_id,omitempty"`
	WorldRevision int            `json:"world_revision"`
	Speaker       string         `json:"speaker,omitempty"`
	Location      string         `json:"location,omitempty"`
	AddedRevision int            `json:"added_revision"`
}

// CognitionRecord stores a structured conclusion or intention, never raw CoT.
// Sources refer only to observations or earlier records in this same document.
type CognitionRecord struct {
	ID            string         `json:"id"`
	Owner         CognitionOwner `json:"owner"`
	Kind          string         `json:"kind"`
	Text          string         `json:"text"`
	Sources       []string       `json:"sources"`
	Confidence    *float64       `json:"confidence,omitempty"`
	Status        string         `json:"status"` // active | resolved | abandoned
	Supersedes    string         `json:"supersedes,omitempty"`
	AddedRevision int            `json:"added_revision"`
}

type CognitionState struct {
	SchemaVersion int               `json:"schema_version"`
	Owner         CognitionOwner    `json:"owner"`
	Revision      int               `json:"revision"`
	Observations  []Observation     `json:"observations"`
	Records       []CognitionRecord `json:"records"`
	Thoughts      []ThoughtReceipt  `json:"thoughts,omitempty"`
}

// CurrentRecords excludes superseded records but retains terminal statuses.
// The returned data are detached so a context builder cannot mutate the history.
func (s CognitionState) CurrentRecords() []CognitionRecord {
	superseded := map[string]bool{}
	for _, r := range s.Records {
		superseded[r.Supersedes] = true
	}
	result := []CognitionRecord{}
	for _, r := range s.Records {
		if !superseded[r.ID] {
			r.Sources = append([]string{}, r.Sources...)
			if r.Confidence != nil {
				value := *r.Confidence
				r.Confidence = &value
			}
			result = append(result, r)
		}
	}
	return result
}

// CognitionStore is bound to one registered actor. No global facts, narrative
// plans, provider configuration or other actors' memory are loaded here.
type CognitionStore struct {
	registry *AgentRegistry
	owner    CognitionOwner
}

func NewCognitionStore(worldDir, characterID string) (*CognitionStore, error) {
	r, err := NewRegistry(worldDir)
	if err != nil {
		return nil, err
	}
	d, exists, err := r.Read()
	if err != nil {
		return nil, err
	}
	a, ok := d.Agents[characterID]
	if !exists || !ok {
		return nil, errors.New("cognition requires an existing registered character")
	}
	return &CognitionStore{registry: r, owner: CognitionOwner{WorldID: d.WorldID, AgentID: a.AgentID, CharacterID: characterID}}, nil
}

func (s *CognitionStore) Owner() CognitionOwner { return s.owner }

func (s *CognitionStore) path() string {
	return filepath.Join(s.registry.dir, "agents", s.owner.AgentID, CognitionFile)
}

// checkBindingLocked also rejects a registry replaced with another world's data.
func (s *CognitionStore) checkBindingLocked() error {
	d, exists, err := s.registry.load()
	if err != nil {
		return err
	}
	a, ok := d.Agents[s.owner.CharacterID]
	if !exists || !ok || d.WorldID != s.owner.WorldID || a.AgentID != s.owner.AgentID {
		return errors.New("cognition owner no longer matches registry")
	}
	// Refuse redirected partitions/files, including the atomic writer's temp file.
	for i, path := range []string{filepath.Join(s.registry.dir, "agents"), filepath.Dir(s.path()), s.path(), s.path() + ".tmp"} {
		info, err := os.Lstat(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("cognition paths must not be symbolic links")
		}
		if i < 2 && (err != nil || !info.IsDir()) {
			return errors.New("registered cognition partition is missing or invalid; preserve registry for recovery")
		}
	}
	return nil
}

func cognitionID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\x00\r\n") && utf8.ValidString(id)
}

func cognitionText(text string) bool {
	return strings.TrimSpace(text) != "" && len(text) <= 32768 && utf8.ValidString(text)
}

func validateCognition(d CognitionState, owner CognitionOwner) error {
	bad := func(reason string) error { return fmt.Errorf("invalid cognition: %s", reason) }
	if d.SchemaVersion != 1 || d.Owner != owner || d.Revision < 1 || len(d.Observations) == 0 {
		return bad("metadata or owner")
	}
	seen := map[string]int{}
	for _, o := range d.Observations {
		if !cognitionID(o.ID) || seen[o.ID] != 0 || o.Recipient != owner || !cognitionText(o.Text) || !cognitionID(o.SourceID) || o.WorldRevision < 0 || o.AddedRevision < 1 || o.AddedRevision > d.Revision {
			return bad("observation identity, recipient or provenance")
		}
		if (o.Speaker != "" && !cognitionID(o.Speaker)) || (o.Location != "" && !cognitionText(o.Location)) {
			return bad("observation speaker or location")
		}
		switch o.Kind {
		case "direct_observation", "public_statement", "private_report", "rumor":
			if !cognitionID(o.EventID) || o.WorldRevision < 1 {
				return bad("event-backed observation requires committed source")
			}
		case "profile":
			if o.EventID != "" {
				return bad("profile is not an event receipt")
			}
		default:
			return bad("observation kind")
		}
		seen[o.ID] = o.AddedRevision
	}
	previous := map[string]CognitionRecord{}
	superseded := map[string]bool{}
	for _, r := range d.Records {
		if !cognitionID(r.ID) || seen[r.ID] != 0 || r.Owner != owner || !cognitionText(r.Text) || r.AddedRevision < 1 || r.AddedRevision > d.Revision || len(r.Sources) == 0 {
			return bad("record identity or sources")
		}
		switch r.Kind {
		case "belief", "hypothesis", "goal", "fear", "bias", "commitment", "plan", "relationship", "working_memory", "long_term_memory", "summary":
		default:
			return bad("record kind")
		}
		if r.Status != "active" && r.Status != "resolved" && r.Status != "abandoned" {
			return bad("record status")
		}
		if r.Confidence != nil && (math.IsNaN(*r.Confidence) || math.IsInf(*r.Confidence, 0) || *r.Confidence < 0 || *r.Confidence > 1) {
			return bad("confidence must be within 0..1")
		}
		sources := map[string]bool{}
		for _, source := range r.Sources {
			if seen[source] == 0 || seen[source] > r.AddedRevision || sources[source] {
				return bad("source is missing, foreign or later than record")
			}
			sources[source] = true
		}
		if r.Supersedes != "" {
			old, ok := previous[r.Supersedes]
			if !ok || old.Kind != r.Kind || old.AddedRevision > r.AddedRevision || superseded[old.ID] {
				return bad("supersedes must name a current record of the same kind")
			}
			superseded[old.ID] = true
		}
		seen[r.ID], previous[r.ID] = r.AddedRevision, r
	}
	return validateThoughtReceipts(d)
}

func (s *CognitionStore) validateLocked(state CognitionState) error {
	if err := validateCognition(state, s.owner); err != nil {
		return err
	}
	d, _, err := s.registry.load()
	if err != nil {
		return err
	}
	for _, observation := range state.Observations {
		if observation.WorldRevision > d.WorldRevision {
			return errors.New("cognition is ahead of registered world; synchronize world or restore an explicit cognition branch")
		}
	}
	return nil
}

func (s *CognitionStore) readLocked() (CognitionState, error) {
	if err := s.checkBindingLocked(); err != nil {
		return CognitionState{}, err
	}
	f, err := os.Open(s.path())
	if os.IsNotExist(err) {
		return CognitionState{SchemaVersion: 1, Owner: s.owner, Observations: []Observation{}, Records: []CognitionRecord{}}, nil
	}
	if err != nil {
		return CognitionState{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCognitionBytes+1))
	if err != nil || len(b) > maxCognitionBytes {
		return CognitionState{}, errors.New("cognition read failed or size limit exceeded; preserve file for recovery")
	}
	var d CognitionState
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&d) != nil || dec.Decode(new(any)) != io.EOF {
		return CognitionState{}, errors.New("invalid cognition document; preserve file for recovery")
	}
	if err := s.validateLocked(d); err != nil {
		return CognitionState{}, err
	}
	return d, nil
}

// Read is pure and returns a detached document. Missing files are not initialized.
func (s *CognitionStore) Read() (CognitionState, error) {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	return s.readLocked()
}

// Append atomically publishes one revision of receipts and derived records.
// AddedRevision is assigned here, never accepted from the caller. No deletion or
// in-place update operation is exposed. Failed writes do not consume revisions.
func (s *CognitionStore) Append(expectedRevision int, observations []Observation, records []CognitionRecord) (CognitionState, error) {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	return s.appendLocked(expectedRevision, observations, records)
}

func (s *CognitionStore) appendLocked(expectedRevision int, observations []Observation, records []CognitionRecord) (CognitionState, error) {
	d, err := s.readLocked()
	if err != nil {
		return CognitionState{}, err
	}
	if expectedRevision != d.Revision {
		return CognitionState{}, ErrCognitionConflict
	}
	if len(observations)+len(records) == 0 {
		return CognitionState{}, errors.New("empty cognition update")
	}
	d.Revision++
	for _, o := range observations {
		if o.AddedRevision != 0 {
			return CognitionState{}, errors.New("observation revision is assigned by store")
		}
		o.AddedRevision = d.Revision
		d.Observations = append(d.Observations, o)
	}
	for _, r := range records {
		if r.AddedRevision != 0 {
			return CognitionState{}, errors.New("record revision is assigned by store")
		}
		r.AddedRevision = d.Revision
		d.Records = append(d.Records, r)
	}
	return s.publishLocked(d)
}

func (s *CognitionStore) publishLocked(d CognitionState) (CognitionState, error) {
	if err := s.validateLocked(d); err != nil {
		return CognitionState{}, err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil || len(b) > maxCognitionBytes {
		return CognitionState{}, errors.New("cognition update exceeds storage limits")
	}
	if err := fsutil.WriteFileAtomic(s.path(), b); err != nil {
		return CognitionState{}, err
	}
	// Re-decode the committed representation so caller-owned pointers and slices
	// do not alias the returned document either.
	var saved CognitionState
	if err := json.Unmarshal(b, &saved); err != nil {
		return CognitionState{}, err
	}
	return saved, nil
}
