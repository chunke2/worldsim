// Package narrative owns author intentions and reader knowledge, never world truth.
package narrative

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"worldsim/internal/engine"
	"worldsim/internal/fsutil"
)

type Thread struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`            // question, secret, foreshadow, conflict, arc, promise
	Scope          string   `json:"scope,omitempty"` // main, subplot, character
	Description    string   `json:"description"`
	Status         string   `json:"status"` // planned, planted, reinforced, resolved
	SourceEvents   []string `json:"source_events"`
	ReaderChapters []int    `json:"reader_chapters"`
	Characters     []string `json:"characters"`
	NextCondition  string   `json:"next_condition"`
	DueChapter     int      `json:"due_chapter,omitempty"`
}

type ChapterCandidate struct {
	ID               string   `json:"id"`
	SourceEvents     []string `json:"source_events"`
	POV              string   `json:"pov"`
	EffectiveChange  string   `json:"effective_change"`
	OpeningQuestion  string   `json:"opening_question,omitempty"`
	ClosingQuestion  string   `json:"closing_question,omitempty"`
	DelayedQuestions []string `json:"delayed_questions,omitempty"`
	Escalations      []string `json:"escalations,omitempty"`
	PublishedChapter int      `json:"published_chapter,omitempty"`
}

type Pressure struct {
	ID           string   `json:"id"`
	Reason       string   `json:"reason"`
	Condition    string   `json:"condition"`
	Cost         string   `json:"cost"`
	Characters   []string `json:"characters"`
	Status       string   `json:"status"` // proposed; accepted/fulfilled need world event evidence
	SourceEvents []string `json:"source_events"`
}

type State struct {
	ReviewedRevision   int                `json:"reviewed_revision,omitempty"`
	ReviewedDay        int                `json:"reviewed_day,omitempty"`
	ReviewReason       string             `json:"review_reason,omitempty"`
	Version            int                `json:"version"`
	Theme              string             `json:"theme"`
	ReaderPromise      string             `json:"reader_promise"`
	CurrentTension     float64            `json:"current_tension,omitempty"`
	CurrentRisk        string             `json:"current_risk,omitempty"`
	Costs              []string           `json:"costs,omitempty"`
	InformationGaps    []string           `json:"information_gaps,omitempty"`
	RandomnessBoundary string             `json:"randomness_boundary,omitempty"`
	Threads            []Thread           `json:"threads"`
	Pressures          []Pressure         `json:"pressures"`
	ChapterCandidates  []ChapterCandidate `json:"chapter_candidates,omitempty"`
}

func Load(dir string) (State, error) {
	s := State{Version: 1, Threads: []Thread{}, Pressures: []Pressure{}}
	b, err := os.ReadFile(filepath.Join(dir, "narrative.json"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

// SavePlan is an author-only command; it cannot mark events or revelations as completed.
func SavePlan(dir string, s State, world *engine.WorldState, eventIDs map[string]bool, chapters map[int]bool) error {
	if s.ReviewedRevision < 0 || s.ReviewedRevision > world.Revision || s.ReviewedDay < 0 || s.ReviewedDay > world.Day {
		return fmt.Errorf("invalid review boundary")
	}
	if s.ReviewedRevision > 0 && strings.TrimSpace(s.ReviewReason) == "" {
		return fmt.Errorf("manual review needs a reason")
	}
	if s.Version != 1 {
		return fmt.Errorf("unsupported narrative schema")
	}
	if s.CurrentTension < 0 || s.CurrentTension > 1 {
		return fmt.Errorf("narrative tension must be 0..1")
	}
	seen := map[string]bool{}
	for _, t := range s.Threads {
		if t.ID == "" || seen[t.ID] || strings.TrimSpace(t.Description) == "" {
			return fmt.Errorf("invalid/duplicate narrative thread: %s", t.ID)
		}
		seen[t.ID] = true
		switch t.Kind {
		case "question", "secret", "foreshadow", "conflict", "arc", "promise":
		default:
			return fmt.Errorf("invalid thread kind")
		}
		if t.Scope != "" && t.Scope != "main" && t.Scope != "subplot" && t.Scope != "character" {
			return fmt.Errorf("invalid thread scope")
		}
		switch t.Status {
		case "planned", "planted", "reinforced", "misdirected", "payoff", "recovered", "resolved":
		default:
			return fmt.Errorf("invalid thread status")
		}
		if t.Status != "planned" && len(t.SourceEvents) == 0 {
			return fmt.Errorf("%s needs committed event evidence", t.ID)
		}
		for _, id := range t.SourceEvents {
			if !eventIDs[id] {
				return fmt.Errorf("unknown event: %s", id)
			}
		}
		for _, n := range t.ReaderChapters {
			if !chapters[n] {
				return fmt.Errorf("reader knowledge requires a published chapter: %d", n)
			}
		}
		for _, name := range t.Characters {
			if _, ok := world.Entities[name]; !ok || name == "" {
				return fmt.Errorf("unknown character: %s", name)
			}
		}
	}
	for _, c := range s.ChapterCandidates {
		if c.ID == "" || seen[c.ID] || strings.TrimSpace(c.EffectiveChange) == "" || len(c.SourceEvents) == 0 {
			return fmt.Errorf("invalid chapter candidate: %s", c.ID)
		}
		seen[c.ID] = true
		if _, ok := world.Entities[c.POV]; !ok || c.POV == "" {
			return fmt.Errorf("unknown candidate POV: %s", c.POV)
		}
		for _, id := range c.SourceEvents {
			if !eventIDs[id] {
				return fmt.Errorf("unknown candidate event: %s", id)
			}
		}
		if c.PublishedChapter > 0 && !chapters[c.PublishedChapter] {
			return fmt.Errorf("candidate references unpublished chapter: %d", c.PublishedChapter)
		}
	}
	for _, p := range s.Pressures {
		if p.ID == "" || seen[p.ID] || p.Reason == "" || p.Condition == "" {
			return fmt.Errorf("invalid pressure")
		}
		seen[p.ID] = true
		if p.Status != "proposed" {
			return fmt.Errorf("phase 1 accepts proposed pressures only; world adjudication required")
		}
		for _, name := range p.Characters {
			if _, ok := world.Entities[name]; !ok || name == "" {
				return fmt.Errorf("unknown pressure character")
			}
		}
		for _, id := range p.SourceEvents {
			if !eventIDs[id] {
				return fmt.Errorf("unknown pressure source")
			}
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, "narrative.json"), b)
}

// WriterContext preserves the distinction between planned payoff and established fact.
func (s State) WriterContext(chapter int) string {
	var b strings.Builder
	b.WriteString("作者意图（不是既成事实，不得写入角色知识）：\n主题：" + s.Theme + "\n读者承诺：" + s.ReaderPromise + "\n")
	fmt.Fprintf(&b, "当前张力=%.2f；风险=%s；已付/待付代价=%v；信息差=%v；偶然性边界=%s\n", s.CurrentTension, s.CurrentRisk, s.Costs, s.InformationGaps, s.RandomnessBoundary)
	for _, t := range s.Threads {
		// Even a later published reveal must not leak into an earlier chapter rewrite.
		var known []int
		for _, n := range t.ReaderChapters {
			if n < chapter {
				known = append(known, n)
			}
		}
		fmt.Fprintf(&b, "%s [%s/%s] %s；事件证据=%v；此前读者可见章节=%v；下一条件=%s；预期回收章=%d\n", t.ID, t.Kind, t.Status, t.Description, t.SourceEvents, known, t.NextCondition, t.DueChapter)
	}
	for _, c := range s.ChapterCandidates {
		if c.PublishedChapter == 0 || c.PublishedChapter == chapter {
			fmt.Fprintf(&b, "章节候选 %s：来源=%v；有效变化=%s；开场问题=%s；结尾问题=%s；延后=%v；升级=%v\n", c.ID, c.SourceEvents, c.EffectiveChange, c.OpeningQuestion, c.ClosingQuestion, c.DelayedQuestions, c.Escalations)
		}
	}
	return b.String()
}

type DayProgress struct {
	Day        int      `json:"day"`
	Evidence   []string `json:"evidence"`
	Candidates []string `json:"candidates"`
	Repeated   int      `json:"repeated"`
	Status     string   `json:"status"`
	Reason     string   `json:"reason"`
}

type Assessment struct {
	Days      []DayProgress `json:"days"`
	Stalled   bool          `json:"stalled"`
	Pressures []Pressure    `json:"pressure_candidates"`
	Limit     string        `json:"limit"`
}

// Assess measures evidenced state transitions. Prose novelty is not proof of progress.
func Assess(entries []engine.LogEntry, throughDay int) Assessment {
	a := Assessment{Days: []DayProgress{}, Pressures: []Pressure{}, Limit: "状态变更证据检查；不能自动证明文学质量或判断同义改写。旧日志缺少原因/来源时标为待核实。"}
	byDay := map[int]*DayProgress{}
	values := map[string]string{}
	// A state transition is evidenced only when it cites an earlier immutable
	// event-log entry. Arbitrary strings and self references do not qualify.
	eventRevision := map[string]int{}
	for _, e := range entries {
		eventRevision[fmt.Sprintf("event:%d:%s", e.Revision, e.CommandID)] = e.Revision
	}
	for d := 1; d <= throughDay; d++ {
		byDay[d] = &DayProgress{Day: d, Evidence: []string{}, Candidates: []string{}}
	}
	for _, e := range entries {
		d := byDay[e.Day]
		if d == nil {
			continue
		}
		for _, c := range e.Changes {
			b, _ := json.Marshal(c.Value)
			value := string(b)
			if c.Op == "set" && values[c.Path] == value {
				d.Repeated++
				continue
			}
			values[c.Path] = value
			if strings.Contains(c.Path, "tension") || strings.Contains(c.Path, "weather") || strings.Contains(c.Path, "last_seen") || strings.Contains(c.Path, "persona_sheet") || strings.HasSuffix(c.Path, ".role") {
				continue
			}
			meaningful := strings.Contains(c.Path, ".relationship.") || strings.Contains(c.Path, ".assets.") || strings.HasSuffix(c.Path, ".money") || strings.HasSuffix(c.Path, ".health") || strings.HasSuffix(c.Path, ".alive")
			id := fmt.Sprintf("event:%d:%s", e.Revision, e.CommandID)
			hasPriorEvidence := false
			for _, source := range e.SourceEvents {
				if rev, ok := eventRevision[source]; ok && rev < e.Revision {
					hasPriorEvidence = true
					break
				}
			}
			if meaningful && hasPriorEvidence && e.Reason != "" && value != "0" {
				d.Evidence = append(d.Evidence, id+" "+c.Path)
			} else {
				d.Candidates = append(d.Candidates, id+" "+c.Path)
			}
		}
	}
	streak := 0
	for day := 1; day <= throughDay; day++ {
		d := byDay[day]
		if len(d.Evidence) > 0 {
			d.Status = "evidenced_change"
			d.Reason = "有来源的资源/关系/存活状态变化，仍需叙事评估"
			streak = 0
		} else {
			streak++
			d.Status = "unverified_progress"
			d.Reason = "没有可验证的推进证据；文本变化/机械数值变化不自动视为剧情推进"
			if streak >= 3 {
				d.Status = "stagnation_risk"
			}
		}
		a.Days = append(a.Days, *d)
	}
	a.Stalled = streak >= 3
	if a.Stalled {
		a.Pressures = append(a.Pressures, Pressure{ID: fmt.Sprintf("review-day-%d", throughDay), Reason: "连续三个周期缺少可验证推进", Condition: "从已存在的未解问题或承诺中选择一项，提供可见线索或需要付出代价的选择；先验证世界规则与角色可观察性", Cost: "由世界规则确定，不得凭空扣资源", Status: "proposed", Characters: []string{}})
	}
	return a
}

func EventIndex(entries []engine.LogEntry) map[string]bool {
	m := map[string]bool{}
	for _, e := range entries {
		m[fmt.Sprintf("event:%d:%s", e.Revision, e.CommandID)] = true
	}
	return m
}

func ReadJournal(dir string) ([]engine.LogEntry, error) {
	b, err := os.ReadFile(filepath.Join(dir, "event_log.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []engine.LogEntry
	for i, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e engine.LogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("日志第%d行: %w", i+1, err)
		}
		out = append(out, e)
	}
	for i := 1; i < len(out); i++ {
		if out[i].Revision != out[i-1].Revision+1 {
			return nil, fmt.Errorf("non-contiguous journal at revision %d", out[i].Revision)
		}
	}
	return out, nil
}

// AssessAfterReview resets the guard only after an explicit, recorded author review.
func AssessAfterReview(entries []engine.LogEntry, day int, s State) Assessment {
	if s.ReviewedRevision == 0 {
		return Assess(entries, day)
	}
	var tail []engine.LogEntry
	for _, e := range entries {
		if e.Revision > s.ReviewedRevision {
			e.Day -= s.ReviewedDay
			tail = append(tail, e)
		}
	}
	a := Assess(tail, day-s.ReviewedDay)
	for i := range a.Days {
		a.Days[i].Day += s.ReviewedDay
	}
	return a
}
