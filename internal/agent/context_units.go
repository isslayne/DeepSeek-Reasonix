package agent

import (
	"crypto/sha256"
	"encoding/hex"

	"reasonix/internal/provider"
)

// ContextUnitKind is the smallest atomic unit the maintenance planner may
// retain, archive, or summarize. Message indexes remain available for legacy
// projection storage, but every boundary is selected from these units.
type ContextUnitKind uint8

const (
	UnitSystem ContextUnitKind = iota
	UnitUserTurn
	UnitAssistantText
	UnitToolGroup
	UnitCheckpoint
	UnitSyntheticControl
)

type ToolSideEffectClass string

const (
	ToolReadOnly        ToolSideEffectClass = "read_only"
	ToolIdempotentWrite ToolSideEffectClass = "idempotent_write"
	ToolNonIdempotent   ToolSideEffectClass = "non_idempotent"
	ToolUnknownEffect   ToolSideEffectClass = "unknown"
)

type ToolCallReceipt struct {
	CallID        string              `json:"call_id,omitempty"`
	ToolName      string              `json:"tool_name,omitempty"`
	ArgumentsHash string              `json:"arguments_hash,omitempty"`
	ArgumentsHint string              `json:"arguments_hint,omitempty"`
	Status        string              `json:"status,omitempty"`
	ResultHash    string              `json:"result_hash,omitempty"`
	ArchiveRef    string              `json:"archive_ref,omitempty"`
	SideEffect    ToolSideEffectClass `json:"side_effect,omitempty"`
	ResourceIDs   []string            `json:"resource_ids,omitempty"`
}

type ToolGroupReceipt struct {
	AssistantMessageIndex int               `json:"assistant_message_index"`
	Calls                 []ToolCallReceipt `json:"calls"`
	Complete              bool              `json:"complete"`
	PairingMode           string            `json:"pairing_mode"` // id|positional|unknown
}

type ContextUnit struct {
	Kind ContextUnitKind

	VisibleStart  int
	VisibleEnd    int
	CanonicalFrom int
	CanonicalTo   int

	Messages []provider.Message

	Complete        bool
	UserAuthored    bool
	ProviderVisible bool
	EstimatedTokens int

	ToolGroup *ToolGroupReceipt
}

type contextUnitBuildOptions struct {
	allowPositionalToolPairing bool
}

func (a *Agent) contextUnits(msgs []provider.Message) []ContextUnit {
	allowPositional := false
	if a != nil {
		allowPositional = provider.SupportsPositionalToolPairing(a.svc.prov)
	}
	units := buildContextUnits(msgs, contextUnitBuildOptions{allowPositionalToolPairing: allowPositional})
	if a == nil || a.svc.tools == nil {
		return units
	}
	for i := range units {
		if units[i].ToolGroup == nil {
			continue
		}
		for j := range units[i].ToolGroup.Calls {
			call := &units[i].ToolGroup.Calls[j]
			if registered, ok := a.svc.tools.Get(call.ToolName); ok {
				if registered.ReadOnly() {
					call.SideEffect = ToolReadOnly
				} else {
					call.SideEffect = ToolUnknownEffect
				}
			}
		}
	}
	return units
}

func buildContextUnits(msgs []provider.Message, options contextUnitBuildOptions) []ContextUnit {
	units := make([]ContextUnit, 0, len(msgs))
	for i := 0; i < len(msgs); {
		start := i
		message := msgs[i]
		end := i + 1
		kind := UnitAssistantText
		complete := true
		userAuthored := false
		var group *ToolGroupReceipt

		switch {
		case message.LocalOnly:
			kind = UnitSyntheticControl
		case message.Synthetic || message.ProjectionKind == projectionKindSyntheticControl:
			kind = UnitSyntheticControl
		case message.Role == provider.RoleSystem:
			kind = UnitSystem
		case isCheckpointMessage(message):
			kind = UnitCheckpoint
		case message.Role == provider.RoleUser:
			if IsUserAuthoredTurn(message.Content) {
				kind = UnitUserTurn
				userAuthored = true
			} else {
				kind = UnitSyntheticControl
			}
		case message.Role == provider.RoleAssistant && len(message.ToolCalls) > 0:
			kind = UnitToolGroup
			results := make([]provider.Message, 0, len(message.ToolCalls))
			for end < len(msgs) {
				candidate := msgs[end]
				if candidate.LocalOnly {
					end++
					continue
				}
				if candidate.Role != provider.RoleTool {
					break
				}
				results = append(results, candidate)
				end++
			}
			group = buildToolGroupReceipt(start, message.ToolCalls, results, options.allowPositionalToolPairing)
			complete = group.Complete
		case message.Role == provider.RoleTool:
			// An orphan result has no safe semantic interpretation. Keep it as an
			// incomplete atomic unit so every automatic fold fails closed.
			kind = UnitToolGroup
			complete = false
			group = &ToolGroupReceipt{AssistantMessageIndex: -1, PairingMode: "unknown"}
		}

		unitMessages := append([]provider.Message(nil), msgs[start:end]...)
		providerVisible := false
		for _, candidate := range unitMessages {
			if !candidate.LocalOnly {
				providerVisible = true
				break
			}
		}
		units = append(units, ContextUnit{
			Kind: kind, VisibleStart: start, VisibleEnd: end,
			CanonicalFrom: start, CanonicalTo: end,
			Messages: unitMessages, Complete: complete, UserAuthored: userAuthored,
			ProviderVisible: providerVisible, EstimatedTokens: estimateMessagesTokens(unitMessages),
			ToolGroup: group,
		})
		i = end
	}
	return units
}

func isCheckpointMessage(message provider.Message) bool {
	if message.ProjectionKind == projectionKindHistoryCheckpoint || message.ProjectionKind == projectionKindActiveCheckpoint {
		return true
	}
	if isActiveTurnCheckpointMessage(message) {
		return true
	}
	return isCompactionSummary(message) && message.Content != activeTurnContinuation
}

func buildToolGroupReceipt(assistantIndex int, calls []provider.ToolCall, results []provider.Message, allowPositional bool) *ToolGroupReceipt {
	receipt := &ToolGroupReceipt{
		AssistantMessageIndex: assistantIndex,
		Calls:                 make([]ToolCallReceipt, len(calls)),
		PairingMode:           "unknown",
	}
	for i, call := range calls {
		receipt.Calls[i] = ToolCallReceipt{
			CallID: call.ID, ToolName: call.Name,
			ArgumentsHash: hashContextPayload(call.Arguments),
			ArgumentsHint: summarizeToolArgs(call.Arguments),
			SideEffect:    ToolUnknownEffect,
		}
	}

	if distinctToolCallIDs(calls) {
		receipt.PairingMode = "id"
		if len(results) != len(calls) {
			return receipt
		}
		byID := make(map[string]provider.Message, len(results))
		for _, result := range results {
			if result.ToolCallID == "" {
				return receipt
			}
			if _, duplicate := byID[result.ToolCallID]; duplicate {
				return receipt
			}
			byID[result.ToolCallID] = result
		}
		for i, call := range calls {
			result, ok := byID[call.ID]
			if !ok {
				return receipt
			}
			receipt.Calls[i].Status = "completed"
			receipt.Calls[i].ResultHash = hashContextPayload(providerVisibleToolContent(result))
		}
		receipt.Complete = true
		return receipt
	}

	if !allowPositional || len(results) != len(calls) {
		return receipt
	}
	receipt.PairingMode = "positional"
	for i, result := range results {
		receipt.Calls[i].Status = "completed"
		receipt.Calls[i].ResultHash = hashContextPayload(providerVisibleToolContent(result))
	}
	receipt.Complete = true
	return receipt
}

func distinctToolCallIDs(calls []provider.ToolCall) bool {
	seen := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if call.ID == "" {
			return false
		}
		if _, duplicate := seen[call.ID]; duplicate {
			return false
		}
		seen[call.ID] = struct{}{}
	}
	return true
}

func providerVisibleToolContent(message provider.Message) string {
	if message.ProviderContent != "" {
		return message.ProviderContent
	}
	return message.Content
}

func hashContextPayload(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func contextUnitBoundary(units []ContextUnit, boundary int) bool {
	if boundary == 0 {
		return true
	}
	for _, unit := range units {
		if unit.VisibleEnd == boundary {
			return true
		}
	}
	return false
}

func contextUnitsInRange(units []ContextUnit, start, end int) ([]ContextUnit, bool) {
	if start < 0 || end < start || !contextUnitBoundary(units, start) || !contextUnitBoundary(units, end) {
		return nil, false
	}
	selected := make([]ContextUnit, 0)
	for _, unit := range units {
		if unit.VisibleEnd <= start {
			continue
		}
		if unit.VisibleStart >= end {
			break
		}
		if unit.VisibleStart < start || unit.VisibleEnd > end {
			return nil, false
		}
		selected = append(selected, unit)
	}
	return selected, true
}

func contextUnitsFoldable(units []ContextUnit) bool {
	for _, unit := range units {
		if unit.Kind == UnitToolGroup && !unit.Complete {
			return false
		}
	}
	return true
}
