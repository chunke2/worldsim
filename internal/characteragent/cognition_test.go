package characteragent

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func cognitionFixture(t *testing.T) (string, *CognitionStore, *CognitionStore) {
	t.Helper()
	dir := t.TempDir()
	r, _ := NewRegistry(dir)
	if _, err := r.Reconcile(fiveCharacters()); err != nil {
		t.Fatal(err)
	}
	a, err := NewCognitionStore(dir, "沈砚")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewCognitionStore(dir, "罗绮")
	if err != nil {
		t.Fatal(err)
	}
	return dir, a, b
}

func receipt(s *CognitionStore, id, text string) Observation {
	return Observation{ID: id, Recipient: s.Owner(), Kind: "direct_observation", Text: text, SourceID: "delivery-" + id, EventID: "event-" + id, WorldRevision: 1}
}

func belief(s *CognitionStore, id, source string) CognitionRecord {
	confidence := 0.6
	return CognitionRecord{ID: id, Owner: s.Owner(), Kind: "belief", Text: "这只是角色判断，可能错误", Sources: []string{source}, Confidence: &confidence, Status: "active"}
}

func mustCognitionBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCognitionPureReadAndUnknownActor(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewCognitionStore(dir, "沈砚"); err == nil {
		t.Fatal("unregistered actor accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("constructor created files")
	}
	dir, a, _ := cognitionFixture(t)
	for _, id := range []string{"missing", "../沈砚", "..", ""} {
		if _, err := NewCognitionStore(dir, id); err == nil {
			t.Fatal("nonregistered path-like ID accepted")
		}
	}
	d, err := a.Read()
	if err != nil || d.Revision != 0 || len(d.Records) != 0 || d.Owner != a.Owner() {
		t.Fatal("incorrect empty view")
	}
	if _, err := os.Stat(a.path()); !os.IsNotExist(err) {
		t.Fatal("read created cognition file")
	}
	if err := os.Remove(filepath.Dir(a.path())); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err == nil {
		t.Fatal("missing registered partition silently reset cognition")
	}
}

func TestCognitionFiveActorsIsolatedAndRestartStable(t *testing.T) {
	dir, a, _ := cognitionFixture(t)
	for id := range fiveCharacters().Entities {
		s, _ := NewCognitionStore(dir, id)
		// Deliberately use identical local IDs: identity binding must still hold.
		saved, err := s.Append(0, []Observation{receipt(s, "observation", id+"-PRIVATE-CANARY")}, []CognitionRecord{belief(s, "belief", "observation")})
		if err != nil || saved.Revision != 1 {
			t.Fatal("initial cognition save failed", err)
		}
		reopened, _ := NewCognitionStore(dir, id)
		got, err := reopened.Read()
		if err != nil || !reflect.DeepEqual(saved, got) {
			t.Fatal("restart changed cognition")
		}
		data := mustCognitionBytes(t, s.path())
		for other := range fiveCharacters().Entities {
			if other != id && bytes.Contains(data, []byte(other+"-PRIVATE-CANARY")) {
				t.Fatal("another actor's canary entered document")
			}
		}
	}
	otherDir, other, _ := cognitionFixture(t)
	if a.Owner().AgentID == other.Owner().AgentID || dir == otherDir {
		t.Fatal("same-name cross-world identity collision")
	}
	if _, err := other.Append(0, []Observation{receipt(a, "foreign", "private")}, nil); err == nil {
		t.Fatal("other world's observation accepted")
	}
}

func TestCognitionRejectsForeignOwnersSourcesAndInvalidUpdates(t *testing.T) {
	_, a, b := cognitionFixture(t)
	if _, err := a.Append(0, []Observation{receipt(a, "observation", "A-PRIVATE-CANARY")}, nil); err != nil {
		t.Fatal(err)
	}
	before := mustCognitionBytes(t, a.path())
	tests := []struct {
		name string
		obs  []Observation
		recs []CognitionRecord
	}{
		{"foreign receipt", []Observation{receipt(b, "foreign", "B-PRIVATE-CANARY")}, nil},
		{"foreign record with colliding source", nil, []CognitionRecord{belief(b, "copied", "observation")}},
		{"missing source", nil, []CognitionRecord{belief(a, "b", "other-private-source")}},
		{"self cycle", nil, []CognitionRecord{belief(a, "cycle", "cycle")}},
		{"forward source", nil, []CognitionRecord{belief(a, "first", "second"), belief(a, "second", "observation")}},
		{"duplicate observation", []Observation{receipt(a, "observation", "changed")}, nil},
		{"record observation ID collision", nil, []CognitionRecord{belief(a, "observation", "observation")}},
		{"empty update", nil, nil},
	}
	for _, kind := range []string{"reasoning", "draft", "author_plan", "world_fact"} {
		r := belief(a, "wrong-kind", "observation")
		r.Kind = kind
		tests = append(tests, struct {
			name string
			obs  []Observation
			recs []CognitionRecord
		}{kind, nil, []CognitionRecord{r}})
	}
	badConfidence := belief(a, "nan", "observation")
	nan := math.NaN()
	badConfidence.Confidence = &nan
	tests = append(tests, struct {
		name string
		obs  []Observation
		recs []CognitionRecord
	}{"NaN confidence", nil, []CognitionRecord{badConfidence}})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.Append(1, tc.obs, tc.recs); err == nil {
				t.Fatal("invalid update accepted")
			} else if strings.Contains(err.Error(), "PRIVATE-CANARY") {
				t.Fatal("error exposed private content")
			}
			if !bytes.Equal(before, mustCognitionBytes(t, a.path())) {
				t.Fatal("rejected update changed file")
			}
		})
	}
}

func TestCognitionRevisionHistoryAndDetachedViews(t *testing.T) {
	dir, a, _ := cognitionFixture(t)
	first, err := a.Append(0, []Observation{receipt(a, "o1", "角色听见宣言")}, []CognitionRecord{belief(a, "b1", "o1")})
	if err != nil {
		t.Fatal(err)
	}
	updated := belief(a, "b2", "o1")
	updated.Text, updated.Supersedes = "角色改变判断", "b1"
	summary := CognitionRecord{ID: "summary", Owner: a.Owner(), Kind: "summary", Text: "仅概括自己的判断", Sources: []string{"b2"}, Status: "active"}
	saved, err := a.Append(1, nil, []CognitionRecord{updated, summary})
	if err != nil || len(saved.Records) != 3 || saved.Records[0].Text != first.Records[0].Text || saved.Records[2].AddedRevision != 2 {
		t.Fatal("history not preserved", err)
	}
	current := saved.CurrentRecords()
	if len(current) != 2 || current[0].ID != "b2" {
		t.Fatal("superseded belief remained current")
	}
	current[0].Sources[0] = "mutated"
	*current[0].Confidence = 0
	if saved.Records[1].Sources[0] != "o1" || *saved.Records[1].Confidence != 0.6 {
		t.Fatal("current view aliases history")
	}
	before := mustCognitionBytes(t, a.path())
	if _, err := a.Append(1, nil, []CognitionRecord{belief(a, "stale", "o1")}); !errors.Is(err, ErrCognitionConflict) {
		t.Fatal("stale write accepted")
	}
	fork := belief(a, "fork", "o1")
	fork.Supersedes = "b1"
	if _, err := a.Append(2, nil, []CognitionRecord{fork}); err == nil {
		t.Fatal("already superseded belief fork accepted")
	}
	if !bytes.Equal(before, mustCognitionBytes(t, a.path())) {
		t.Fatal("conflict or fork changed history")
	}
	reopened, _ := NewCognitionStore(dir, a.Owner().CharacterID)
	got, err := reopened.Read()
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatal("reloaded history differs")
	}
}

func TestCognitionAtomicRetryAndConcurrentCAS(t *testing.T) {
	dir, a, _ := cognitionFixture(t)
	if err := os.Mkdir(a.path()+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Append(0, []Observation{receipt(a, "o", "private")}, nil); err == nil {
		t.Fatal("write failure reported success")
	}
	if got, err := a.Read(); err != nil || got.Revision != 0 {
		t.Fatal("failed write consumed revision")
	}
	if err := os.Remove(a.path() + ".tmp"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsOut := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := NewCognitionStore(dir, a.Owner().CharacterID)
			if err == nil {
				_, err = s.Append(0, []Observation{receipt(s, "o", "private")}, nil)
			}
			errorsOut <- err
		}()
	}
	wg.Wait()
	close(errorsOut)
	success, conflict := 0, 0
	for err := range errorsOut {
		if err == nil {
			success++
		} else if errors.Is(err, ErrCognitionConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("expected one commit and one revision conflict")
	}
}

func TestCognitionCorruptionAndRegistryReplacementAreNotRepaired(t *testing.T) {
	_, a, b := cognitionFixture(t)
	if _, err := b.Append(0, []Observation{receipt(b, "o", "B-PRIVATE-CANARY")}, nil); err != nil {
		t.Fatal(err)
	}
	valid := mustCognitionBytes(t, b.path())
	for _, invalid := range [][]byte{
		[]byte(`{"reasoning":"PRIVATE-CANARY"}`),
		append(append([]byte{}, valid...), []byte(` {}`)...),
		valid, // A's partition containing a correctly formed document for B.
	} {
		if err := os.WriteFile(a.path(), invalid, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Read(); err == nil || strings.Contains(err.Error(), "PRIVATE-CANARY") {
			t.Fatal("corrupt/foreign document accepted or leaked")
		}
		if _, err := a.Append(0, []Observation{receipt(a, "o", "a")}, nil); err == nil {
			t.Fatal("corrupt file overwritten")
		}
		if !bytes.Equal(invalid, mustCognitionBytes(t, a.path())) {
			t.Fatal("corrupt file changed")
		}
	}
	_, foreign, _ := cognitionFixture(t)
	data := mustCognitionBytes(t, filepath.Join(foreign.registry.dir, RegistryFile))
	if err := os.WriteFile(filepath.Join(a.registry.dir, RegistryFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err == nil {
		t.Fatal("replaced world registry accepted")
	}
}

func TestCognitionStrictSchemaRejectsUnknownFieldsAndLaterSources(t *testing.T) {
	_, a, _ := cognitionFixture(t)
	d, err := a.Append(0, []Observation{receipt(a, "o", "private")}, []CognitionRecord{belief(a, "b", "o")})
	if err != nil {
		t.Fatal(err)
	}
	d.Revision = 2
	d.Observations[0].AddedRevision = 2 // record from rev 1 cannot know rev 2.
	bad, _ := json.Marshal(d)
	if err := os.WriteFile(a.path(), bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err == nil {
		t.Fatal("future observation source accepted")
	}
	d.Observations[0].AddedRevision = 1
	bad, _ = json.Marshal(d)
	bad = append([]byte(`{"reasoning":"PRIVATE-CANARY",`), bad[1:]...)
	if err := os.WriteFile(a.path(), bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err == nil || strings.Contains(err.Error(), "PRIVATE-CANARY") {
		t.Fatal("unknown reasoning field accepted or echoed")
	}
}

func TestCognitionGoalCompletionAndLocalSummary(t *testing.T) {
	_, a, _ := cognitionFixture(t)
	profile := receipt(a, "profile", "自己的目标和恐惧")
	profile.Kind, profile.EventID, profile.WorldRevision = "profile", "", 0
	goal := CognitionRecord{ID: "goal-1", Owner: a.Owner(), Kind: "goal", Text: "保住自己的筹码", Sources: []string{"profile"}, Status: "active"}
	if _, err := a.Append(0, []Observation{profile}, []CognitionRecord{goal}); err != nil {
		t.Fatal(err)
	}
	goal.ID, goal.Status, goal.Supersedes = "goal-2", "resolved", "goal-1"
	goal.Sources = []string{"result"}
	summary := CognitionRecord{ID: "summary", Owner: a.Owner(), Kind: "long_term_memory", Text: "目标已完成", Sources: []string{"goal-2"}, Status: "active"}
	d, err := a.Append(1, []Observation{receipt(a, "result", "自己的筹码还在")}, []CognitionRecord{goal, summary})
	if err != nil || len(d.Records) != 3 || len(d.CurrentRecords()) != 2 || d.CurrentRecords()[0].Status != "resolved" {
		t.Fatal("goal completion lost history or provenance", err)
	}
}

func TestCognitionWorldRewindStopsFutureMemoryWithoutDeletingIt(t *testing.T) {
	_, a, _ := cognitionFixture(t)
	if _, err := a.Append(0, []Observation{receipt(a, "o", "未来不可继承的记忆")}, nil); err != nil {
		t.Fatal(err)
	}
	before := mustCognitionBytes(t, a.path())
	world := fiveCharacters()
	world.Revision = 0
	if _, err := a.registry.ReconcileAfterRewind(world); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err == nil {
		t.Fatal("future memory exposed after rewind")
	}
	if _, err := a.Append(1, []Observation{receipt(a, "next", "new")}, nil); err == nil {
		t.Fatal("future memory silently continued")
	}
	if !bytes.Equal(before, mustCognitionBytes(t, a.path())) {
		t.Fatal("rewind guard deleted cognition")
	}
}

func TestCognitionRejectsSymlinkPartitionAndOversizedDocument(t *testing.T) {
	_, a, b := cognitionFixture(t)
	if _, err := b.Append(0, []Observation{receipt(b, "o", "B-PRIVATE-CANARY")}, nil); err != nil {
		t.Fatal(err)
	}
	t.Run("symbolic link", func(t *testing.T) {
		if err := os.Symlink(b.path(), a.path()); err != nil {
			t.Skip("OS does not allow creating test symlinks")
		}
		defer os.Remove(a.path())
		before := mustCognitionBytes(t, b.path())
		if _, err := a.Read(); err == nil {
			t.Fatal("redirected cognition read accepted")
		}
		if _, err := a.Append(0, []Observation{receipt(a, "o", "a")}, nil); err == nil {
			t.Fatal("redirected cognition write accepted")
		}
		if !bytes.Equal(before, mustCognitionBytes(t, b.path())) {
			t.Fatal("other role changed via symbolic link")
		}
	})
	large := bytes.Repeat([]byte(" "), maxCognitionBytes+1)
	if err := os.WriteFile(a.path(), large, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(); err == nil {
		t.Fatal("oversized file accepted")
	}
	if !bytes.Equal(large, mustCognitionBytes(t, a.path())) {
		t.Fatal("oversized file was modified")
	}
}
