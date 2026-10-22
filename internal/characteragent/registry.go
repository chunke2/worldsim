// Package characteragent owns persistent character Agent identities. Registry
// entries do not yet execute model requests; that is Phase 2B/2C.
package characteragent

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"worldsim/internal/config"
	"worldsim/internal/engine"
	"worldsim/internal/fsutil"
)

const RegistryFile = "agent_registry.json"

type Identity struct {
	AgentID            string                 `json:"agent_id"`
	CharacterID        string                 `json:"character_id"`
	Importance         config.AgentImportance `json:"importance"`
	State              string                 `json:"state"` // active | dormant
	DormantReason      string                 `json:"dormant_reason,omitempty"`
	RegisteredRevision int                    `json:"registered_revision"`
}

// Document is authoritative for identity allocation. Private memory stays in
// each Agent directory and is never copied into the registry.
type Document struct {
	SchemaVersion int                 `json:"schema_version"`
	WorldID       string              `json:"world_id"`
	Revision      int                 `json:"revision"` // registry revision, not world revision
	WorldRevision int                 `json:"world_revision"`
	Agents        map[string]Identity `json:"agents"` // keyed by the engine's entity ID
}

type AgentRegistry struct {
	dir string
	mu  *sync.Mutex
}

var directoryLocks sync.Map

func NewRegistry(worldDir string) (*AgentRegistry, error) {
	dir, err := filepath.Abs(worldDir)
	if err != nil {
		return nil, err
	}
	key := filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	lock, _ := directoryLocks.LoadOrStore(key, &sync.Mutex{})
	return &AgentRegistry{dir: dir, mu: lock.(*sync.Mutex)}, nil
}

func stableID(worldID, characterID string) string {
	h := sha256.Sum256([]byte(worldID + "\x00" + characterID))
	return hex.EncodeToString(h[:])
}

func (r *AgentRegistry) load() (Document, bool, error) {
	var d Document
	b, err := os.ReadFile(filepath.Join(r.dir, RegistryFile))
	if os.IsNotExist(err) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, false, fmt.Errorf("invalid character Agent registry; preserve file for recovery")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return d, false, fmt.Errorf("invalid trailing Agent registry data")
	}
	if d.SchemaVersion != 1 || len(d.WorldID) != 32 || d.Revision < 1 || d.WorldRevision < 0 || d.Agents == nil {
		return d, false, fmt.Errorf("invalid Agent registry metadata")
	}
	if _, err := hex.DecodeString(d.WorldID); err != nil {
		return d, false, fmt.Errorf("invalid Agent registry world ID")
	}
	for id, a := range d.Agents {
		if strings.TrimSpace(id) == "" || id != a.CharacterID || a.AgentID != stableID(d.WorldID, id) || !a.Importance.Valid() || (a.State != "active" && a.State != "dormant") || a.RegisteredRevision < 0 {
			return d, false, fmt.Errorf("invalid character Agent identity")
		}
	}
	return d, true, nil
}

// Read is pure: absent registries are returned as absent, never initialized.
func (r *AgentRegistry) Read() (Document, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.load()
}

func defaultImportance(id string, ent engine.Entity, protagonist string) config.AgentImportance {
	role, _ := ent.Extra["role"].(string)
	tier, _ := ent.Extra["tier"].(string)
	if id == protagonist || role == "protagonist" || role == "rival" || role == "love_interest" {
		return config.ImportanceCore
	}
	if role == "temporary_npc" || role == "walkon" || tier == "walkon" || ent.Status == "mentioned" {
		return config.ImportanceBackground
	}
	return config.ImportanceSupporting
}

func lifecycle(ent engine.Entity) (state, reason string) {
	if !ent.Alive || ent.Status == "dead" {
		return "dormant", "dead"
	}
	switch ent.Status {
	case "departed", "mentioned", "dormant":
		return "dormant", ent.Status
	case "active":
		return "active", ""
	default:
		return "dormant", "not_actionable"
	}
}

// Reconcile is an idempotent projection of a committed world snapshot. Directory
// creation precedes the atomic registry write. Interrupted attempts can leave an
// empty directory, but cannot publish an identity whose directory is absent.
// Existing identity, importance and memory are retained on every lifecycle change.
func (r *AgentRegistry) Reconcile(world *engine.WorldState) (Document, error) {
	return r.reconcile(world, false)
}

// ReconcileAfterRewind is called only after an explicit snapshot restoration.
// Normal synchronization rejects stale snapshots rather than undoing new state.
func (r *AgentRegistry) ReconcileAfterRewind(world *engine.WorldState) (Document, error) {
	return r.reconcile(world, true)
}

func (r *AgentRegistry) reconcile(world *engine.WorldState, afterRewind bool) (Document, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if world == nil || world.Revision < 0 {
		return Document{}, fmt.Errorf("Agent reconciliation requires a valid world snapshot")
	}
	for id := range world.Entities {
		if strings.TrimSpace(id) == "" || id != strings.TrimSpace(id) {
			return Document{}, fmt.Errorf("invalid entity ID; diagnose the world before registering Agents")
		}
	}
	d, exists, err := r.load()
	if err != nil {
		return Document{}, err
	}
	if exists && world.Revision < d.WorldRevision && !afterRewind {
		return Document{}, fmt.Errorf("stale world snapshot; Agent reconciliation requires explicit rewind recovery")
	}
	before, _ := json.Marshal(d)
	if !exists {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return Document{}, err
		}
		d = Document{SchemaVersion: 1, WorldID: hex.EncodeToString(id[:]), Agents: map[string]Identity{}}
	}
	for id, ent := range world.Entities {
		a, ok := d.Agents[id]
		if !ok {
			a = Identity{AgentID: stableID(d.WorldID, id), CharacterID: id, Importance: defaultImportance(id, ent, world.WorldLevel.Protagonist), RegisteredRevision: world.Revision}
		}
		a.State, a.DormantReason = lifecycle(ent)
		d.Agents[id] = a
	}
	for id, a := range d.Agents {
		if _, ok := world.Entities[id]; !ok {
			a.State, a.DormantReason = "dormant", "absent_from_world"
			d.Agents[id] = a
		}
	}
	// Keep world revision separate: rewind may legitimately lower it. Stable IDs
	// and private files survive; this is not a rollback of cognition or memory.
	d.WorldRevision = world.Revision
	for _, a := range d.Agents {
		if err := os.MkdirAll(filepath.Join(r.dir, "agents", a.AgentID), 0700); err != nil {
			return Document{}, err
		}
	}
	after, _ := json.Marshal(d)
	if exists && bytes.Equal(before, after) {
		return d, nil
	}
	d.Revision++
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return Document{}, err
	}
	if err := os.MkdirAll(r.dir, 0755); err != nil {
		return Document{}, err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(r.dir, RegistryFile), b); err != nil {
		return Document{}, err
	}
	return d, nil
}
