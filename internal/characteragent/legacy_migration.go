package characteragent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"worldsim/internal/config"
	"worldsim/internal/fsutil"
)

const legacyMemoryFile = "agents_memory.json"

type LegacyActorPlan struct {
	Owner   CognitionOwner `json:"owner"`
	Working int            `json:"working"`
	Archive int            `json:"archive"`
}

// LegacyMigrationPlan contains counts and digests, never memory text.
type LegacyMigrationPlan struct {
	SchemaVersion  int               `json:"schema_version"`
	ID             string            `json:"id"`
	WorldID        string            `json:"world_id"`
	SourceSHA256   string            `json:"source_sha256"`
	RegistrySHA256 string            `json:"registry_sha256"`
	Status         string            `json:"status"`
	Actors         []LegacyActorPlan `json:"actors"`
	Unassigned     map[string]int    `json:"unassigned"`
	Files          map[string]string `json:"files"`
}

type LegacyItem struct {
	Bucket string          `json:"bucket"`
	Index  int             `json:"index"`
	Raw    json.RawMessage `json:"original"`
}

// LegacyQuarantine is explicitly not a CognitionState or an observation packet.
type LegacyQuarantine struct {
	SchemaVersion int            `json:"schema_version"`
	Owner         CognitionOwner `json:"owner"`
	PlanID        string         `json:"plan_id"`
	SourceSHA256  string         `json:"source_sha256"`
	Status        string         `json:"status"`
	Reason        string         `json:"reason"`
	Items         []LegacyItem   `json:"items"`
}

type unassignedLegacy struct {
	LegacyItem
	Group  string `json:"group"`
	Reason string `json:"reason"`
}

type legacyJob struct {
	plan  LegacyMigrationPlan
	files map[string][]byte
}

func legacyPlanID(source, registry string) string {
	return privateHash([]byte("legacy-memory-v1\x00" + source + "\x00" + registry))
}

func legacyBuckets(b []byte) (map[string]map[string][]json.RawMessage, error) {
	if err := checkUniqueJSON(b); err != nil {
		return nil, err
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(b, &root) != nil || root == nil {
		return nil, errors.New("legacy root must be an object")
	}
	result := map[string]map[string][]json.RawMessage{}
	// Names matching wrapper keys remain legal in the old direct-map format.
	wrapped := false
	for _, key := range []string{"memories", "archive"} {
		if value, ok := root[key]; ok && (bytes.HasPrefix(bytes.TrimSpace(value), []byte("{")) || bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			wrapped = true
		}
	}
	if !wrapped {
		var groups map[string][]json.RawMessage
		if json.Unmarshal(b, &groups) != nil {
			return nil, errors.New("invalid legacy character groups")
		}
		result["memories"] = groups
		return result, nil
	}
	for key, value := range root {
		if key != "memories" && key != "archive" {
			return nil, errors.New("unknown legacy top-level field; preserve source for recovery")
		}
		var groups map[string][]json.RawMessage
		if json.Unmarshal(value, &groups) != nil {
			return nil, errors.New("invalid legacy bucket")
		}
		result[key] = groups
	}
	return result, nil
}

func legacyReason(raw json.RawMessage, group string, registry Document) string {
	if _, ok := registry.Agents[group]; !ok {
		return "unknown_character"
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "invalid_entry"
	}
	var actor, content, kind string
	if json.Unmarshal(fields["actor"], &actor) != nil || actor == "" {
		return "missing_actor"
	}
	if actor != group {
		return "actor_mismatch"
	}
	if json.Unmarshal(fields["content"], &content) != nil || strings.TrimSpace(content) == "" {
		return "invalid_content"
	}
	if json.Unmarshal(fields["kind"], &kind) != nil {
		return "unsupported_kind"
	}
	switch kind {
	case "event", "dialogue", "state", "observation", "reflection", "plan", "archive", "summary":
	default:
		return "unsupported_kind"
	}
	for _, key := range []string{"reasoning", "reasoning_content", "chain_of_thought", "prompt", "author_plan", "api_key"} {
		if _, ok := fields[key]; ok {
			return "unsafe_field"
		}
	}
	return ""
}

func buildLegacyJob(r *AgentRegistry) (legacyJob, error) {
	d, exists, err := r.load()
	if err != nil || !exists {
		return legacyJob{}, errors.New("migration requires a valid existing Agent registry")
	}
	source, err := readPrivateFile(r.dir, legacyMemoryFile)
	if err != nil {
		return legacyJob{}, errors.New("cannot read agents_memory.json; source left unchanged")
	}
	registry, err := readPrivateFile(r.dir, RegistryFile)
	if err != nil {
		return legacyJob{}, errors.New("cannot snapshot Agent registry")
	}
	buckets, err := legacyBuckets(source)
	if err != nil {
		return legacyJob{}, err
	}
	p := LegacyMigrationPlan{SchemaVersion: 1, WorldID: d.WorldID, SourceSHA256: privateHash(source), RegistrySHA256: privateHash(registry), Status: "quarantine_only", Actors: []LegacyActorPlan{}, Unassigned: map[string]int{}, Files: map[string]string{}}
	p.ID = legacyPlanID(p.SourceSHA256, p.RegistrySHA256)
	job := legacyJob{plan: p, files: map[string][]byte{}}
	base := filepath.Join("memory_migrations", p.ID)
	job.files[filepath.Join(base, "source.json")] = source
	job.files[filepath.Join(base, "registry.json")] = registry
	unassigned := []unassignedLegacy{}
	ids := make([]string, 0, len(d.Agents))
	for id := range d.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	bundles := map[string]LegacyQuarantine{}
	counts := map[string]LegacyActorPlan{}
	for _, id := range ids {
		owner := CognitionOwner{WorldID: d.WorldID, AgentID: d.Agents[id].AgentID, CharacterID: id}
		bundles[id] = LegacyQuarantine{SchemaVersion: 1, Owner: owner, PlanID: p.ID, SourceSHA256: p.SourceSHA256, Status: "quarantined", Reason: "legacy_visibility_not_verified", Items: []LegacyItem{}}
		counts[id] = LegacyActorPlan{Owner: owner}
	}
	for _, bucket := range []string{"memories", "archive"} {
		groups := make([]string, 0, len(buckets[bucket]))
		for group := range buckets[bucket] {
			groups = append(groups, group)
		}
		sort.Strings(groups)
		for _, group := range groups {
			for index, raw := range buckets[bucket][group] {
				item := LegacyItem{Bucket: bucket, Index: index, Raw: raw}
				if reason := legacyReason(raw, group, d); reason != "" {
					unassigned = append(unassigned, unassignedLegacy{LegacyItem: item, Group: group, Reason: reason})
					job.plan.Unassigned[reason]++
					continue
				}
				bundle := bundles[group]
				bundle.Items = append(bundle.Items, item)
				bundles[group] = bundle
				count := counts[group]
				if bucket == "memories" {
					count.Working++
				} else {
					count.Archive++
				}
				counts[group] = count
			}
		}
	}
	for _, id := range ids {
		bundle := bundles[id]
		job.plan.Actors = append(job.plan.Actors, counts[id])
		data, err := json.MarshalIndent(bundle, "", "  ")
		if err != nil {
			return legacyJob{}, err
		}
		job.files[filepath.Join("agents", bundle.Owner.AgentID, "legacy", p.ID+".json")] = data
	}
	data, err := json.MarshalIndent(unassigned, "", "  ")
	if err != nil {
		return legacyJob{}, err
	}
	job.files[filepath.Join(base, "unassigned.json")] = data
	for relative, data := range job.files {
		if len(data) > maxPrivateFileBytes {
			return legacyJob{}, errors.New("migration artifact exceeds size limit; source preserved")
		}
		job.plan.Files[filepath.ToSlash(relative)] = privateHash(data)
	}
	return job, nil
}

func PreviewLegacyMigration(worldDir string) (LegacyMigrationPlan, error) {
	r, err := NewRegistry(worldDir)
	if err != nil {
		return LegacyMigrationPlan{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	job, err := buildLegacyJob(r)
	return job.plan, err
}

// writeLegacyOnce supports interrupted retries but never replaces existing data.
func writeLegacyOnce(root, relative string, data []byte) error {
	path, err := privatePath(root, relative)
	if err != nil {
		return err
	}
	if old, err := readPrivateFile(root, relative); err == nil {
		if bytes.Equal(old, data) {
			return nil
		}
		return errors.New("migration artifact differs; preserve it and investigate")
	} else if !os.IsNotExist(err) {
		return errors.New("migration artifact cannot be safely inspected")
	}
	if _, err := privatePath(root, relative+".tmp"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("cannot create migration partition")
	}
	if err := fsutil.WriteFileAtomic(path, data); err != nil {
		return errors.New("migration write failed; source preserved, retry same plan after resolving storage error")
	}
	return nil
}

func ApplyLegacyMigration(worldDir, expectedPlanID string) (LegacyMigrationPlan, error) {
	r, err := NewRegistry(worldDir)
	if err != nil {
		return LegacyMigrationPlan{}, err
	}
	end, err := config.BeginWorldActivity(filepath.Dir(filepath.Dir(r.dir)), r.dir)
	if err != nil {
		return LegacyMigrationPlan{}, err
	}
	defer end()
	r.mu.Lock()
	defer r.mu.Unlock()
	job, err := buildLegacyJob(r)
	if err != nil {
		return LegacyMigrationPlan{}, err
	}
	if expectedPlanID == "" || expectedPlanID != job.plan.ID {
		return LegacyMigrationPlan{}, errors.New("migration plan changed; preview again before applying")
	}
	base := filepath.Join("memory_migrations", job.plan.ID)
	// Back up the exact source before writing any per-character artifact.
	order := []string{filepath.Join(base, "source.json"), filepath.Join(base, "registry.json")}
	rest := []string{}
	for relative := range job.files {
		if relative != order[0] && relative != order[1] {
			rest = append(rest, relative)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)
	for _, relative := range order {
		if err := writeLegacyOnce(r.dir, relative, job.files[relative]); err != nil {
			return LegacyMigrationPlan{}, err
		}
	}
	current, err := buildLegacyJob(r)
	if err != nil || current.plan.ID != job.plan.ID {
		return LegacyMigrationPlan{}, errors.New("source or registry changed during migration; output remains uncommitted")
	}
	manifest, err := json.MarshalIndent(job.plan, "", "  ")
	if err != nil {
		return LegacyMigrationPlan{}, err
	}
	if err := writeLegacyOnce(r.dir, filepath.Join(base, "manifest.json"), manifest); err != nil {
		return LegacyMigrationPlan{}, err
	}
	return job.plan, nil
}

func decodeLegacyArtifact(b []byte, target any) bool {
	if checkUniqueJSON(b) != nil {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(target) == nil && d.Decode(new(any)) == io.EOF
}

// ReadLegacyQuarantine is explicit diagnostic access, not part of Read() or
// CurrentRecords(). It reads only this actor's payload, never other actors' text.
func (s *CognitionStore) ReadLegacyQuarantine(planID string) (LegacyQuarantine, error) {
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	bad := errors.New("legacy quarantine is uncommitted, corrupt or owned by another actor")
	if !validDigest(planID) {
		return LegacyQuarantine{}, bad
	}
	if err := s.checkBindingLocked(); err != nil {
		return LegacyQuarantine{}, err
	}
	b, err := readPrivateFile(s.registry.dir, filepath.Join("memory_migrations", planID, "manifest.json"))
	if err != nil {
		return LegacyQuarantine{}, bad
	}
	var manifest LegacyMigrationPlan
	if !decodeLegacyArtifact(b, &manifest) || manifest.SchemaVersion != 1 || manifest.ID != planID || manifest.WorldID != s.owner.WorldID || manifest.Status != "quarantine_only" || !validDigest(manifest.SourceSHA256) || !validDigest(manifest.RegistrySHA256) || legacyPlanID(manifest.SourceSHA256, manifest.RegistrySHA256) != planID {
		return LegacyQuarantine{}, bad
	}
	base := filepath.Join("memory_migrations", planID)
	if manifest.Files[filepath.ToSlash(filepath.Join(base, "source.json"))] != manifest.SourceSHA256 || manifest.Files[filepath.ToSlash(filepath.Join(base, "registry.json"))] != manifest.RegistrySHA256 || len(manifest.Files) != len(manifest.Actors)+3 {
		return LegacyQuarantine{}, bad
	}
	relative := filepath.Join("agents", s.owner.AgentID, "legacy", planID+".json")
	b, err = readPrivateFile(s.registry.dir, relative)
	if err != nil || manifest.Files[filepath.ToSlash(relative)] != privateHash(b) {
		return LegacyQuarantine{}, bad
	}
	var result LegacyQuarantine
	if !decodeLegacyArtifact(b, &result) || result.SchemaVersion != 1 || result.Owner != s.owner || result.PlanID != planID || result.SourceSHA256 != manifest.SourceSHA256 || result.Status != "quarantined" || result.Reason != "legacy_visibility_not_verified" {
		return LegacyQuarantine{}, bad
	}
	return result, nil
}
