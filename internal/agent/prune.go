package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

// Legacy snip helpers still support compatibility storage. Their public APIs
// are no-ops; pressure-time Harness pruning uses the rune-based policy below.
const (
	snippedMarker = "[snipped tool result — "
	prunedMarker  = "[elided tool result — "
	minPruneBytes = 1024

	toolPruneThresholdRunes = 8192
	toolPruneHeadRunes      = 4096
	toolPruneTailRunes      = 1024
	toolPruneMarker         = "[... tool result middle pruned ...]"
	toolClearMarker         = toolPruneMarker + "\n[cleared tool result — exact content retained in the current session archive]"

	pressureKeepRecentToolGroups = 3
	overflowKeepRecentToolGroups = 1
	minimumToolClearTokens       = 4096
)

func pruneToolResultContent(content string) (string, bool) {
	if utf8.RuneCountInString(content) <= toolPruneThresholdRunes {
		return content, false
	}
	headEnd := byteOffsetAfterRunes(content, toolPruneHeadRunes)
	tailStart := byteOffsetBeforeLastRunes(content, toolPruneTailRunes)
	var pruned strings.Builder
	pruned.Grow(headEnd + len(toolPruneMarker) + len(content) - tailStart)
	pruned.WriteString(content[:headEnd])
	pruned.WriteString(toolPruneMarker)
	pruned.WriteString(content[tailStart:])
	return pruned.String(), true
}

func byteOffsetAfterRunes(content string, count int) int {
	if count <= 0 {
		return 0
	}
	seen := 0
	for offset := range content {
		if seen == count {
			return offset
		}
		seen++
	}
	return len(content)
}

func byteOffsetBeforeLastRunes(content string, count int) int {
	offset := len(content)
	for range count {
		if offset == 0 {
			return 0
		}
		_, size := utf8.DecodeLastRuneInString(content[:offset])
		offset -= size
	}
	return offset
}

// pruneToolResultsToProjectionLocked installs a durable, model-visible prune
// projection. The caller owns compactionRunMu for the whole maintenance run;
// canonical storage, including RawContent, is never modified.
func (a *Agent) pruneToolResultsToProjectionLocked(trigger string) (bool, error) {
	canonical, transcriptVersion := a.sess.conversation.snapshotMessagesVersion()
	a.sess.compactionMu.Lock()
	stateSnapshot := a.sess.compactionState
	a.sess.compactionMu.Unlock()
	visible, _ := a.visibleInputForFold(stateSnapshot, canonical, transcriptVersion)
	plan := a.planToolResultClear(visible, stateSnapshot.Projection.ToolReceipts, trigger)
	if plan.affected == 0 {
		return false, nil
	}
	projected := plan.projected
	sourceTokens := a.estimatedVisibleRequestTokens(visible)
	resultTokens := a.estimatedVisibleRequestTokens(projected)
	savedTokens := max(0, sourceTokens-resultTokens)
	minimumReclaim := minimumToolClearTokens
	if window := a.effectiveContextWindow(); window > 0 {
		minimumReclaim = max(minimumReclaim, window/20)
	}
	if trigger != CompactionTriggerOverflow && savedTokens < minimumReclaim {
		return false, nil
	}
	inputHash := a.contextMaintenanceInputHash(modelInputMessages(visible))
	outputHash := providerVisibleFingerprint(modelInputMessages(projected))
	projectionVersion := stateSnapshot.Projection.ProjectionVersion + 1
	now := time.Now().UTC()
	coveredHash := coveredPrefixHash(canonical, len(canonical))
	receipt := &ContextMaintenanceReceipt{
		OperationID: fmt.Sprintf("prune-%d-%s", projectionVersion, outputHash), Status: "applied", Action: "prune",
		Trigger: trigger, SourceProjection: stateSnapshot.Projection.ProjectionVersion, ProjectionVersion: projectionVersion,
		CoveredCount: len(canonical), CoveredPrefixHash: coveredHash, InputHash: inputHash, OutputHash: outputHash,
		InputTokens: sourceTokens, ResultTokens: resultTokens, SavedTokens: savedTokens,
		AffectedToolResults: plan.affected, CacheBreak: true, Mode: MaintenanceLosslessToolClear,
		ProjectionGeneration: stateSnapshot.ProjectionGeneration + 1, CacheGeneration: stateSnapshot.CacheGeneration + 1,
		CoveredCanonicalFrom: stateSnapshot.Projection.CoveredCount, CoveredCanonicalTo: len(canonical),
		FoldUnits: plan.foldUnits, ArchiveBytes: plan.archiveBytes, ArchiveRefsCount: plan.affected,
		KeptRecentToolGroups: plan.keptGroups,
		ProviderWindowSource: a.requestBudget(provider.Request{Messages: projected}, 0, 0).Source,
		CreatedAt:            now,
	}
	next := stateSnapshot
	next.SchemaVersion = compactionStateSchemaCurrent
	next.TranscriptVersion = transcriptVersion
	next.Generation++
	next.ProjectionGeneration++
	next.CacheGeneration++
	next.MaintenanceRearmAtTokens = resultTokens + a.maintenanceRearmDelta()
	next.PromptCacheKey = a.currentPromptCacheKey()
	next.Projection = ContextProjection{
		Messages: projected, TranscriptVersion: transcriptVersion, ProjectionVersion: projectionVersion,
		Items:            projectionItemsFromMessages(projected, stateSnapshot.Projection.ActiveCheckpoint),
		ActiveCheckpoint: stateSnapshot.Projection.ActiveCheckpoint,
		CoveredCount:     len(canonical), CoveredPrefixHash: coveredHash, SourceTokens: sourceTokens,
		ProjectionTokens: resultTokens, ViewInputHash: inputHash, ViewOutputHash: outputHash,
		ToolReceipts: plan.receipts, CreatedAt: now,
	}
	next.LastReceipt = receipt
	next.UpdatedAt = now

	a.sess.compactionMu.Lock()
	current, currentVersion := a.sess.conversation.snapshotMessagesVersion()
	if currentVersion != transcriptVersion || len(current) != len(canonical) ||
		coveredPrefixHash(current, len(current)) != coveredHash ||
		a.sess.compactionState.Projection.ProjectionVersion != stateSnapshot.Projection.ProjectionVersion ||
		a.sess.compactionState.Generation != stateSnapshot.Generation ||
		a.sess.compactionState.ProjectionGeneration != stateSnapshot.ProjectionGeneration ||
		a.sess.compactionState.CacheGeneration != stateSnapshot.CacheGeneration {
		a.sess.compactionMu.Unlock()
		return false, errCompressStaleContext
	}
	previous := a.sess.compactionState
	a.sess.compactionState = next
	if err := a.persistCompactionStateLocked(); err != nil {
		a.sess.compactionState = previous
		a.sess.compactionMu.Unlock()
		if errors.Is(err, errCompressStaleContext) {
			return false, err
		}
		return false, fmt.Errorf("persist prune projection: %w", err)
	}
	a.sess.checkpointState = "applied"
	a.sess.compactionMu.Unlock()
	a.emitContextMaintenance(receipt)
	return true, nil
}

type toolResultClearPlan struct {
	projected    []provider.Message
	receipts     []ToolCallReceipt
	affected     int
	archiveBytes int
	foldUnits    int
	keptGroups   int
}

func (a *Agent) planToolResultClear(visible []provider.Message, existing []ToolCallReceipt, trigger string) toolResultClearPlan {
	projected := append([]provider.Message(nil), visible...)
	units := a.contextUnits(visible)
	completeGroups := make([]int, 0)
	for i, unit := range units {
		if unit.Kind == UnitToolGroup && unit.Complete && unit.ToolGroup != nil {
			completeGroups = append(completeGroups, i)
		}
	}
	keepRecent := pressureKeepRecentToolGroups
	if trigger == CompactionTriggerOverflow {
		keepRecent = overflowKeepRecentToolGroups
	}
	clearBefore := max(0, len(completeGroups)-keepRecent)
	clearUnits := make(map[int]struct{}, clearBefore)
	for _, unitIndex := range completeGroups[:clearBefore] {
		clearUnits[unitIndex] = struct{}{}
	}

	affected := 0
	archiveBytes := 0
	toolReceipts := append([]ToolCallReceipt(nil), existing...)
	for unitIndex, unit := range units {
		if _, eligible := clearUnits[unitIndex]; !eligible || unit.ToolGroup == nil {
			continue
		}
		calls := make(map[string]ToolCallReceipt, len(unit.ToolGroup.Calls))
		for _, call := range unit.ToolGroup.Calls {
			calls[call.CallID] = call
		}
		resultOrdinal := 0
		for messageIndex := unit.VisibleStart + 1; messageIndex < unit.VisibleEnd; messageIndex++ {
			message := visible[messageIndex]
			if message.LocalOnly || message.Role != provider.RoleTool {
				continue
			}
			ordinal := resultOrdinal
			resultOrdinal++
			source := providerVisibleToolContent(message)
			if utf8.RuneCountInString(source) <= toolPruneThresholdRunes {
				continue
			}
			archiveBody := source
			if message.RawContent != "" {
				archiveBody = message.RawContent
			}
			ref := toolResultRef(message.ToolCallID, archiveBody)
			placeholder := clearedToolResultPlaceholder(message, source, archiveBody, ref)
			if estimateTextTokens(placeholder) >= estimateTextTokens(source) {
				continue
			}
			projected[messageIndex].Content = placeholder
			projected[messageIndex].RawContent = ""
			projected[messageIndex].ProviderContent = ""
			receipt := calls[message.ToolCallID]
			if unit.ToolGroup.PairingMode == "positional" && ordinal < len(unit.ToolGroup.Calls) {
				receipt = unit.ToolGroup.Calls[ordinal]
			}
			receipt.ToolName = firstNonEmpty(receipt.ToolName, message.Name)
			receipt.Status = toolResultStatus(message)
			receipt.ResultHash = hashContextPayload(source)
			receipt.ArchiveRef = "session://tool-results/" + ref
			if receipt.SideEffect == "" {
				receipt.SideEffect = ToolUnknownEffect
			}
			toolReceipts = upsertToolCallReceipt(toolReceipts, receipt)
			affected++
			archiveBytes += len(archiveBody)
		}
	}
	projected = provider.ProjectionMessages(projected)
	for i := range projected {
		if projected[i].Role == provider.RoleTool && strings.Contains(projected[i].Content, toolClearMarker) {
			projected[i].ProjectionKind = projectionKindToolPlaceholder
		}
	}
	return toolResultClearPlan{
		projected: projected, receipts: toolReceipts, affected: affected,
		archiveBytes: archiveBytes, foldUnits: clearBefore,
		keptGroups: min(len(completeGroups), keepRecent),
	}
}

func clearedToolResultPlaceholder(message provider.Message, visibleBody, archiveBody, resultRef string) string {
	digest := hashContextPayload(visibleBody)
	marker := toolOutputRecoveryMarker(message.Name, message.ToolCallID, resultRef, len(archiveBody), 0)
	return fmt.Sprintf("%s\ntool=%s call_id=%s status=%s content_hash=%s archive_ref=session://tool-results/%s original_bytes=%d%s",
		toolClearMarker, boundedMarkerField(message.Name, 128, "tool"),
		boundedMarkerField(message.ToolCallID, 128, "-"), toolResultStatus(message),
		digest, resultRef, len(archiveBody), marker)
}

func toolResultStatus(message provider.Message) string {
	if message.ToolExecution != nil && message.ToolExecution.State != "" {
		return message.ToolExecution.State
	}
	return "completed"
}

func upsertToolCallReceipt(existing []ToolCallReceipt, receipt ToolCallReceipt) []ToolCallReceipt {
	for i := range existing {
		if existing[i].CallID == receipt.CallID && existing[i].ArchiveRef == receipt.ArchiveRef {
			existing[i] = receipt
			return existing
		}
	}
	return append(existing, receipt)
}

type toolResultMaintenanceMode int

const (
	toolResultSnip toolResultMaintenanceMode = iota
	toolResultPrune
)

// PruneStats reports one maintenance pass.
type PruneStats struct {
	Results    int
	SavedChars int
	Archive    string
	Mode       toolResultMaintenanceMode
	InputHash  string
	Force      bool
}

// SnipStaleToolResults is a no-op: automatic prune/snip projections are gone.
func (a *Agent) SnipStaleToolResults() (PruneStats, error) {
	return PruneStats{Mode: toolResultSnip}, nil
}

// PruneStaleToolResults is a no-op: automatic prune/snip projections are gone.
func (a *Agent) PruneStaleToolResults() (PruneStats, error) {
	return PruneStats{Mode: toolResultPrune}, nil
}

type snipStrategy struct {
	head      int
	tail      int
	headChars int
	tailChars int
}

var (
	defaultReadOnlySnip      = snipStrategy{head: 80, tail: 12, headChars: 10000, tailChars: 2000}
	defaultSideEffectingSnip = snipStrategy{head: 40, tail: 40, headChars: 8000, tailChars: 8000}
)

func (a *Agent) snipStrategyFor(name string) snipStrategy {
	if a.svc.tools != nil {
		if t, ok := a.svc.tools.Get(name); ok {
			if h, ok := t.(tool.SnipHinter); ok {
				return snipStrategyFromHint(h.SnipHint())
			}
			if t.ReadOnly() {
				return defaultReadOnlySnip
			}
			return defaultSideEffectingSnip
		}
	}
	return defaultReadOnlySnip
}

func snipStrategyFromHint(h tool.SnipHint) snipStrategy {
	return snipStrategy{head: h.Head, tail: h.Tail, headChars: h.HeadChars, tailChars: h.TailChars}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
