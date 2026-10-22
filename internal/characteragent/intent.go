package characteragent

import (
	"errors"
	"math"
	"strings"
)

// CharacterIntent is the only public output accepted by deterministic rules.
// There is no arbitrary state Change, private belief or reasoning field.
type CharacterIntent struct {
	ActorID       string           `json:"actor_id"`
	TurnID        string           `json:"turn_id"`
	Action        string           `json:"action"`
	Target        string           `json:"target,omitempty"`
	Destination   string           `json:"destination,omitempty"`
	Speech        string           `json:"speech,omitempty"`
	Audience      []string         `json:"audience,omitempty"`
	Cards         []string         `json:"cards,omitempty"`
	Claim         string           `json:"claim,omitempty"`
	Quantity      int              `json:"quantity,omitempty"`
	Sources       []string         `json:"sources"`
	Preconditions IntentConditions `json:"preconditions,omitempty"`
}

type IntentConditions struct {
	Location     string   `json:"location,omitempty"`
	MinimumMoney *float64 `json:"minimum_money,omitempty"`
	TargetAlive  bool     `json:"target_alive,omitempty"`
}

type TurnSnapshot struct {
	ID            string   `json:"id"`
	WorldRevision int      `json:"world_revision"`
	Mode          string   `json:"mode"`
	Actors        []string `json:"actors"`
}

func validateIntent(intent CharacterIntent, owner CognitionOwner, turn string, visible map[string]bool) error {
	bad := errors.New("invalid character intent identity, action or sources")
	if intent.ActorID != owner.CharacterID || intent.TurnID != turn || len(intent.Sources) == 0 || len(intent.Sources) > 64 || len(intent.Cards) > 3 || len(intent.Speech) > 4096 || len(intent.Target) > 256 || len(intent.Destination) > 256 || len(intent.Claim) > 128 || intent.Quantity < 0 {
		return bad
	}
	switch intent.Action {
	case "observe", "wait", "speak", "move", "give", "play", "accept", "challenge":
	default:
		return bad
	}
	for _, source := range intent.Sources {
		if !visible[source] {
			return bad
		}
	}
	seenAudience := map[string]bool{}
	for _, actor := range intent.Audience {
		if !cognitionID(actor) || seenAudience[actor] {
			return bad
		}
		seenAudience[actor] = true
	}
	for _, card := range intent.Cards {
		if !cognitionID(card) {
			return bad
		}
	}
	if strings.TrimSpace(intent.Speech) == "" && intent.Action == "speak" {
		return bad
	}
	if v := intent.Preconditions.MinimumMoney; v != nil && (*v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
		return bad
	}
	return nil
}
