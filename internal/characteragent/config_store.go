package characteragent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"worldsim/internal/config"
	"worldsim/internal/fsutil"
)

const RuntimeConfigFile = "agent_runtime.json"

func (r *AgentRegistry) readConfig(defaults config.AgentRuntimeConfig, registry Document) (config.AgentRuntimeConfig, error) {
	c := defaults
	b, err := os.ReadFile(filepath.Join(r.dir, RuntimeConfigFile))
	if err == nil {
		// Persisted documents are complete; do not hide missing fields by merging
		// them with freshly derived defaults.
		c = config.AgentRuntimeConfig{}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("invalid Agent configuration; preserve for recovery")
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return c, fmt.Errorf("invalid trailing Agent config data")
		}
		if c.SchemaVersion != 1 || c.Revision < 1 {
			return c, fmt.Errorf("invalid Agent config metadata")
		}
	} else if !os.IsNotExist(err) {
		return c, err
	}
	if c.Characters == nil {
		c.Characters = map[string]config.CharacterAgentConfig{}
	}
	for id, identity := range registry.Agents {
		if _, ok := c.Characters[id]; !ok {
			c.Characters[id] = config.CharacterAgentConfig{Importance: identity.Importance}
		}
	}
	return c, nil
}

// ConfigView returns defaults for new registrations without writing on GET.
func (r *AgentRegistry) ConfigView(defaults config.AgentRuntimeConfig) (config.AgentRuntimeConfig, Document, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, exists, err := r.load()
	if err != nil {
		return config.AgentRuntimeConfig{}, d, err
	}
	if !exists {
		d = Document{Agents: map[string]Identity{}}
	}
	c, err := r.readConfig(defaults, d)
	return c, d, err
}

// SaveConfig serializes validation and revision checks with reconciliation.
// Configuration is a single atomic document; the registry retains initial defaults.
func (r *AgentRegistry) SaveConfig(next config.AgentRuntimeConfig, registryRevision int, providers config.ProviderRegistry) (config.AgentRuntimeConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, exists, err := r.load()
	if err != nil {
		return next, err
	}
	if !exists {
		return next, fmt.Errorf("register characters before configuring Agents")
	}
	if d.Revision != registryRevision {
		return next, config.ErrRevisionConflict
	}
	current, err := r.readConfig(config.AgentRuntimeConfig{SchemaVersion: 1}, d)
	if err != nil {
		return next, err
	}
	if next.Revision != current.Revision {
		return next, config.ErrRevisionConflict
	}
	if len(next.Characters) != len(d.Agents) {
		return next, fmt.Errorf("configuration must include every registered character")
	}
	for id := range next.Characters {
		if _, ok := d.Agents[id]; !ok {
			return next, fmt.Errorf("unknown character")
		}
	}
	if err := next.Validate(providers); err != nil {
		return next, err
	}
	next.Revision++
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return next, err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(r.dir, RuntimeConfigFile), b); err != nil {
		return next, err
	}
	return next, nil
}
