package novel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"worldsim/internal/llm"
	"worldsim/internal/sim"
)

func validChapter(title string) string {
	return "第1章·" + title + "\n" + strings.Repeat("她看见门旁的刻痕，停下脚步，没有贸然拉动把手。", 6) + "\n【本章结束】\n【本章摘要】她在门旁发现了刻痕，但没有拉动把手，刻痕的含义仍然未知。"
}
func TestRejectIncompleteChapterBeforeWriting(t *testing.T) {
	w := &Writer{BookDir: t.TempDir()}
	p := ChapterPlan{Num: 1, DayStart: 1, DayEnd: 1}
	sources := []sim.ChronicleEntry{{Day: 1, Kind: "FACT", Content: "刻痕", SourceEvents: []string{"event:1:test"}}}
	cases := []llm.CompletionResult{{Content: validChapter("门"), FinishReason: "length"}, {Content: strings.Split(validChapter("门"), "【本章摘要】")[0], FinishReason: "stop"}, {Content: strings.ReplaceAll(validChapter("门"), "【本章结束】", ""), FinishReason: "stop"}}
	for _, res := range cases {
		if _, err := w.publish(p, res, sources); err == nil {
			t.Fatal("incomplete chapter accepted")
		}
	}
	files, _ := os.ReadDir(w.BookDir)
	if len(files) != 0 {
		t.Fatal("failed chapter wrote files")
	}
}
func TestVersionedPublishAndIncrementalPlans(t *testing.T) {
	w := &Writer{BookDir: t.TempDir()}
	p := ChapterPlan{Num: 1, DayStart: 1, DayEnd: 1}
	sources := []sim.ChronicleEntry{{Day: 1, Kind: "FACT", Content: "刻痕", SourceEvents: []string{"event:1:test"}}}
	for _, title := range []string{"门", "刻痕"} {
		if _, err := w.publish(p, llm.CompletionResult{Content: validChapter(title), FinishReason: "stop"}, sources); err != nil {
			t.Fatal(err)
		}
	}
	versions, _ := filepath.Glob(filepath.Join(w.BookDir, "versions", "001", "*", "publication.json"))
	if len(versions) != 2 {
		t.Fatal("versions lost")
	}
	old, _ := filepath.Glob(filepath.Join(w.BookDir, "versions", "001", "*", "previous.md"))
	if len(old) != 1 {
		t.Fatal("old chapter not backed up")
	}
	files, _ := filepath.Glob(filepath.Join(w.BookDir, "chapters", "*.md"))
	if len(files) != 1 {
		t.Fatal("duplicate chapter numbers")
	}
	os.WriteFile(filepath.Join(w.BookDir, "chapter_plans.json"), []byte(`[{"num":1,"day_start":1,"day_end":1}]`), 0644)
	plans, err := w.EnsurePlans(context.Background(), sources)
	if err != nil || len(plans) != 1 {
		t.Fatal(plans, err)
	} // nil API: cannot call LLM
	w.LoadSummariesBefore(1)
	if w.PrevSummary != "" {
		t.Fatal("future summary leaked")
	}
	w.LoadSummariesBefore(2)
	if !strings.Contains(w.PrevSummary, "刻痕") {
		t.Fatal("summary not loaded")
	}
}
