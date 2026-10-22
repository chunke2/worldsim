package characteragent

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"worldsim/internal/engine"
	"worldsim/internal/fsutil"
	"worldsim/internal/narrative"
)

type TurnProgress struct {
	Turn         int      `json:"turn"`
	Revision     int      `json:"revision"`
	SourceID     string   `json:"source_id"`
	Changes      []string `json:"effective_changes"`
	StalledTurns int      `json:"stalled_turns"`
	Reason       string   `json:"reason"`
}
type RuntimeNarrativeReport struct {
	SchemaVersion     int                `json:"schema_version"`
	ThroughRevision   int                `json:"through_revision"`
	Turns             []TurnProgress     `json:"turns"`
	PendingConditions []narrative.Thread `json:"pending_conditions"`
	Pressures         []WorldPressure    `json:"pressures"`
	Limit             string             `json:"limit"`
}

// AssessRuntime consumes committed facts. It never promotes model hypotheses,
// author intentions or private plans to evidence of world progress.
func AssessRuntime(dir string, state *engine.WorldState) (RuntimeNarrativeReport, error) {
	report := RuntimeNarrativeReport{SchemaVersion: 1, ThroughRevision: state.Revision, Turns: []TurnProgress{}, Limit: "规则证明的行动、信息揭示与代价；对话的新颖性、人物弧光和文学效果仍需审阅。目标、关系、承诺未显式落入世界状态时不宣称已发生。"}
	entries, err := narrative.ReadJournal(dir)
	if err != nil {
		return report, err
	}
	for _, entry := range entries {
		if entry.Revision > state.Revision {
			continue
		}
		for _, change := range entry.Changes {
			if change.Path != "world_level.runtime" {
				continue
			}
			data, err := json.Marshal(change.Value)
			if err != nil {
				return report, err
			}
			var scene SceneState
			if !decodeLegacyArtifact(data, &scene) {
				return report, fmt.Errorf("invalid narrative runtime source at revision %d", entry.Revision)
			}
			if scene.LastTurnRevision != entry.Revision || scene.LastTurnID != entry.CommandID {
				continue
			}
			progress := TurnProgress{Turn: scene.Turn, Revision: entry.Revision, SourceID: fmt.Sprintf("event:%d:%s", entry.Revision, entry.CommandID), Changes: []string{}, StalledTurns: scene.StalledTurns}
			for _, outcome := range scene.LastOutcomes {
				if !outcome.Accepted {
					continue
				}
				kind := ""
				switch outcome.Action {
				case "play":
					kind = "已承诺隐藏出牌；引入可被质疑的新决策"
				case "challenge":
					kind = "公开翻牌验证真伪，并产生不可逆生命代价"
				case "accept":
					kind = "接受未验证风险；公开累计惩罚条件推进"
				case "move":
					kind = "空间位置变化"
				case "give":
					kind = "资源转移"
				}
				if kind != "" {
					progress.Changes = append(progress.Changes, outcome.ActorID+"："+kind)
				}
			}
			if len(progress.Changes) == 0 {
				progress.Reason = "只有等待、观察、表达或失败行动；未证明新增可信事实、关系或目标变化"
			} else {
				progress.Reason = "有确定性规则裁决支持的有效变化"
			}
			report.Turns = append(report.Turns, progress)
		}
	}
	plan, err := narrative.Load(dir)
	if err != nil {
		return report, err
	}
	for _, thread := range plan.Threads {
		if thread.Status != "resolved" && thread.Status != "recovered" {
			report.PendingConditions = append(report.PendingConditions, thread)
		}
	}
	if len(state.WorldLevel.Runtime) > 0 {
		scene, err := readScene(state)
		if err != nil {
			return report, err
		}
		report.Pressures = scene.Pressures
	}
	return report, nil
}

func SaveRuntimeAssessment(dir string, state *engine.WorldState) error {
	report, err := AssessRuntime(dir, state)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, "narrative_runtime.json"), b)
}
