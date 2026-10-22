package novel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"worldsim/internal/fsutil"
	"worldsim/internal/llm"
	"worldsim/internal/sim"
)

type Publication struct {
	POV             string      `json:"pov"`
	ReaderKnowledge string      `json:"reader_knowledge"` // Chapter summary, distinct from character belief and author intent.
	Version         string      `json:"version"`
	Plan            ChapterPlan `json:"plan"`
	Title           string      `json:"title"`
	Body            string      `json:"body"`
	Summary         string      `json:"summary"`
	SourceIDs       []string    `json:"source_ids"`
	SourceDigest    string      `json:"source_digest"`
	// Structural validation cannot certify literary quality or semantic truth.
	Review  string            `json:"review"`
	Quality map[string]string `json:"quality"`
}

func (w *Writer) ReadPlans() ([]ChapterPlan, error) {
	b, err := os.ReadFile(filepath.Join(w.BookDir, "chapter_plans.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p []ChapterPlan
	err = json.Unmarshal(b, &p)
	return p, err
}

// EnsurePlans runs on the explicit generation command only. Published boundaries stay fixed.
func (w *Writer) EnsurePlans(ctx context.Context, entries []sim.ChronicleEntry) ([]ChapterPlan, error) {
	plans, err := w.ReadPlans()
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		legacy, _ := filepath.Glob(filepath.Join(w.BookDir, "chapters", "*.md"))
		if len(legacy) > 0 {
			return nil, fmt.Errorf("旧章节没有持久化分章边界；请在版本副本中确认边界与摘要后迁移，原章节保持不变")
		}
	}
	end := 0
	for _, p := range plans {
		if p.DayEnd > end {
			end = p.DayEnd
		}
	}
	var fresh []sim.ChronicleEntry
	for _, e := range entries {
		if e.Day > end && w.EligibleMaterial(e) {
			fresh = append(fresh, e)
		}
	}
	if len(fresh) == 0 {
		if len(plans) == 0 {
			return nil, fmt.Errorf("没有带已提交事件来源的可验证章节素材；旧编年史仅可诊断，不能直接发布")
		}
		return plans, nil
	}
	next := w.PlanChapters(ctx, fresh, nil)
	for i := range next {
		next[i].Num = len(plans) + 1
		next[i].SourceDigest = chapterDigest(next[i], entries)
		plans = append(plans, next[i])
	}
	if err := os.MkdirAll(w.BookDir, 0755); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(plans, "", "  ")
	if err != nil {
		return nil, err
	}
	err = fsutil.WriteFileAtomic(filepath.Join(w.BookDir, "chapter_plans.json"), b)
	return plans, err
}

func chapterDigest(p ChapterPlan, entries []sim.ChronicleEntry) string {
	var ids []string
	for _, e := range entries {
		if e.Day >= p.DayStart && e.Day <= p.DayEnd && len(e.SourceEvents) > 0 {
			ids = append(ids, e.SourceEvents...)
		}
	}
	b, _ := json.Marshal(ids)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func validateChapter(p ChapterPlan, res llm.CompletionResult) (body, summary, title string, err error) {
	if res.FinishReason != "stop" {
		return "", "", "", fmt.Errorf("章节未正常结束（finish_reason=%s），未发布", res.FinishReason)
	}
	text := strings.TrimSpace(res.Content)
	if !utf8.ValidString(text) || strings.Contains(text, "```") {
		return "", "", "", fmt.Errorf("章节格式异常")
	}
	_, body, summary = splitSummary(text)
	prefix := fmt.Sprintf("第%d章·", p.Num)
	first, _, found := strings.Cut(body, "\n")
	if !found || !strings.HasPrefix(first, prefix) {
		return "", "", "", fmt.Errorf("章节标题或正文缺失")
	}
	title = strings.TrimSpace(strings.TrimPrefix(first, prefix))
	if title == "" || utf8.RuneCountInString(summary) < 20 || strings.HasPrefix(summary, "第") && !strings.ContainsAny(summary, "。；，") {
		return "", "", "", fmt.Errorf("章节缺少有效标题或事实摘要")
	}
	if !strings.HasSuffix(body, "【本章结束】") {
		return "", "", "", fmt.Errorf("章节缺少结束标记，未发布")
	}
	body = strings.TrimSpace(strings.TrimSuffix(body, "【本章结束】"))
	if utf8.RuneCountInString(body) < 80 {
		return "", "", "", fmt.Errorf("章节正文不足以构成场景")
	}
	last := strings.TrimRight(body, "”’\"」』）) \r\n")
	if !strings.HasSuffix(last, "。") && !strings.HasSuffix(last, "！") && !strings.HasSuffix(last, "？") && !strings.HasSuffix(last, "…") && !strings.HasSuffix(last, ".") && !strings.HasSuffix(last, "!") && !strings.HasSuffix(last, "?") {
		return "", "", "", fmt.Errorf("章节结尾疑似断句，未发布")
	}
	return body, summary, title, nil
}

func (w *Writer) publish(p ChapterPlan, res llm.CompletionResult, entries []sim.ChronicleEntry) (string, error) {
	body, summary, title, err := validateChapter(p, res)
	if err != nil {
		return "", err
	}
	if p.SourceDigest != "" && p.SourceDigest != chapterDigest(p, entries) {
		return "", fmt.Errorf("分章来源已变化，请创建新的作品分支")
	}
	pub := Publication{Version: fmt.Sprintf("%d", time.Now().UnixNano()), Plan: p, Title: title, Body: body, Summary: summary, SourceDigest: chapterDigest(p, entries), Review: "automatic_gates_passed; semantic_review_required", Quality: map[string]string{
		"completion": "pass", "format": "pass", "provenance": "pass", "continuity": "pass",
		"fact_semantics": "pending_review", "pov_semantics": "pending_review", "foreshadow_fairness": "pending_review", "arc_effect": "pending_review", "rhythm": "pending_review",
	}}
	pub.POV = w.HeroName
	pub.ReaderKnowledge = summary
	for _, e := range entries {
		if e.Day >= p.DayStart && e.Day <= p.DayEnd && w.EligibleMaterial(e) {
			pub.SourceIDs = append(pub.SourceIDs, e.SourceEvents...)
		}
	}
	if len(pub.SourceIDs) == 0 {
		return "", fmt.Errorf("章节无来源事件")
	}
	chDir := filepath.Join(w.BookDir, "chapters")
	if err := os.MkdirAll(chDir, 0755); err != nil {
		return "", err
	}
	paths, err := filepath.Glob(filepath.Join(chDir, fmt.Sprintf("%03d_*.md", p.Num)))
	if err != nil {
		return "", err
	}
	if len(paths) > 1 {
		return "", fmt.Errorf("章号 %d 有多个旧文件，需要人工确认", p.Num)
	}
	path := filepath.Join(chDir, fmt.Sprintf("%03d_%s.md", p.Num, sanitize(title)))
	var previous []byte
	if len(paths) == 1 {
		path = paths[0] // Keep the legacy filename to avoid duplicate chapter numbers.
		old, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if string(old) == body {
			return "", fmt.Errorf("输出与旧章节完全重复，未发布")
		}
		previous = old
	}
	if duplicate, other := w.nearDuplicate(body, p.Num); duplicate {
		return "", fmt.Errorf("正文与第%d章高度重复，未发布", other)
	}
	// Only validated candidates enter immutable version storage.
	versionDir := filepath.Join(w.BookDir, "versions", fmt.Sprintf("%03d", p.Num), pub.Version)
	if err := os.MkdirAll(versionDir, 0755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(pub, "", "  ")
	if err != nil {
		return "", err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(versionDir, "publication.json"), data); err != nil {
		return "", err
	}
	if previous != nil {
		if err := fsutil.WriteFileAtomic(filepath.Join(versionDir, "previous.md"), previous); err != nil {
			return "", err
		}
	}
	current := filepath.Join(w.BookDir, "published")
	if err := os.MkdirAll(current, 0755); err != nil {
		return "", err
	}
	old, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", readErr
	}
	if err := fsutil.WriteFileAtomic(path, []byte(body)); err != nil {
		return "", err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(current, fmt.Sprintf("%03d.json", p.Num)), data); err != nil {
		var rollbackErr error
		if readErr == nil {
			rollbackErr = fsutil.WriteFileAtomic(path, old)
		} else {
			rollbackErr = os.Remove(path)
		}
		return "", fmt.Errorf("发布元数据失败: %w；投影恢复结果: %v；不可变版本位于 %s", err, rollbackErr, versionDir)
	}
	w.updatePrevSummary(p.Num, title, summary)
	return body, nil
}

// Unknown legacy provenance remains diagnosable; it is not silently promoted to fact.
func (w *Writer) EligibleMaterial(e sim.ChronicleEntry) bool {
	if len(e.SourceEvents) == 0 {
		return false
	}
	if e.Kind == "CANDIDATE" || e.Kind == "PLAN" || e.Source == "导演" || e.Source == "反思" {
		return false
	}
	if e.Visibility == "private" || e.Visibility == "restricted" {
		for _, name := range e.Audience {
			if name == w.HeroName {
				return true
			}
		}
		return false
	}
	return e.Kind == "FACT" || e.Kind == "STATE" || e.Kind == "SAID" || e.Kind == "OBSERVATION"
}

func (w *Writer) nearDuplicate(body string, chapter int) (bool, int) {
	normalize := func(s string) map[string]bool {
		r := []rune(strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) || strings.ContainsRune("，。！？；：、,.!?;:\"'“”‘’（）()【】", r) {
				return -1
			}
			return r
		}, s))
		out := map[string]bool{}
		for i := 0; i+20 <= len(r); i += 10 {
			out[string(r[i:i+20])] = true
		}
		return out
	}
	a := normalize(body)
	if len(a) == 0 {
		return false, 0
	}
	paths, _ := filepath.Glob(filepath.Join(w.BookDir, "published", "*.json"))
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var p Publication
		if json.Unmarshal(b, &p) != nil || p.Plan.Num == chapter {
			continue
		}
		other := normalize(p.Body)
		matches := 0
		for k := range a {
			if other[k] {
				matches++
			}
		}
		denom := len(a)
		if len(other) < denom {
			denom = len(other)
		}
		if denom > 0 && float64(matches)/float64(denom) >= 0.75 {
			return true, p.Plan.Num
		}
	}
	return false, 0
}

func (w *Writer) CheckContinuityBefore(num int) error {
	for n := 1; n < num; n++ {
		b, err := os.ReadFile(filepath.Join(w.BookDir, "published", fmt.Sprintf("%03d.json", n)))
		if err != nil {
			return fmt.Errorf("第%d章缺少可验证版本及摘要，暂停续写；请先在副本中恢复或审阅旧章", n)
		}
		var p Publication
		if json.Unmarshal(b, &p) != nil || p.Summary == "" || p.Body == "" || p.Plan.Num != n {
			return fmt.Errorf("第%d章发布记录损坏", n)
		}
		if p.POV != "" && p.POV != w.HeroName {
			return fmt.Errorf("叙事视角变化需要新的作品分支")
		}
	}
	return nil
}

// LoadSummariesBefore never injects a later chapter into a rewrite of an earlier one.
func (w *Writer) LoadSummariesBefore(num int) {
	w.PrevSummary = ""
	paths, _ := filepath.Glob(filepath.Join(w.BookDir, "published", "*.json"))
	sort.Strings(paths)
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var p Publication
		if json.Unmarshal(b, &p) == nil && p.Plan.Num < num {
			w.updatePrevSummary(p.Plan.Num, p.Title, p.Summary)
		}
	}
}
