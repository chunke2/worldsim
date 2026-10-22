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

// SyncCharacterObservations is the explicit bridge for the independent runtime.
// It never invokes models, consumes EventCard.Frame or changes world facts.
func (s *Simulator) SyncCharacterObservations() (characteragent.DeliveryReport, error) {
	if err := s.ReconcileCharacterAgents(); err != nil {
		return characteragent.DeliveryReport{}, err
	}
	p, err := characteragent.NewVisibilityProjector(s.worldDir)
	if err != nil {
		return characteragent.DeliveryReport{}, err
	}
	return p.DeliverAll()
}
