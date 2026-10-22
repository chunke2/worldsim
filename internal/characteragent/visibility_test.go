package characteragent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"worldsim/internal/engine"
)

func visibilityFixture(t *testing.T) (string, *engine.StateEngine, *VisibilityProjector) {
	t.Helper()
	dir := t.TempDir()
	e := engine.NewStateEngine(engine.Rules{}, filepath.Join(dir, "event_log.jsonl"))
	*e.State() = *fiveCharacters()
	for id, ent := range e.State().Entities {
		ent.Location = "白室"
		e.State().Entities[id] = ent
	}
	r, _ := NewRegistry(dir)
	if _, err := r.Reconcile(e.State()); err != nil {
		t.Fatal(err)
	}
	p, _ := NewVisibilityProjector(dir)
	return dir, e, p
}

func commitVisible(t *testing.T, e *engine.StateEngine, p *VisibilityProjector, id string, occurrences []engine.Occurrence, changes ...engine.Change) {
	t.Helper()
	if err := e.Submit(context.Background(), &engine.Proposal{CommandID: id, ActorID: "world_agent", BaseRevision: e.State().Revision, Type: "adjudication", Occurrences: occurrences, Changes: changes, Reason: "AUTHOR-PRIVATE-CANARY"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.registry.Reconcile(e.State()); err != nil {
		t.Fatal(err)
	}
}

func TestVisibilityFiveActorAudienceAndFaithfulSpeech(t *testing.T) {
	dir, e, p := visibilityFixture(t)
	commitVisible(t, e, p, "observations", []engine.Occurrence{
		{Kind: "observation", Actor: "沈砚", Content: "A-PRIVATE-CANARY", Location: "白室", Audience: []string{"沈砚"}},
		{Kind: "observation", Actor: "罗绮", Content: "B-PRIVATE-CANARY", Location: "白室", Audience: []string{"罗绮"}},
	})
	commitVisible(t, e, p, "whisper", []engine.Occurrence{{Kind: "utterance", Actor: "顾闻", Content: "我只有一张牌（可能是谎话）", Location: "白室", Audience: []string{"沈砚", "顾闻"}}})
	// Subsequent movement must not rewrite who heard a historical utterance.
	commitVisible(t, e, p, "move", nil, engine.Change{Path: "entities.沈砚.location", Op: "set", Value: "走廊"})
	before := migrationTree(t, dir)
	packet, err := p.Preview("沈砚")
	if err != nil || len(packet.Observations) != 2 {
		t.Fatal("wrong local packet", err)
	}
	if !bytes.Equal(mustJSON(t, before), mustJSON(t, migrationTree(t, dir))) {
		t.Fatal("preview wrote files")
	}
	if packet.Observations[1].Speaker != "顾闻" || packet.Observations[1].Kind != "public_statement" || packet.Observations[1].Text != "我只有一张牌（可能是谎话）" {
		t.Fatal("speech was rewritten or promoted to fact")
	}
	report, err := p.DeliverAll()
	if err != nil || report.Added != 4 || report.Recipients != 3 || report.IgnoredEntries != 1 {
		t.Fatal("wrong delivery report", report, err)
	}
	for id := range fiveCharacters().Entities {
		s, _ := NewCognitionStore(dir, id)
		state, err := s.Read()
		if err != nil {
			t.Fatal(err)
		}
		data := mustJSON(t, state)
		if bytes.Contains(data, []byte("AUTHOR-PRIVATE-CANARY")) {
			t.Fatal("author reason leaked")
		}
		if id != "沈砚" && bytes.Contains(data, []byte("A-PRIVATE-CANARY")) {
			t.Fatal("A private view leaked")
		}
		if id != "罗绮" && bytes.Contains(data, []byte("B-PRIVATE-CANARY")) {
			t.Fatal("B private view leaked")
		}
		if id != "沈砚" && id != "顾闻" && bytes.Contains(data, []byte("可能是谎话")) {
			t.Fatal("same room expanded whisper audience")
		}
		if len(state.Records) != 0 {
			t.Fatal("projection fabricated beliefs")
		}
	}
	after := migrationTree(t, dir)
	if report, err := p.DeliverAll(); err != nil || report.Added != 0 || report.Unchanged != 4 {
		t.Fatal("replay duplicated receipts", err)
	}
	if !bytes.Equal(mustJSON(t, after), mustJSON(t, migrationTree(t, dir))) {
		t.Fatal("replay changed files")
	}
	// Second-hand information requires another explicit, committed utterance.
	commitVisible(t, e, p, "relay", []engine.Occurrence{{Kind: "utterance", Actor: "罗绮", Content: "B-PRIVATE-CANARY", Location: "白室", Audience: []string{"叶晴"}}})
	if _, err := p.Deliver("叶晴"); err != nil {
		t.Fatal(err)
	}
	s, _ := NewCognitionStore(dir, "叶晴")
	state, _ := s.Read()
	if len(state.Observations) != 1 || state.Observations[0].Speaker != "罗绮" {
		t.Fatal("relay source not preserved")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVisibilityRejectsUnsubmittedFutureAndAmbiguousEvidence(t *testing.T) {
	dir, e, p := visibilityFixture(t)
	commitVisible(t, e, p, "actual", []engine.Occurrence{{Kind: "observation", Actor: "沈砚", Content: "committed", Location: "白室", Audience: []string{"沈砚"}}})
	// The journal has a later append, but registry publication has not advanced.
	if err := e.Submit(context.Background(), &engine.Proposal{CommandID: "future", ActorID: "world_agent", BaseRevision: e.State().Revision, Type: "adjudication", Occurrences: []engine.Occurrence{{Kind: "observation", Actor: "沈砚", Content: "FUTURE-PRIVATE-CANARY", Location: "白室", Audience: []string{"沈砚"}}}}); err != nil {
		t.Fatal(err)
	}
	packet, err := p.Preview("沈砚")
	if err != nil || len(packet.Observations) != 1 || strings.Contains(string(mustJSON(t, packet)), "FUTURE") {
		t.Fatal("unpublished journal tail exposed", err)
	}
	if _, err := p.registry.Reconcile(e.State()); err != nil {
		t.Fatal(err)
	}
	if report, err := p.DeliverAll(); err != nil || report.Added != 2 {
		t.Fatal("published tail not delivered", err)
	}
	// A first-person observation cannot be copied to another actor.
	commitVisible(t, e, p, "ambiguous", []engine.Occurrence{{Kind: "observation", Actor: "沈砚", Content: "ambiguous", Location: "白室", Audience: []string{"沈砚", "罗绮"}}})
	before := migrationTree(t, dir)
	if _, err := p.DeliverAll(); err == nil {
		t.Fatal("ambiguous observation accepted")
	}
	if !bytes.Equal(mustJSON(t, before), mustJSON(t, migrationTree(t, dir))) {
		t.Fatal("invalid audience caused partial write")
	}
}

func TestVisibilityBranchMismatchIsNotOverwritten(t *testing.T) {
	dir, e, p := visibilityFixture(t)
	commitVisible(t, e, p, "one", []engine.Occurrence{{Kind: "observation", Actor: "沈砚", Content: "original", Location: "白室", Audience: []string{"沈砚"}}})
	if _, err := p.DeliverAll(); err != nil {
		t.Fatal(err)
	}
	s, _ := NewCognitionStore(dir, "沈砚")
	before := mustCognitionBytes(t, s.path())
	journal := mustCognitionBytes(t, filepath.Join(dir, "event_log.jsonl"))
	journal = bytes.ReplaceAll(journal, []byte("original"), []byte("REPLACED-PRIVATE-CANARY"))
	if err := os.WriteFile(filepath.Join(dir, "event_log.jsonl"), journal, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeliverAll(); err == nil || strings.Contains(err.Error(), "PRIVATE-CANARY") {
		t.Fatal("same revision branch mismatch accepted or leaked")
	}
	if !bytes.Equal(before, mustCognitionBytes(t, s.path())) {
		t.Fatal("branch mismatch overwrote memory")
	}
	if err := os.Remove(filepath.Join(dir, "event_log.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeliverAll(); err == nil {
		t.Fatal("missing old evidence accepted")
	}
}

func TestVisibilityPartialIOFailureRetryAndCorruptActorPreflight(t *testing.T) {
	dir, e, p := visibilityFixture(t)
	for id := range fiveCharacters().Entities {
		commitVisible(t, e, p, "o-"+id, []engine.Occurrence{{Kind: "observation", Actor: id, Content: id + "-PRIVATE-CANARY", Location: "白室", Audience: []string{id}}})
	}
	s, _ := NewCognitionStore(dir, "顾闻")
	if err := os.Mkdir(s.path()+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeliverAll(); err == nil {
		t.Fatal("I/O failure reported success")
	}
	if err := os.Remove(s.path() + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeliverAll(); err != nil {
		t.Fatal("retry failed", err)
	}
	for id := range fiveCharacters().Entities {
		actor, _ := NewCognitionStore(dir, id)
		state, err := actor.Read()
		if err != nil || len(state.Observations) != 1 {
			t.Fatal("retry duplicated or lost memory", err)
		}
	}
	commitVisible(t, e, p, "next", []engine.Occurrence{{Kind: "utterance", Actor: "沈砚", Content: "new", Location: "白室", Audience: []string{"罗绮"}}})
	if err := os.WriteFile(s.path(), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	before := migrationTree(t, dir)
	if _, err := p.DeliverAll(); err == nil {
		t.Fatal("corrupt actor ignored")
	}
	if !bytes.Equal(mustJSON(t, before), mustJSON(t, migrationTree(t, dir))) {
		t.Fatal("preflight failed after another actor was written")
	}
}

func TestVisibilityRejectsMalformedAndUnknownAudienceWithoutLeaking(t *testing.T) {
	for _, mode := range []string{"duplicate-json", "partial-line", "missing-audience", "unknown-audience", "unknown-kind", "gap"} {
		t.Run(mode, func(t *testing.T) {
			dir, e, p := visibilityFixture(t)
			commitVisible(t, e, p, "one", []engine.Occurrence{{Kind: "observation", Actor: "沈砚", Content: "PRIVATE-CANARY", Location: "白室", Audience: []string{"沈砚"}}})
			path := filepath.Join(dir, "event_log.jsonl")
			original := mustCognitionBytes(t, path)
			var entry engine.LogEntry
			if json.Unmarshal(bytes.TrimSpace(original), &entry) != nil {
				t.Fatal("fixture invalid")
			}
			switch mode {
			case "missing-audience":
				entry.Occurrences[0].Audience = nil
			case "unknown-audience":
				entry.Occurrences[0].Audience = []string{"unknown"}
			case "unknown-kind":
				entry.Occurrences[0].Kind = "author_plan"
			}
			data := append(mustJSON(t, entry), '\n')
			if mode == "duplicate-json" {
				data = append([]byte(`{"revision":1,`), data[1:]...)
			}
			if mode == "partial-line" {
				data = data[:len(data)-1]
			}
			if mode == "gap" {
				entry.Revision += 2
				entry.CommandID = "later"
				data = append(data, append(mustJSON(t, entry), '\n')...)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			before := migrationTree(t, dir)
			if _, err := p.DeliverAll(); err == nil || strings.Contains(err.Error(), "PRIVATE-CANARY") {
				t.Fatal("invalid journal accepted or private text echoed")
			}
			if !bytes.Equal(mustJSON(t, before), mustJSON(t, migrationTree(t, dir))) {
				t.Fatal("bad journal changed memory")
			}
		})
	}
}

func TestVisibilityMissingJournalDoesNotUseAuthorOrLegacyFallbacks(t *testing.T) {
	dir, _, p := visibilityFixture(t)
	for _, name := range []string{"agents_memory.json", "chronicle.jsonl", "worldbook.md", "world_state.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("AUTHOR-AND-LEGACY-PRIVATE-CANARY"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := migrationTree(t, dir)
	if packet, err := p.Preview("沈砚"); err != nil || len(packet.Observations) != 0 {
		t.Fatal("invented missing observations", err)
	}
	if report, err := p.DeliverAll(); err != nil || report.Added != 0 || !report.MissingJournal {
		t.Fatal("missing journal not diagnosed", err)
	}
	if !bytes.Equal(mustJSON(t, before), mustJSON(t, migrationTree(t, dir))) {
		t.Fatal("fallback sources changed cognition")
	}
	if _, err := p.Preview("unknown"); err == nil {
		t.Fatal("unknown actor accepted")
	}
}
