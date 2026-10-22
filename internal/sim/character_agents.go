package sim

import "worldsim/internal/characteragent"

// ReconcileCharacterAgents updates derived metadata and creates empty private
// partitions. It does not read old memories, call models or execute intentions.
func (s *Simulator) ReconcileCharacterAgents() error {
	registry, err := characteragent.NewRegistry(s.worldDir)
	if err != nil {
		return err
	}
	state, err := s.engine.Snapshot()
	if err != nil {
		return err
	}
	_, err = registry.Reconcile(state)
	return err
}
