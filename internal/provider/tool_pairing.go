package provider

import (
	"encoding/json"

	"reasonix/internal/nilutil"
)

// ToolSchema is a tool definition exposed to the model. Parameters is JSON Schema.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// PositionalToolPairingPolicy explicitly opts a provider into positional pairing.
type PositionalToolPairingPolicy interface {
	SupportsPositionalToolPairing() bool
}

func SupportsPositionalToolPairing(p Provider) bool {
	if nilutil.IsNil(p) {
		return false
	}
	policy, ok := p.(PositionalToolPairingPolicy)
	return ok && policy.SupportsPositionalToolPairing()
}
