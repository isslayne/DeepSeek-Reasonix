package agent

import (
	"errors"
	"fmt"

	"reasonix/internal/provider"
)

var ErrContextIrreducible = errors.New("context cannot be reduced safely")

type IrreducibleReason string

const (
	IrreducibleImmutableAnchorTooLarge IrreducibleReason = "immutable_anchor_too_large"
	IrreducibleNoCompletedUnit         IrreducibleReason = "no_completed_unit"
	IrreducibleInflightToolGroup       IrreducibleReason = "inflight_tool_group"
	IrreducibleSummaryRequestTooLarge  IrreducibleReason = "summary_request_too_large"
	IrreducibleSummarizerUnavailable   IrreducibleReason = "summarizer_unavailable"
	IrreducibleNoTokenSavings          IrreducibleReason = "no_token_savings"
	IrreducibleCheckpointInvalid       IrreducibleReason = "checkpoint_invalid"
	IrreducibleNoProjectionProgress    IrreducibleReason = "no_projection_progress"
	IrreducibleUnsafeToolPairing       IrreducibleReason = "unsafe_tool_pairing"
)

type IrreducibleContextError struct {
	Reason IrreducibleReason `json:"reason"`

	EffectiveWindow       int `json:"effective_window,omitempty"`
	EstimatedPrompt       int `json:"estimated_prompt,omitempty"`
	ImmutableAnchorTokens int `json:"immutable_anchor_tokens,omitempty"`
	LargestAtomicUnit     int `json:"largest_atomic_unit,omitempty"`
	SummaryPromptTokens   int `json:"summary_prompt_tokens,omitempty"`
	MinimumOutputTokens   int `json:"minimum_output_tokens,omitempty"`
	ProtocolMargin        int `json:"protocol_margin,omitempty"`

	InflightToolCalls int    `json:"inflight_tool_calls,omitempty"`
	CompletedUnits    int    `json:"completed_units,omitempty"`
	ProjectionGen     uint64 `json:"projection_generation,omitempty"`
	InputHash         string `json:"input_hash,omitempty"`
	Detail            string `json:"detail,omitempty"`
}

func (e *IrreducibleContextError) Error() string {
	if e == nil {
		return ErrContextIrreducible.Error()
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s: %s", ErrContextIrreducible, e.Reason, e.Detail)
	}
	return fmt.Sprintf("%s: %s", ErrContextIrreducible, e.Reason)
}

func (e *IrreducibleContextError) Is(target error) bool {
	return target == ErrContextIrreducible || target == ErrCompactionRequired
}

func irreducible(reason IrreducibleReason, budget RequestBudget, detail string) error {
	return &IrreducibleContextError{
		Reason: reason, EffectiveWindow: budget.EffectiveWindow,
		EstimatedPrompt: budget.EstimatedPrompt, SummaryPromptTokens: budget.EstimatedPrompt,
		MinimumOutputTokens: budget.MinimumOutput, ProtocolMargin: budget.ProtocolMargin,
		Detail: detail,
	}
}

func (a *Agent) irreducibleForMessages(reason IrreducibleReason, msgs []provider.Message, detail string) error {
	req := provider.Request{Messages: a.normalizeModelRequestMessages(msgs), MaxTokens: a.maxOutputTokens}
	if a.svc.tools != nil {
		req.Tools = a.svc.tools.Schemas()
	}
	budget := a.requestBudget(req, req.MaxTokens, 0)
	result := &IrreducibleContextError{
		Reason: reason, EffectiveWindow: budget.EffectiveWindow,
		EstimatedPrompt: budget.EstimatedPrompt, ProtocolMargin: budget.ProtocolMargin,
		InputHash: providerVisibleFingerprint(modelInputMessages(msgs)),
	}
	if a != nil {
		result.ProjectionGen = a.maintenanceProgressSnapshot().Generation
	}
	units := a.contextUnits(msgs)
	for _, unit := range units {
		result.LargestAtomicUnit = max(result.LargestAtomicUnit, unit.EstimatedTokens)
		if unit.Complete && unit.Kind != UnitSystem && unit.Kind != UnitCheckpoint && unit.Kind != UnitSyntheticControl {
			result.CompletedUnits++
		}
		if unit.Kind == UnitToolGroup && !unit.Complete && unit.ToolGroup != nil {
			result.InflightToolCalls += len(unit.ToolGroup.Calls)
		}
	}
	anchorEnd := 0
	if active := a.activeTurnStart(msgs); active >= 0 {
		anchorEnd = active + 1
	} else {
		for _, unit := range units {
			if unit.Kind != UnitSystem && unit.Kind != UnitCheckpoint {
				break
			}
			anchorEnd = unit.VisibleEnd
		}
	}
	if anchorEnd > 0 && anchorEnd <= len(msgs) {
		result.ImmutableAnchorTokens = a.estimatedVisibleRequestTokens(msgs[:anchorEnd])
	}
	result.Detail = detail
	return result
}

func (a *Agent) classifyNoFold(msgs []provider.Message, detail string) error {
	reason := IrreducibleNoCompletedUnit
	for _, unit := range a.contextUnits(msgs) {
		if unit.Kind != UnitToolGroup || unit.Complete {
			continue
		}
		if unit.ToolGroup == nil || unit.ToolGroup.AssistantMessageIndex < 0 || unit.ToolGroup.PairingMode == "unknown" {
			reason = IrreducibleUnsafeToolPairing
			break
		}
		reason = IrreducibleInflightToolGroup
	}
	return a.irreducibleForMessages(reason, msgs, detail)
}

type RequestBudget struct {
	EffectiveWindow  int
	EstimatedPrompt  int
	RequestedOutput  int
	EffectiveOutput  int
	MinimumOutput    int
	ProtocolMargin   int
	HardInputCeiling int
	WindowMode       provider.ContextWindowMode
	Source           string
}

func protocolMarginForWindow(window int) int {
	margin := protocolReserveTokens
	if window > 0 {
		margin = window / 200
	}
	return min(1024, max(protocolReserveTokens, margin))
}

func (a *Agent) requestBudget(req provider.Request, requestedOutput, minimumOutput int) RequestBudget {
	budget := RequestBudget{RequestedOutput: requestedOutput, MinimumOutput: minimumOutput}
	if a == nil {
		return budget
	}
	policy := contextBudgetPolicyOf(a.svc.prov)
	if policy.WindowMode == provider.ContextWindowUnknown && a.lastAdmission().ObservedWindow > 0 {
		policy.WindowMode = provider.ContextWindowShared
	}
	budget.WindowMode = policy.WindowMode
	budget.EffectiveWindow = a.effectiveContextWindow()
	budget.EstimatedPrompt = a.estimatedRequestTokens(req)
	budget.ProtocolMargin = protocolMarginForWindow(budget.EffectiveWindow)
	budget.HardInputCeiling = max(0, budget.EffectiveWindow-budget.ProtocolMargin)
	budget.Source = admissionSource(req.MaxTokens, policy,
		budget.EffectiveWindow > 0 && (a.contextWindow <= 0 || budget.EffectiveWindow < a.contextWindow))

	effective := requestedOutput
	if effective <= 0 {
		effective = req.MaxTokens
	}
	if policy.MaxOutputTokens > 0 && (effective <= 0 || effective > policy.MaxOutputTokens) {
		effective = policy.MaxOutputTokens
	}
	if learned := a.learnedCompletionBudget(); learned > 0 && (effective <= 0 || effective > learned) {
		effective = learned
	}
	if policy.WindowMode == provider.ContextWindowShared || a.lastAdmission().ObservedWindow > 0 {
		available := budget.EffectiveWindow - budget.EstimatedPrompt - budget.ProtocolMargin
		if effective <= 0 || effective > available {
			effective = available
		}
	}
	budget.EffectiveOutput = max(0, effective)
	return budget
}

type MaintenanceAttemptKey struct {
	InputHash       string
	ProjectionGen   uint64
	EffectiveWindow int
	Trigger         string
}

type maintenanceProgressSnapshot struct {
	Generation        uint64
	ProjectionVersion uint64
	CoveredCount      int
	InputHash         string
	OutputHash        string
	SavedTokens       int
	CheckpointEnd     int
}

func (a *Agent) maintenanceProgressSnapshot() maintenanceProgressSnapshot {
	if a == nil {
		return maintenanceProgressSnapshot{}
	}
	a.sess.compactionMu.Lock()
	defer a.sess.compactionMu.Unlock()
	state := a.sess.compactionState
	snapshot := maintenanceProgressSnapshot{
		Generation: state.ProjectionGeneration, ProjectionVersion: state.Projection.ProjectionVersion,
		CoveredCount: state.Projection.CoveredCount, InputHash: state.Projection.ViewInputHash,
		OutputHash: state.Projection.ViewOutputHash,
	}
	if state.LastReceipt != nil {
		snapshot.SavedTokens = state.LastReceipt.SavedTokens
	}
	if state.Projection.ActiveCheckpoint != nil {
		snapshot.CheckpointEnd = state.Projection.ActiveCheckpoint.CanonicalEnd
	}
	return snapshot
}

func maintenanceMadeProgress(before, after maintenanceProgressSnapshot, failingInputHash string) bool {
	if after.Generation <= before.Generation || after.ProjectionVersion <= before.ProjectionVersion || after.SavedTokens <= 0 {
		return false
	}
	if after.OutputHash == "" || after.OutputHash == failingInputHash || after.OutputHash == before.OutputHash {
		return false
	}
	if after.CoveredCount < before.CoveredCount || after.CheckpointEnd < before.CheckpointEnd {
		return false
	}
	return true
}
