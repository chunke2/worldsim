package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"worldsim/internal/engine"
	"worldsim/internal/fsutil"
	"worldsim/internal/narrative"
)

// RunDay performs the narrative guard before any model call. Assessment is a
// conservative evidence check, not a claim that a quiet scene has no value.
func (s *Simulator) RunDay(ctx context.Context) (result *DayResult, resultErr error) {
	if _, _, enabled, err := s.independentSettings(); err != nil {
		return nil, err
	} else if enabled {
		return s.runIndependentDay(ctx)
	}
	if err := s.ReconcileCharacterAgents(); err != nil {
		return nil, fmt.Errorf("角色 Agent 注册失败: %w", err)
	}
	// Even a partially committed turn must reconcile deaths/new characters.
	defer func() {
		if err := s.ReconcileCharacterAgents(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("世界可能已提交，角色 Agent 同步待重试: %w", err))
		}
	}()
	journal, err := narrative.ReadJournal(s.worldDir)
	if err != nil {
		return nil, err
	}
	plan, err := narrative.Load(s.worldDir)
	if err != nil {
		return nil, err
	}
	progress := narrative.AssessAfterReview(journal, s.day, plan)
	if progress.Stalled {
		return nil, fmt.Errorf("叙事停滞风险：连续三个周期缺少可验证推进；已暂停重复生成。请读取 /api/world/narrative 的压力候选并审阅证据，确认后记录 review_reason 与 reviewed_revision/day")
	}
	res, runErr := s.runDay(ctx)
	if runErr != nil {
		return res, runErr
	}
	journal, err = narrative.ReadJournal(s.worldDir)
	if err != nil {
		return res, err
	}
	progress = narrative.AssessAfterReview(journal, s.day, plan)
	b, err := json.MarshalIndent(progress, "", "  ")
	if err != nil {
		return res, err
	}
	if err = fsutil.WriteFileAtomic(filepath.Join(s.worldDir, "narrative_progress.json"), b); err != nil {
		return res, err
	}
	if progress.Stalled {
		res.Paused = true
		res.PauseMsg = "叙事停滞风险：暂停继续生成，等待世界内压力方案及证据审阅"
	}
	return res, nil
}

func (s *Simulator) commitClock(ctx context.Context, day int) error {
	if s.engine.State().Day == day {
		return nil
	}
	return s.engine.Submit(ctx, &engine.Proposal{CommandID: s.nextCmd("clock"), ActorID: "world_agent", BaseRevision: s.engine.State().Revision, Type: "adjudication", Reason: "advance simulation clock", Changes: []engine.Change{{Path: "world_level.day", Op: "set", Value: day}}})
}
