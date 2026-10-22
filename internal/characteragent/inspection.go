package characteragent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type AgentInspection struct {
	CharacterID         string `json:"character_id"`
	AgentID             string `json:"agent_id"`
	State               string `json:"state"`
	Thinking            bool   `json:"thinking"`
	CognitionRevision   int    `json:"cognition_revision"`
	Observations        int    `json:"observations"`
	LatestObservations  int    `json:"latest_observations"`
	Thoughts            int    `json:"thoughts"`
	LastTurn            string `json:"last_turn,omitempty"`
	LastAction          string `json:"last_action,omitempty"`
	LastTransportStatus string `json:"last_transport_status,omitempty"`
	PromptTokens        int    `json:"prompt_tokens"`
	CompletionTokens    int    `json:"completion_tokens"`
	UsageEstimated      bool   `json:"usage_estimated"`
}

// ReadCallAudit is explicit operator inspection, not a model context builder.
func ReadCallAudit(dir, id string) ([]CallAudit, error) {
	s, err := NewCognitionStore(dir, id)
	if err != nil {
		return nil, err
	}
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	if err := s.checkBindingLocked(); err != nil {
		return nil, err
	}
	raw, err := readPrivateFile(s.registry.dir, filepath.Join("agents", s.owner.AgentID, "audit.jsonl"))
	if os.IsNotExist(err) {
		return []CallAudit{}, nil
	}
	if err != nil {
		return nil, errors.New("cannot read character audit")
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		return nil, errors.New("incomplete character audit")
	}
	result := []CallAudit{}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var item CallAudit
		if checkUniqueJSON(line) != nil || json.Unmarshal(line, &item) != nil || item.Owner != s.owner || item.PromptTokens < 0 || item.CompletionTokens < 0 {
			return nil, errors.New("invalid character audit")
		}
		result = append(result, item)
	}
	return result, nil
}

func InspectAgent(dir, id string) (AgentInspection, error) {
	s, err := NewCognitionStore(dir, id)
	if err != nil {
		return AgentInspection{}, err
	}
	d, _, err := s.registry.Read()
	if err != nil {
		return AgentInspection{}, err
	}
	entry := d.Agents[id]
	out := AgentInspection{CharacterID: id, AgentID: s.owner.AgentID, State: entry.State}
	if value, ok := thoughtLocks.Load(thoughtLockKey{s.registry.mu, s.owner.AgentID}); ok {
		gate := value.(interface {
			TryLock() bool
			Unlock()
		})
		if gate.TryLock() {
			gate.Unlock()
		} else {
			out.Thinking = true
		}
	}
	cognition, err := s.Read()
	if err != nil {
		return out, err
	}
	out.CognitionRevision = cognition.Revision
	out.Observations = len(cognition.Observations)
	for _, o := range cognition.Observations {
		if o.WorldRevision == d.WorldRevision && o.EventID != "" {
			out.LatestObservations++
		}
	}
	out.Thoughts = len(cognition.Thoughts)
	if len(cognition.Thoughts) > 0 {
		last := cognition.Thoughts[len(cognition.Thoughts)-1]
		out.LastTurn = last.TurnID
		if last.Intent != nil {
			out.LastAction = last.Intent.Action
		}
	}
	audit, err := ReadCallAudit(dir, id)
	if err != nil {
		return out, err
	}
	for _, entry := range audit {
		out.LastTransportStatus = entry.Status
		if entry.Status == "received" || entry.Status == "incomplete" {
			out.PromptTokens += entry.PromptTokens
			out.CompletionTokens += entry.CompletionTokens
			out.UsageEstimated = out.UsageEstimated || entry.UsageEstimated
		}
	}
	return out, nil
}
