// Package diagnostic performs read-only world data audits.
package diagnostic

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"worldsim/internal/engine"
	"worldsim/internal/narrative"
	"worldsim/internal/sim"
)

type Finding struct {
	Severity string `json:"severity"`
	Layer    string `json:"layer"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Count    int    `json:"count,omitempty"`
	Recovery string `json:"recovery"`
}

type Report struct {
	World             string                 `json:"world"`
	Revision          int                    `json:"revision"`
	Day               int                    `json:"day"`
	Protagonist       string                 `json:"protagonist,omitempty"`
	NarrativePOV      string                 `json:"narrative_pov,omitempty"`
	Entities          int                    `json:"entities"`
	JournalEntries    int                    `json:"journal_entries"`
	ChronicleEntries  int                    `json:"chronicle_entries"`
	LegacyChapters    int                    `json:"legacy_chapters"`
	PublishedChapters int                    `json:"published_chapters"`
	Progress          narrative.Assessment   `json:"narrative_progress"`
	Findings          []Finding              `json:"findings"`
	FileHashes        map[string]string      `json:"file_hashes"`
	Metrics           map[string]interface{} `json:"metrics"`
	ReadOnly          bool                   `json:"read_only"`
}

func Hashes(dir string) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out[e.Name()] = fmt.Sprintf("%x", sha256.Sum256(b))
	}
	return out, nil
}

func Analyze(dir string) (Report, error) {
	r := Report{World: filepath.Base(dir), Findings: []Finding{}, FileHashes: map[string]string{}, Metrics: map[string]interface{}{}, ReadOnly: true}
	before, err := Hashes(dir)
	if err != nil {
		return r, err
	}
	r.FileHashes = before
	b, err := os.ReadFile(filepath.Join(dir, "world_state.json"))
	if err != nil {
		return r, err
	}
	var state engine.WorldState
	if err := json.Unmarshal(b, &state); err != nil {
		return r, fmt.Errorf("world_state: %w", err)
	}
	r.Revision, r.Day, r.Entities = state.Revision, state.Day, len(state.Entities)
	hero, pov, identityErr := engine.ResolveIdentity(&state)
	if identityErr != nil {
		r.Findings = append(r.Findings, Finding{"critical", "world_fact", "IDENTITY_UNRESOLVED", identityErr.Error(), 1, "在迁移副本中显式写入 world_level.protagonist 与 narrative_pov，并确认两者引用现有实体。"})
	} else {
		r.Protagonist, r.NarrativePOV = hero, pov
	}

	emptyEntities, badLocations, badRelations := 0, 0, 0
	for name, ent := range state.Entities {
		if strings.TrimSpace(name) == "" {
			emptyEntities++
		}
		if ent.Location != "" {
			if _, ok := state.WorldLevel.Locations[ent.Location]; !ok {
				badLocations++
			}
		}
		for other := range ent.Relationship {
			if other == "" {
				badRelations++
			} else if _, ok := state.Entities[other]; !ok {
				badRelations++
			}
		}
	}
	suspiciousLocations := 0
	for name, loc := range state.WorldLevel.Locations {
		if len([]rune(name)) > 40 || strings.Contains(name, "【】") || strings.TrimSpace(loc.Name) == "" || loc.Name != name {
			suspiciousLocations++
		}
	}
	if emptyEntities > 0 {
		r.Findings = append(r.Findings, Finding{"critical", "world_fact", "EMPTY_ENTITY", "存在空实体 ID。", emptyEntities, "复制世界后删除空实体，并逐条人工重定向其关系与状态；原世界保持只读。"})
	}
	if badLocations+suspiciousLocations > 0 {
		r.Findings = append(r.Findings, Finding{"error", "world_fact", "LOCATION_POLLUTION", "实体引用不存在地点，或地点键混入场景文本。", badLocations + suspiciousLocations, "在迁移副本建立地点别名表；只合并经人工确认的同一地点，保留旧键映射。"})
	}
	if badRelations > 0 {
		r.Findings = append(r.Findings, Finding{"error", "character_cognition", "RELATION_POLLUTION", "关系图含空角色或不存在角色。", badRelations, "在迁移副本隔离无效边，并保留原值和来源日志供审计。"})
	}

	journal, journalErr := narrative.ReadJournal(dir)
	if journalErr != nil {
		r.Findings = append(r.Findings, Finding{"critical", "world_fact", "JOURNAL_INVALID", journalErr.Error(), 1, "从最后一个连续 revision 建立恢复分支；不要覆盖原日志。"})
	} else {
		r.JournalEntries = len(journal)
		missingReason, missingSources, invalidPaths := 0, 0, 0
		for _, e := range journal {
			if strings.TrimSpace(e.Reason) == "" {
				missingReason++
			}
			if len(e.SourceEvents) == 0 && len(e.Occurrences) == 0 {
				missingSources++
			}
			for _, c := range e.Changes {
				if strings.Contains(c.Path, "entities..") {
					invalidPaths++
				}
			}
		}
		r.Metrics["journal_missing_reason"] = missingReason
		r.Metrics["journal_missing_sources"] = missingSources
		r.Metrics["journal_invalid_entity_paths"] = invalidPaths
		if invalidPaths > 0 {
			r.Findings = append(r.Findings, Finding{"critical", "world_fact", "INVALID_ENTITY_PATH", "旧日志包含 entities.. 空实体路径，不能可靠重放为角色状态。", invalidPaths, "迁移时将这些变更放入 quarantine，不猜测目标角色。"})
		}
		if missingSources > 0 {
			r.Findings = append(r.Findings, Finding{"warning", "world_fact", "LEGACY_UNSOURCED_EVENTS", "旧提交缺少来源事件或可观察证据。", missingSources, "标记为 legacy_unverified；不得自动升级为角色知识或已兑现伏笔。"})
		}
		r.Progress = narrative.Assess(journal, state.Day)
	}

	chroniclePath := filepath.Join(dir, "chronicle.jsonl")
	if f, err := os.Open(chroniclePath); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 4*1024*1024)
		missingSources, privatePublic := 0, 0
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == "" {
				continue
			}
			var e sim.ChronicleEntry
			if json.Unmarshal(scanner.Bytes(), &e) != nil {
				continue
			}
			r.ChronicleEntries++
			if len(e.SourceEvents) == 0 {
				missingSources++
			}
			if e.Visibility == "public" && (e.Source == "反思" || e.Kind == "PLAN") {
				privatePublic++
			}
		}
		r.Metrics["chronicle_missing_sources"] = missingSources
		r.Metrics["private_or_plan_marked_public"] = privatePublic
		if missingSources > 0 {
			r.Findings = append(r.Findings, Finding{"warning", "novel_writing", "CHRONICLE_PROVENANCE_MISSING", "编年史条目无法追溯到不可变事件。", missingSources, "旧条目仅作为候选素材；新章节要求来源 ID。"})
		}
	}

	legacy, _ := filepath.Glob(filepath.Join(dir, "novel", "chapters", "*.md"))
	published, _ := filepath.Glob(filepath.Join(dir, "novel", "published", "*.json"))
	r.LegacyChapters, r.PublishedChapters = len(legacy), len(published)
	if r.LegacyChapters > r.PublishedChapters {
		r.Findings = append(r.Findings, Finding{"warning", "novel_writing", "LEGACY_CHAPTERS", "现有章节缺少版本化发布元数据、可靠摘要或来源清单。", r.LegacyChapters - r.PublishedChapters, "保留原章；在版本副本中人工确认 POV、摘要、来源日区间后登记为 legacy-reviewed。"})
	}
	after, err := Hashes(dir)
	if err != nil {
		return r, err
	}
	if !equalHashes(before, after) {
		return r, fmt.Errorf("read-only audit detected changed source files")
	}
	sort.SliceStable(r.Findings, func(i, j int) bool { return severity(r.Findings[i].Severity) < severity(r.Findings[j].Severity) })
	return r, nil
}

func equalHashes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func severity(s string) int {
	switch s {
	case "critical":
		return 0
	case "error":
		return 1
	case "warning":
		return 2
	}
	return 3
}
