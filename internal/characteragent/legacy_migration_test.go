package characteragent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"worldsim/internal/config"
)

func legacyFixture(t *testing.T, wrapped bool) (string, *CognitionStore, []byte) {
	t.Helper()
	dir, a, _ := cognitionFixture(t)
	memories, archive := map[string][]map[string]any{}, map[string][]map[string]any{}
	for id := range fiveCharacters().Entities {
		memories[id] = []map[string]any{{"actor": id, "content": id + "-PRIVATE-CANARY", "kind": "event", "day": 1, "importance": 0.8, "source_events": []string{"unverified-event"}, "custom": "preserved"}}
		archive[id] = []map[string]any{{"actor": id, "content": id + "-ARCHIVE-CANARY", "kind": "summary", "day": 1}}
	}
	var source []byte
	var err error
	if wrapped {
		source, err = json.MarshalIndent(map[string]any{"memories": memories, "archive": archive}, "", "  ")
	} else {
		source, err = json.Marshal(memories)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, legacyMemoryFile), source, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, a, source
}

func migrationTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			relative, _ := filepath.Rel(root, path)
			result[relative] = legacyHash(mustCognitionBytes(t, path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLegacyMigrationFiveActorPreviewApplyAndRetry(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "wrapped"}[wrapped], func(t *testing.T) {
			dir, a, source := legacyFixture(t, wrapped)
			// Existing trusted cognition must not be changed by migration.
			if _, err := a.Append(0, []Observation{receipt(a, "existing", "current cognition")}, nil); err != nil {
				t.Fatal(err)
			}
			before := migrationTree(t, dir)
			plan, err := PreviewLegacyMigration(dir)
			if err != nil || len(plan.Actors) != 5 {
				t.Fatal("preview failed", err)
			}
			for relative := range plan.Files {
				if strings.Contains(relative, "\\") {
					t.Fatal("manifest paths must be portable")
				}
			}
			if !reflect.DeepEqual(before, migrationTree(t, dir)) {
				t.Fatal("preview changed files")
			}
			report, _ := json.Marshal(plan)
			if bytes.Contains(report, []byte("CANARY")) {
				t.Fatal("preview exposed memory text")
			}
			if _, err := a.ReadLegacyQuarantine(plan.ID); err == nil {
				t.Fatal("uncommitted migration exposed")
			}
			applied, err := ApplyLegacyMigration(dir, plan.ID)
			if err != nil || !reflect.DeepEqual(plan, applied) {
				t.Fatal("apply failed", err)
			}
			if !bytes.Equal(source, mustCognitionBytes(t, filepath.Join(dir, legacyMemoryFile))) || !bytes.Equal(source, mustCognitionBytes(t, filepath.Join(dir, "memory_migrations", plan.ID, "source.json"))) {
				t.Fatal("source or snapshot changed")
			}
			if before[filepath.Join("agents", a.Owner().AgentID, CognitionFile)] != legacyHash(mustCognitionBytes(t, a.path())) {
				t.Fatal("migration changed cognition")
			}
			for _, actor := range plan.Actors {
				s, _ := NewCognitionStore(dir, actor.Owner.CharacterID)
				bundle, err := s.ReadLegacyQuarantine(plan.ID)
				want := 1
				if wrapped {
					want = 2
				}
				if err != nil || len(bundle.Items) != want || bundle.Status != "quarantined" {
					t.Fatal("wrong actor bundle", err)
				}
				b, _ := json.Marshal(bundle)
				for other := range fiveCharacters().Entities {
					if other != actor.Owner.CharacterID && bytes.Contains(b, []byte(other+"-PRIVATE-CANARY")) {
						t.Fatal("private memories crossed actor partitions")
					}
				}
				if !bytes.Contains(b, []byte("custom")) {
					t.Fatal("unknown entry field lost")
				}
			}
			after := migrationTree(t, dir)
			if _, err := ApplyLegacyMigration(dir, plan.ID); err != nil {
				t.Fatal("idempotent retry failed", err)
			}
			if !reflect.DeepEqual(after, migrationTree(t, dir)) {
				t.Fatal("retry changed or duplicated artifacts")
			}
		})
	}
}

func TestLegacyMigrationConflictingAndUnknownActorsRemainUnassigned(t *testing.T) {
	dir, a, _ := legacyFixture(t, false)
	source := []byte(`{"memories":{"沈砚":[{"actor":"罗绮","kind":"event","content":"OTHER-PRIVATE-CANARY"},{"kind":"event","content":"missing actor"},{"actor":"沈砚","kind":"reasoning","content":"RAW-REASONING-CANARY"},{"actor":"沈砚","kind":"event","content":"x","reasoning":"RAW-REASONING-CANARY"},null]},"archive":{"未注册人物":[{"actor":"未注册人物","kind":"summary","content":"UNKNOWN-PRIVATE-CANARY"}]}}`)
	if err := os.WriteFile(filepath.Join(dir, legacyMemoryFile), source, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PreviewLegacyMigration(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"actor_mismatch", "missing_actor", "unsupported_kind", "unsafe_field", "invalid_entry", "unknown_character"} {
		if plan.Unassigned[reason] != 1 {
			t.Fatal("incorrect quarantine diagnostic", reason)
		}
	}
	if _, err := ApplyLegacyMigration(dir, plan.ID); err != nil {
		t.Fatal(err)
	}
	bundle, err := a.ReadLegacyQuarantine(plan.ID)
	if err != nil || len(bundle.Items) != 0 {
		t.Fatal("ambiguous memory assigned to actor")
	}
	for _, marker := range []string{"OTHER-PRIVATE-CANARY", "RAW-REASONING-CANARY", "UNKNOWN-PRIVATE-CANARY"} {
		if !bytes.Contains(mustCognitionBytes(t, filepath.Join(dir, "memory_migrations", plan.ID, "unassigned.json")), []byte(marker)) {
			t.Fatal("unassigned original lost")
		}
	}
	if d, err := a.Read(); err != nil || d.Revision != 0 {
		t.Fatal("unverified migration entered cognition")
	}
}

func TestLegacyMigrationMalformedSourceIsReadOnly(t *testing.T) {
	for _, source := range []string{`null`, `[]`, `{"沈砚":[]} {}`, `{"沈砚":[],"沈砚":[]}`, `{"memories":{},"archive":{},"unexpected":1}`, `{"沈砚":[{"actor":"沈砚","actor":"罗绮"}]}`, `{"memories":{"沈砚":{}}}`} {
		t.Run(source, func(t *testing.T) {
			dir, _, _ := legacyFixture(t, false)
			if err := os.WriteFile(filepath.Join(dir, legacyMemoryFile), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			before := migrationTree(t, dir)
			if _, err := PreviewLegacyMigration(dir); err == nil {
				t.Fatal("malformed source accepted")
			}
			if _, err := ApplyLegacyMigration(dir, "anything"); err == nil {
				t.Fatal("malformed source applied")
			}
			if !reflect.DeepEqual(before, migrationTree(t, dir)) {
				t.Fatal("bad source caused writes")
			}
		})
	}
}

func TestLegacyMigrationStalePlanAndWriteFailureRecovery(t *testing.T) {
	dir, a, source := legacyFixture(t, true)
	plan, err := PreviewLegacyMigration(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, legacyMemoryFile), append(source, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLegacyMigration(dir, plan.ID); err == nil {
		t.Fatal("stale plan accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "memory_migrations")); !os.IsNotExist(err) {
		t.Fatal("stale plan created artifacts")
	}
	if err := os.WriteFile(filepath.Join(dir, legacyMemoryFile), source, 0600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "agents", a.Owner().AgentID, "legacy", plan.ID+".json.tmp")
	if err := os.MkdirAll(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLegacyMigration(dir, plan.ID); err == nil {
		t.Fatal("write failure reported success")
	}
	if _, err := a.ReadLegacyQuarantine(plan.ID); err == nil {
		t.Fatal("partial output exposed before commit")
	}
	if !bytes.Equal(source, mustCognitionBytes(t, filepath.Join(dir, "memory_migrations", plan.ID, "source.json"))) {
		t.Fatal("backup not preserved before failure")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLegacyMigration(dir, plan.ID); err != nil {
		t.Fatal("retry did not recover", err)
	}
	if _, err := a.ReadLegacyQuarantine(plan.ID); err != nil {
		t.Fatal("recovered bundle unreadable", err)
	}
}

func TestLegacyMigrationTamperingAndCrossWorldAccessRejected(t *testing.T) {
	dir, a, _ := legacyFixture(t, true)
	plan, _ := PreviewLegacyMigration(dir)
	if _, err := ApplyLegacyMigration(dir, plan.ID); err != nil {
		t.Fatal(err)
	}
	_, foreign, _ := legacyFixture(t, true)
	if _, err := foreign.ReadLegacyQuarantine(plan.ID); err == nil {
		t.Fatal("foreign world accepted migration")
	}
	path := filepath.Join(dir, "agents", a.Owner().AgentID, "legacy", plan.ID+".json")
	changed := []byte(`{"content":"TAMPERED-PRIVATE-CANARY"}`)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ReadLegacyQuarantine(plan.ID); err == nil || strings.Contains(err.Error(), "CANARY") {
		t.Fatal("tampered bundle accepted or private text echoed")
	}
	if _, err := ApplyLegacyMigration(dir, plan.ID); err == nil {
		t.Fatal("retry overwrote conflicting artifact")
	}
	if !bytes.Equal(changed, mustCognitionBytes(t, path)) {
		t.Fatal("conflicting artifact overwritten")
	}
	if _, err := a.ReadLegacyQuarantine("../source"); err == nil {
		t.Fatal("path-like plan accepted")
	}
}

func TestLegacyMigrationConcurrentApplicationsConverge(t *testing.T) {
	dir, a, _ := legacyFixture(t, true)
	plan, _ := PreviewLegacyMigration(dir)
	var wg sync.WaitGroup
	errorsOut := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := ApplyLegacyMigration(dir, plan.ID); errorsOut <- err }()
	}
	wg.Wait()
	close(errorsOut)
	success := 0
	for err := range errorsOut {
		if err == nil {
			success++
		} else if err != config.ErrBusy {
			t.Fatal(err)
		}
	}
	if success == 0 {
		t.Fatal("neither application succeeded")
	}
	if _, err := a.ReadLegacyQuarantine(plan.ID); err != nil {
		t.Fatal("committed output unreadable")
	}
}

func TestLegacyMigrationManifestFailureAndBusyWorld(t *testing.T) {
	dir, a, _ := legacyFixture(t, true)
	plan, _ := PreviewLegacyMigration(dir)
	end, err := config.BeginWorldActivity(filepath.Dir(filepath.Dir(dir)), dir)
	if err != nil {
		t.Fatal(err)
	}
	_, applyErr := ApplyLegacyMigration(dir, plan.ID)
	end()
	if applyErr != config.ErrBusy {
		t.Fatal("running world allowed migration")
	}
	blocked := filepath.Join(dir, "memory_migrations", plan.ID, "manifest.json.tmp")
	if err := os.MkdirAll(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLegacyMigration(dir, plan.ID); err == nil {
		t.Fatal("manifest write failure reported success")
	}
	if _, err := a.ReadLegacyQuarantine(plan.ID); err == nil {
		t.Fatal("uncommitted role copy became readable")
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", a.Owner().AgentID, "legacy", plan.ID+".json")); err != nil {
		t.Fatal("expected prepared role artifact")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLegacyMigration(dir, plan.ID); err != nil {
		t.Fatal("commit retry failed", err)
	}
	if _, err := a.ReadLegacyQuarantine(plan.ID); err != nil {
		t.Fatal("committed quarantine unreadable", err)
	}
}

func TestLegacyMigrationRegistryChangeInvalidatesPlan(t *testing.T) {
	dir, a, _ := legacyFixture(t, true)
	plan, _ := PreviewLegacyMigration(dir)
	world := fiveCharacters()
	world.Revision++
	if _, err := a.registry.Reconcile(world); err != nil {
		t.Fatal(err)
	}
	before := migrationTree(t, dir)
	if _, err := ApplyLegacyMigration(dir, plan.ID); err == nil {
		t.Fatal("stale registry plan accepted")
	}
	if !reflect.DeepEqual(before, migrationTree(t, dir)) {
		t.Fatal("stale registry plan wrote artifacts")
	}
}
