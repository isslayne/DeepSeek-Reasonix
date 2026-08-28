package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/provider"
)

const (
	projectionKindHistoryCheckpoint = "history_checkpoint"
	projectionKindActiveCheckpoint  = "active_turn_checkpoint"
	projectionKindSyntheticControl  = "synthetic_control"
	projectionKindToolPlaceholder   = "tool_result_placeholder"
)

type CheckpointFile struct {
	Path   string `json:"path"`
	Status string `json:"status,omitempty"`
	Hash   string `json:"hash,omitempty"`
}

// ActiveTurnCheckpoint is the typed durable state behind the rendered
// provider checkpoint. Identity, coverage, hashes, generation, and tool
// receipts are host-generated; only the narrative fields may originate from
// the summarizer.
type ActiveTurnCheckpoint struct {
	SchemaVersion int `json:"schema_version"`

	ActiveTurnID      string `json:"active_turn_id"`
	OriginalTaskHash  string `json:"original_task_hash"`
	CanonicalStart    int    `json:"canonical_start"`
	CanonicalEnd      int    `json:"canonical_end"`
	CoveredSourceHash string `json:"covered_source_hash"`
	Generation        uint64 `json:"generation"`
	NarrativeMode     string `json:"narrative_mode,omitempty"`

	StandingConstraints []string          `json:"standing_constraints,omitempty"`
	CompletedOperations []ToolCallReceipt `json:"completed_operations,omitempty"`
	Files               []CheckpointFile  `json:"files,omitempty"`
	Decisions           []string          `json:"decisions,omitempty"`
	Errors              []string          `json:"errors,omitempty"`
	Pending             []string          `json:"pending,omitempty"`
	NextAction          string            `json:"next_action,omitempty"`
	Narrative           string            `json:"narrative,omitempty"`
}

type ProjectionItemKind uint8

const (
	ProjectionCanonicalRange ProjectionItemKind = iota
	ProjectionToolPlaceholder
	ProjectionCheckpoint
	ProjectionSyntheticControl
)

type ProjectionItem struct {
	Kind ProjectionItemKind `json:"kind"`

	CanonicalFrom int `json:"canonical_from,omitempty"`
	CanonicalTo   int `json:"canonical_to,omitempty"`

	Message    *provider.Message     `json:"message,omitempty"`
	Checkpoint *ActiveTurnCheckpoint `json:"checkpoint,omitempty"`
	Synthetic  bool                  `json:"synthetic,omitempty"`
}

type ProviderCapabilities struct {
	PositionalToolPairing bool
}

func projectionItemsFromMessages(messages []provider.Message, checkpoint *ActiveTurnCheckpoint) []ProjectionItem {
	items := make([]ProjectionItem, 0, len(messages))
	checkpointAdded := false
	for _, message := range messages {
		copyMessage := message
		item := ProjectionItem{Kind: ProjectionCanonicalRange, CanonicalFrom: -1, CanonicalTo: -1, Message: &copyMessage}
		switch message.ProjectionKind {
		case projectionKindActiveCheckpoint:
			item.Kind = ProjectionCheckpoint
			if checkpoint != nil {
				item.Message = nil
				copyCheckpoint := *checkpoint
				item.Checkpoint = &copyCheckpoint
				checkpointAdded = true
			}
		case projectionKindHistoryCheckpoint:
			item.Kind = ProjectionCheckpoint
		case projectionKindSyntheticControl:
			item.Kind = ProjectionSyntheticControl
			item.Synthetic = true
		case projectionKindToolPlaceholder:
			item.Kind = ProjectionToolPlaceholder
		}
		items = append(items, item)
	}
	if checkpoint != nil && !checkpointAdded {
		copyCheckpoint := *checkpoint
		items = append(items, ProjectionItem{Kind: ProjectionCheckpoint, CanonicalFrom: -1, CanonicalTo: -1, Checkpoint: &copyCheckpoint})
	}
	return items
}

func upgradeLegacyProjectionMetadata(state *CompactionState) {
	if state == nil || len(state.Projection.Messages) == 0 {
		return
	}
	changed := false
	for i := range state.Projection.Messages {
		message := &state.Projection.Messages[i]
		if message.ProjectionKind != "" {
			continue
		}
		trimmed := strings.TrimSpace(message.Content)
		switch {
		case message.Role == provider.RoleAssistant &&
			strings.HasPrefix(trimmed, activeTurnCheckpointTagOpen) &&
			strings.Contains(trimmed, activeTurnCheckpointTagClose):
			message.ProjectionKind = projectionKindActiveCheckpoint
			changed = true
		case message.Role == provider.RoleUser &&
			strings.HasPrefix(trimmed, summaryTagOpen) &&
			strings.Contains(trimmed, summaryTagClose):
			if message.Content == activeTurnContinuation {
				message.ProjectionKind = projectionKindSyntheticControl
				message.Synthetic = true
			} else {
				message.ProjectionKind = projectionKindHistoryCheckpoint
			}
			changed = true
		case message.Role == provider.RoleTool && strings.Contains(message.Content, toolClearMarker):
			message.ProjectionKind = projectionKindToolPlaceholder
			changed = true
		}
	}
	if changed && len(state.Projection.Items) == 0 {
		state.Projection.Items = projectionItemsFromMessages(state.Projection.Messages, state.Projection.ActiveCheckpoint)
	}
}

// renderProjection is the final provider-agnostic rendering layer. Typed
// checkpoint/control state is converted to legacy-compatible message roles
// only here; ModelMessages subsequently strips ProjectionKind/Synthetic.
func renderProjection(items []ProjectionItem, _ ProviderCapabilities) []provider.Message {
	messages := make([]provider.Message, 0, len(items))
	for _, item := range items {
		if item.Kind == ProjectionCheckpoint && item.Checkpoint != nil {
			messages = append(messages, formatTypedActiveTurnCheckpoint(*item.Checkpoint))
			continue
		}
		if item.Message == nil {
			continue
		}
		message := *item.Message
		if item.Kind == ProjectionSyntheticControl || item.Synthetic {
			message.ProjectionKind = projectionKindSyntheticControl
			message.Synthetic = true
		}
		messages = append(messages, message)
	}
	return provider.ProjectionMessages(messages)
}

func formatTypedActiveTurnCheckpoint(checkpoint ActiveTurnCheckpoint) provider.Message {
	return provider.Message{
		Role:           provider.RoleAssistant,
		Content:        renderActiveTurnCheckpoint(checkpoint),
		ProjectionKind: projectionKindActiveCheckpoint,
	}
}

func renderActiveTurnCheckpoint(checkpoint ActiveTurnCheckpoint) string {
	var body strings.Builder
	fmt.Fprintf(&body, "%s\ncheckpoint_schema=%d active_turn_id=%s generation=%d canonical=[%d,%d) task_hash=%s covered_hash=%s\n",
		activeTurnCheckpointTagOpen, checkpoint.SchemaVersion, checkpoint.ActiveTurnID,
		checkpoint.Generation, checkpoint.CanonicalStart, checkpoint.CanonicalEnd,
		checkpoint.OriginalTaskHash, checkpoint.CoveredSourceHash)
	if len(checkpoint.CompletedOperations) > 0 {
		body.WriteString("\nCompleted operations (host-verified receipts):\n")
		for _, receipt := range checkpoint.CompletedOperations {
			fmt.Fprintf(&body, "- call_id=%s tool=%s status=%s args=%s result=%s archive=%s side_effect=%s\n",
				receipt.CallID, receipt.ToolName, receipt.Status, receipt.ArgumentsHash,
				receipt.ResultHash, receipt.ArchiveRef, receipt.SideEffect)
		}
	}
	writeCheckpointList := func(title string, values []string) {
		if len(values) == 0 {
			return
		}
		body.WriteString("\n" + title + ":\n")
		for _, value := range values {
			fmt.Fprintf(&body, "- %s\n", value)
		}
	}
	writeCheckpointList("Standing constraints", checkpoint.StandingConstraints)
	writeCheckpointList("Decisions", checkpoint.Decisions)
	writeCheckpointList("Errors", checkpoint.Errors)
	writeCheckpointList("Pending", checkpoint.Pending)
	if checkpoint.NextAction != "" {
		body.WriteString("\nNext action:\n" + checkpoint.NextAction + "\n")
	}
	if checkpoint.Narrative != "" {
		body.WriteString("\nNarrative:\n" + checkpoint.Narrative + "\n")
	}
	body.WriteString(activeTurnCheckpointTagClose)
	return body.String()
}

type checkpointModelPayload struct {
	StandingConstraints []string `json:"standing_constraints"`
	Decisions           []string `json:"decisions"`
	Errors              []string `json:"errors"`
	Pending             []string `json:"pending"`
	NextAction          string   `json:"next_action"`
	Narrative           string   `json:"narrative"`
}

func parseCheckpointModelPayload(summary string) (checkpointModelPayload, bool) {
	trimmed := strings.TrimSpace(summary)
	if after, ok := strings.CutPrefix(trimmed, "```json"); ok {
		trimmed = strings.TrimSpace(after)
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "```"))
	}
	var payload checkpointModelPayload
	if json.Unmarshal([]byte(trimmed), &payload) == nil {
		return payload, true
	}
	return checkpointModelPayload{Narrative: trimmed}, false
}

func (a *Agent) buildActiveTurnCheckpoint(state CompactionState, canonical []provider.Message, covered int, summary string) *ActiveTurnCheckpoint {
	start := a.activeTurnStart(canonical)
	if start < 0 || start >= len(canonical) || covered <= start {
		return nil
	}
	if covered > len(canonical) {
		covered = len(canonical)
	}
	task := canonical[start]
	payload, structured := parseCheckpointModelPayload(summary)
	narrativeMode := MaintenanceMechanicalFallback
	if structured {
		narrativeMode = MaintenanceActiveCheckpoint
	}
	if !structured && state.Projection.ActiveCheckpoint != nil {
		previous := state.Projection.ActiveCheckpoint
		payload.StandingConstraints = append([]string(nil), previous.StandingConstraints...)
		payload.Decisions = append([]string(nil), previous.Decisions...)
		payload.Errors = append([]string(nil), previous.Errors...)
		payload.Pending = append([]string(nil), previous.Pending...)
		payload.NextAction = previous.NextAction
		if strings.TrimSpace(payload.Narrative) == "" {
			payload.Narrative = previous.Narrative
		}
	}
	generation := uint64(1)
	if state.Projection.ActiveCheckpoint != nil {
		generation = state.Projection.ActiveCheckpoint.Generation + 1
	}
	turnHash := hashContextPayload(task.Content)
	turnID := fmt.Sprintf("turn:%d:%s", task.CreatedAt, strings.TrimPrefix(turnHash, "sha256:")[:12])
	checkpoint := &ActiveTurnCheckpoint{
		SchemaVersion: 1, ActiveTurnID: turnID, OriginalTaskHash: turnHash,
		CanonicalStart: start, CanonicalEnd: covered,
		CoveredSourceHash: providerVisibleFingerprint(modelInputMessages(canonical[start:covered])),
		Generation:        generation, NarrativeMode: narrativeMode, StandingConstraints: payload.StandingConstraints,
		Decisions: payload.Decisions, Errors: payload.Errors, Pending: payload.Pending,
		NextAction: payload.NextAction, Narrative: payload.Narrative,
	}
	checkpoint.CompletedOperations = a.toolReceiptsForCanonicalRange(canonical, start+1, covered)
	return checkpoint
}

func (a *Agent) toolReceiptsForCanonicalRange(canonical []provider.Message, start, end int) []ToolCallReceipt {
	if start < 0 {
		start = 0
	}
	if end > len(canonical) {
		end = len(canonical)
	}
	if start >= end {
		return nil
	}
	units := a.contextUnits(canonical[start:end])
	receipts := make([]ToolCallReceipt, 0)
	for _, unit := range units {
		if unit.Kind != UnitToolGroup || !unit.Complete || unit.ToolGroup == nil {
			continue
		}
		results := make([]provider.Message, 0, len(unit.ToolGroup.Calls))
		for _, message := range unit.Messages {
			if !message.LocalOnly && message.Role == provider.RoleTool {
				results = append(results, message)
			}
		}
		byID := make(map[string]provider.Message, len(results))
		for _, result := range results {
			byID[result.ToolCallID] = result
		}
		for i, base := range unit.ToolGroup.Calls {
			result := byID[base.CallID]
			if unit.ToolGroup.PairingMode == "positional" && i < len(results) {
				result = results[i]
			}
			body := providerVisibleToolContent(result)
			if result.RawContent != "" {
				body = result.RawContent
			}
			base.Status = toolResultStatus(result)
			base.ResultHash = hashContextPayload(body)
			base.ArchiveRef = "session://tool-results/" + toolResultRef(result.ToolCallID, body)
			receipts = upsertToolCallReceipt(receipts, base)
		}
	}
	return receipts
}
