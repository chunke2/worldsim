package sim

import (
	"fmt"
	"worldsim/internal/characteragent"
	"worldsim/internal/engine"
)

// ---------- 模拟就绪度（素材够不够写小说，不按天数，按剧情弧线完成度） ----------
// 通用规则（任何世界通用，无需配置）：
//   - 完成 ≥3 个剧情段落（arcBook 里 done 的段落——一个段落=一个网文情节单元）
//   - 有足够的戏剧素材（编年史高权重条目 ≥12）
//   - 有伏笔回收（至少 1 条 resolved——证明"有埋有收"）
//   - 张力有过高潮（当前张力 ≥0.4——故事不是一路平）
// 满足即"可写"，天数只是世界时间戳，不是模拟目标。

func (s *Simulator) Readiness() map[string]any {
	if settings, _, enabled, err := s.independentSettings(); enabled || err != nil {
		_ = settings
		if err != nil {
			return map[string]any{"ready": false, "mode": "independent", "reason": err.Error()}
		}
		state, err := s.engine.Snapshot()
		if err != nil {
			return map[string]any{"ready": false, "reason": err.Error()}
		}
		report, err := characteragent.AssessRuntime(s.worldDir, state)
		if err != nil {
			return map[string]any{"ready": false, "reason": err.Error()}
		}
		_, pov, err := engine.ResolveIdentity(state)
		if err != nil {
			return map[string]any{"ready": false, "reason": err.Error()}
		}
		projector, err := characteragent.NewVisibilityProjector(s.worldDir)
		if err != nil {
			return map[string]any{"ready": false, "reason": err.Error()}
		}
		packet, err := projector.Preview(pov)
		if err != nil {
			return map[string]any{"ready": false, "reason": err.Error()}
		}
		known := map[string]bool{}
		for _, o := range packet.Observations {
			known[o.EventID] = true
		}
		changes := 0
		for _, turn := range report.Turns {
			if len(turn.Changes) > 0 && known[turn.SourceID] {
				changes++
			}
		}
		return map[string]any{"ready": changes >= 2, "mode": "independent", "effective_turns": changes, "day": state.Day, "reason": fmt.Sprintf("当前视角已观察到%d次已裁决有效变化；最低要求2次，文学质量由章节审查评估", changes)}
	}
	arcsDone := 0
	for _, a := range s.arcBook {
		if a.Status == "done" {
			arcsDone++
		}
	}
	drama := 0
	for _, c := range s.chronicle {
		if c.Weight >= 0.55 {
			drama++
		}
	}
	totalF, resolvedF := len(s.foreshadows), 0
	for _, f := range s.foreshadows {
		if f.Status == "resolved" {
			resolvedF++
		}
	}
	tension := s.engine.State().WorldLevel.Tension

	ready := arcsDone >= 3 && drama >= 12 && resolvedF >= 1 && tension >= 0.4
	reason := ""
	if !ready {
		if arcsDone < 3 {
			reason += fmt.Sprintf("完成段落 %d/3；", arcsDone)
		}
		if drama < 12 {
			reason += fmt.Sprintf("戏剧素材 %d/12；", drama)
		}
		if resolvedF < 1 {
			reason += "尚无伏笔回收；"
		}
		if tension < 0.4 {
			reason += fmt.Sprintf("张力 %.2f 未达高潮线；", tension)
		}
	}
	return map[string]any{
		"ready":                ready,
		"day":                  s.day,
		"arcs_done":            arcsDone,
		"drama_entries":        drama,
		"foreshadows_resolved": resolvedF,
		"foreshadows_total":    totalF,
		"tension":              tension,
		"reason":               reason,
	}
}
