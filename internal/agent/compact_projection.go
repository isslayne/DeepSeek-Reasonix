package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

const (
	maxCompressAnchorBytes = 512
	maxCompressFocusBytes  = 2000
)

var errCompressStaleContext = errors.New("compress: conversation changed while compression was running; retry with the current context")

// CompressContext implements the context-bound compress tool. It resolves the
// anchor against the current model-visible view and installs a projection only;
// the canonical transcript and checkpoint lineage remain untouched.
func (a *Agent) CompressContext(ctx context.Context, req tool.CompressRequest) (tool.CompressResult, error) {
	direction := strings.TrimSpace(req.Direction)
	anchor := strings.TrimSpace(req.Anchor)
	focus := strings.TrimSpace(req.Focus)
	if direction != "before" && direction != "after" {
		return tool.CompressResult{}, fmt.Errorf("compress: direction must be before or after")
	}
	if anchor == "" {
		return tool.CompressResult{}, fmt.Errorf("compress: anchor must not be empty")
	}
	if len(anchor) > maxCompressAnchorBytes {
		return tool.CompressResult{}, fmt.Errorf("compress: anchor exceeds %d bytes", maxCompressAnchorBytes)
	}
	if len(focus) > maxCompressFocusBytes {
		return tool.CompressResult{}, fmt.Errorf("compress: focus exceeds %d bytes", maxCompressFocusBytes)
	}

	snap := a.snapshotExplicitCompression()
	matches := make([]int, 0, 2)
	for i, msg := range snap.visible {
		if !compressAnchorCandidate(msg) {
			continue
		}
		if strings.Contains(UserMessageText(msg), anchor) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		return tool.CompressResult{}, fmt.Errorf("compress: anchor did not match any current user message; retry with an exact excerpt from a visible user turn")
	}
	if len(matches) > 1 {
		return tool.CompressResult{}, fmt.Errorf("compress: anchor matched %d user messages; retry with a longer unique excerpt", len(matches))
	}

	return a.compressVisibleRange(ctx, snap, CompactionTriggerTool, direction, matches[0], anchorPreview(UserMessageText(snap.visible[matches[0]])), focus)
}

type explicitCompressionSnapshot struct {
	canonical            []provider.Message
	visible              []provider.Message
	transcriptVersion    uint64
	coveredHash          string
	projectionVersion    uint64
	generation           uint64
	projectionGeneration uint64
	cacheGeneration      uint64
	promptCacheKey       string
	toolReceipts         []ToolCallReceipt
}

func (a *Agent) snapshotExplicitCompression() explicitCompressionSnapshot {
	canonical, version := a.sess.conversation.snapshotMessagesVersion()
	cacheKey := a.currentPromptCacheKey()
	a.sess.compactionMu.Lock()
	state := a.sess.compactionState
	a.sess.compactionMu.Unlock()
	visible := canonical
	if projectionValid(state, canonical, cacheKey) {
		if projected := modelVisibleFromProjection(state.Projection, canonical); len(projected) > 0 {
			visible = projected
		}
	}
	return explicitCompressionSnapshot{
		canonical:            canonical,
		visible:              compressionVisibleMessages(visible),
		transcriptVersion:    version,
		coveredHash:          coveredPrefixHash(canonical, len(canonical)),
		projectionVersion:    state.Projection.ProjectionVersion,
		generation:           state.Generation,
		projectionGeneration: state.ProjectionGeneration,
		cacheGeneration:      state.CacheGeneration,
		promptCacheKey:       cacheKey,
		toolReceipts:         append([]ToolCallReceipt(nil), state.Projection.ToolReceipts...),
	}
}

func compressionVisibleMessages(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(msgs)+1)
	for _, msg := range msgs {
		if !msg.LocalOnly {
			summary, user, split := splitLegacyCoalescedSummary(msg)
			if split {
				out = append(out, summary, user)
			} else {
				out = append(out, msg)
			}
		}
	}
	return out
}

// Older schema-v1 sidecars may have persisted a strict-role merge of the
// summary and its following user turn. Split that legacy shape for range
// planning; new sidecars keep the logical messages separate and coalesce only
// on the provider request copy.
func splitLegacyCoalescedSummary(msg provider.Message) (provider.Message, provider.Message, bool) {
	if !isCompactionSummary(msg) {
		return provider.Message{}, provider.Message{}, false
	}
	separator := summaryTagClose + "\n\n"
	i := strings.Index(msg.Content, separator)
	if i < 0 || i+len(separator) >= len(msg.Content) {
		return provider.Message{}, provider.Message{}, false
	}
	summary := msg
	summary.Content = msg.Content[:i+len(summaryTagClose)]
	summary.RawContent = ""
	summary.Images = nil
	summary.ToolCalls = nil
	summary.ResponsesItems = nil
	summary.ServerSearch = nil
	summary.CreatedAt = 0
	user := msg
	user.Content = msg.Content[i+len(separator):]
	user.RawContent = ""
	user.ProjectionKind = ""
	user.Synthetic = false
	return summary, user, true
}

func compressAnchorCandidate(msg provider.Message) bool {
	if msg.Role != provider.RoleUser || msg.LocalOnly || isCompactionSummary(msg) {
		return false
	}
	return IsUserAuthoredTurn(UserMessageText(msg))
}

func anchorPreview(text string) string {
	return truncatePreview(previewProse(text))
}

type visibleCompressionPlan struct {
	result    tool.CompressResult
	foldMask  []bool
	fold      []provider.Message
	firstFold int
}

type preparedVisibleCompression struct {
	fold         []provider.Message
	instructions string
	inputMode    string
}

func (a *Agent) compressVisibleRange(
	ctx context.Context,
	snap explicitCompressionSnapshot,
	trigger string,
	direction string,
	anchorIndex int,
	preview string,
	instructions string,
) (tool.CompressResult, error) {
	a.sess.compactionRunMu.Lock()
	defer a.sess.compactionRunMu.Unlock()
	if !a.explicitCompressionSnapshotCurrent(snap) {
		return tool.CompressResult{}, errCompressStaleContext
	}
	plan, ok := a.planVisibleCompression(snap, direction, anchorIndex, preview)
	if !ok {
		return plan.result, nil
	}
	result := plan.result
	inputMode := SummaryInputNonPrefix
	if direction == "before" && foldMatchesVisiblePrefix(snap.visible, plan.fold) {
		inputMode = SummaryInputCachePrefix
	}

	a.svc.sink.Emit(event.Event{Kind: event.CompactionStarted, Compaction: event.Compaction{Trigger: trigger}})
	prepared, reason, err := a.prepareVisibleCompression(ctx, trigger, plan.fold, instructions, inputMode)
	if err != nil {
		a.emitCompactionAborted(trigger)
		return tool.CompressResult{}, err
	}
	if reason != "" {
		a.emitCompactionAborted(trigger)
		result.Reason = reason
		return result, nil
	}

	res, err := a.foldToSummaryMode(ctx, prepared.fold, prepared.instructions, prepared.inputMode)
	summary := res.Text
	tele := compactionTelemetryFromSummary(trigger, a.CacheState(), result.SourceTokens, res)
	if err != nil {
		tele.Error = err.Error()
		a.emitCompactionTelemetry(tele)
		a.emitCompactionAborted(trigger)
		return tool.CompressResult{}, err
	}
	summary, err = a.interceptCompactionComplete(ctx, summary)
	if err != nil {
		tele.Error = err.Error()
		a.emitCompactionTelemetry(tele)
		a.emitCompactionAborted(trigger)
		return tool.CompressResult{}, err
	}

	projection := buildVisibleCompressionProjection(snap.visible, plan, summary)
	projectionTokens := a.estimatedVisibleRequestTokens(projection)
	tele.ProjectionTokens = projectionTokens
	tele.CoveredCanonicalFrom = 0
	tele.CoveredCanonicalTo = len(snap.canonical)
	tele.ProjectionGeneration = snap.projectionGeneration + 1
	tele.CacheGeneration = snap.cacheGeneration + 1
	result.Messages = len(plan.fold)
	result.ProjectionTokens = projectionTokens
	result.Mode = res.Mode
	if projectionTokens >= result.SourceTokens {
		result.Reason = "compressed context would not be smaller"
		a.emitCompactionTelemetry(tele)
		a.emitCompactionAborted(trigger)
		return result, nil
	}

	inputHash := providerVisibleFingerprint(modelInputMessages(snap.visible))
	outputHash := providerVisibleFingerprint(projection)
	state, err := a.commitSummaryProjection(summaryProjectionCommit{
		canonical: snap.canonical, fold: prepared.fold, projected: projection, result: res,
		transcriptVersion: snap.transcriptVersion, projectionVersion: snap.projectionVersion, generation: snap.generation,
		cacheGeneration:      snap.cacheGeneration,
		projectionGeneration: snap.projectionGeneration,
		activeTurn:           a.activeTurnCreatedAt.Load(), trigger: trigger, summary: summary, maintenanceMode: tele.MaintenanceMode,
		providerWindowSource: tele.ProviderWindowSource,
		inputHash:            inputHash, outputHash: outputHash, sourceTokens: result.SourceTokens, projectionTokens: projectionTokens,
		covered: len(snap.canonical), coveredFrom: 0,
		toolReceipts:    snap.toolReceipts,
		projectionItems: projectionItemsFromMessages(projection, nil),
	})
	if err != nil {
		if errors.Is(err, errCompressStaleContext) {
			tele.Error = err.Error()
			a.emitCompactionTelemetry(tele)
		}
		a.emitCompactionAborted(trigger)
		return tool.CompressResult{}, err
	}
	a.emitCompactionTelemetry(tele)
	a.svc.sink.Emit(event.Event{Kind: event.CompactionDone, Compaction: event.Compaction{
		Trigger: trigger, Messages: len(plan.fold), Summary: summary, Archive: state.LastReceipt.Archive,
	}})
	result.Status = "ok"
	result.Reason = ""
	return result, nil
}

func foldMatchesVisiblePrefix(visible, fold []provider.Message) bool {
	head := 0
	if len(visible) > 0 && visible[0].Role == provider.RoleSystem {
		head = 1
	}
	if len(fold) == 0 || head+len(fold) > len(visible) {
		return false
	}
	return providerVisibleFingerprint(modelInputMessages(fold)) ==
		providerVisibleFingerprint(modelInputMessages(visible[head:head+len(fold)]))
}

func (a *Agent) explicitCompressionSnapshotCurrent(snap explicitCompressionSnapshot) bool {
	current, version := a.sess.conversation.snapshotMessagesVersion()
	a.sess.compactionMu.Lock()
	projectionVersion := a.sess.compactionState.Projection.ProjectionVersion
	generation := a.sess.compactionState.Generation
	a.sess.compactionMu.Unlock()
	return version == snap.transcriptVersion && len(current) == len(snap.canonical) &&
		coveredPrefixHash(current, len(current)) == snap.coveredHash &&
		projectionVersion == snap.projectionVersion && generation == snap.generation &&
		a.currentPromptCacheKey() == snap.promptCacheKey
}

func (a *Agent) planVisibleCompression(snap explicitCompressionSnapshot, direction string, anchorIndex int, preview string) (visibleCompressionPlan, bool) {
	sourceTokens := a.estimatedVisibleRequestTokens(snap.visible)
	plan := visibleCompressionPlan{result: tool.CompressResult{
		Status:           "noop",
		Direction:        direction,
		Anchor:           preview,
		SourceTokens:     sourceTokens,
		ProjectionTokens: sourceTokens,
	}}
	if anchorIndex < 0 || anchorIndex >= len(snap.visible) {
		plan.result.Reason = "anchor is no longer present in the model context"
		return plan, false
	}
	head := 0
	if len(snap.visible) > 0 && snap.visible[0].Role == provider.RoleSystem {
		head = 1
	}
	completedEnd := len(snap.visible)
	if active := a.activeTurnStart(snap.visible); active >= 0 {
		completedEnd = active
	}
	start, end := head, anchorIndex
	if direction == "after" {
		start, end = anchorIndex, completedEnd
	}
	if start < head {
		start = head
	}
	if end > completedEnd {
		end = completedEnd
	}
	if start >= end {
		plan.result.Reason = "selected range is empty"
		return plan, false
	}

	plan.foldMask = make([]bool, len(snap.visible))
	plan.firstFold = len(snap.visible)
	for i, msg := range snap.visible {
		selected := i >= start && i < end
		mergeSummary := i < completedEnd && isCompactionSummary(msg)
		if msg.Role == provider.RoleSystem || i < head || (!selected && !mergeSummary) {
			continue
		}
		plan.foldMask[i] = true
		plan.fold = append(plan.fold, msg)
		if i < plan.firstFold {
			plan.firstFold = i
		}
	}
	if len(plan.fold) == 0 {
		plan.result.Reason = "selected range has no model-visible messages"
		return plan, false
	}
	return plan, true
}

func (a *Agent) prepareVisibleCompression(ctx context.Context, trigger string, fold []provider.Message, instructions, inputMode string) (preparedVisibleCompression, string, error) {
	if a.svc.hooks != nil {
		if hookInstructions := a.svc.hooks.PreCompact(ctx, trigger); hookInstructions != "" {
			if instructions != "" {
				instructions += "\n"
			}
			instructions += hookInstructions
		}
	}
	originalHash := providerVisibleFingerprint(modelInputMessages(fold))
	preparedFold, preparedInstructions, err := a.interceptCompactionPrepare(ctx, fold, instructions)
	if err != nil {
		return preparedVisibleCompression{}, "", err
	}
	preparedFold = modelInputMessages(preparedFold)
	if len(preparedFold) == 0 {
		return preparedVisibleCompression{}, "compaction hook removed the selected range", nil
	}
	if providerVisibleFingerprint(modelInputMessages(preparedFold)) != originalHash {
		inputMode = SummaryInputExtensionRewritten
	}
	return preparedVisibleCompression{fold: preparedFold, instructions: preparedInstructions, inputMode: inputMode}, "", nil
}

func buildVisibleCompressionProjection(visible []provider.Message, plan visibleCompressionPlan, summary string) []provider.Message {
	projection := make([]provider.Message, 0, len(visible)-len(plan.fold)+1)
	for i, msg := range visible {
		if i == plan.firstFold {
			projection = append(projection, formatSummaryMessage(summary))
		}
		if !plan.foldMask[i] {
			projection = append(projection, msg)
		}
	}
	return provider.ProjectionMessages(projection)
}

func compactionTelemetryFromSummary(trigger, cacheState string, sourceTokens int, res foldSummary) CompactionTelemetry {
	tele := CompactionTelemetry{
		Trigger: trigger, CacheState: cacheState, Mode: res.Mode,
		SourceTokens:      sourceTokens,
		ProviderRequestID: res.RequestID,
		FoldTokens:        res.FoldTokens,
		Spans:             1, // one application-layer summary request per transaction
		SummaryInputMode:  res.InputMode,
	}
	usage := res.Usage
	if usage == nil {
		return tele
	}
	tele.InputTokens = usage.PromptTokens
	tele.OutputTokens = usage.CompletionTokens
	tele.CacheHitTokens = usage.CacheHitTokens
	tele.CacheMissTokens = usage.CacheMissTokens
	tele.CacheWriteTokens = usage.CacheWriteTokens
	tele.RequestCount = usage.RequestCount
	if tele.RequestCount <= 0 {
		tele.RequestCount = 1
	}
	return tele
}

// compact writes a context projection; trigger stays "auto"/"manual" for UI cards.
func (a *Agent) compact(ctx context.Context, trigger, instructions string, force bool) error {
	_, err := a.compactToProjection(ctx, trigger, instructions, force, false)
	return err
}

// compactToProjection installs one content-driven summary checkpoint:
// stable prefix + one structured digest + recent verbatim tail.
// The canonical transcript is never rewritten. CompactionNoop means nothing
// was foldable; callers at physical overflow must treat that as hard failure.
// mustFree marks the fold the caller cannot proceed without.
func (a *Agent) compactToProjection(ctx context.Context, trigger, instructions string, force, mustFree bool) (CompactionOutcome, error) {
	a.sess.compactionRunMu.Lock()
	defer a.sess.compactionRunMu.Unlock()
	return a.compactToProjectionLocked(ctx, trigger, instructions, force, mustFree)
}

func (a *Agent) compactToProjectionLocked(ctx context.Context, trigger, instructions string, force, mustFree bool) (CompactionOutcome, error) {
	activeTurn := a.activeTurnCreatedAt.Load()
	canonical, transcriptVersion := a.sess.conversation.snapshotMessagesVersion()
	a.sess.compactionMu.Lock()
	stateSnapshot := a.sess.compactionState
	startProjectionVersion := a.sess.compactionState.Projection.ProjectionVersion
	startGeneration := a.sess.compactionState.Generation
	startProjectionGeneration := a.sess.compactionState.ProjectionGeneration
	a.sess.compactionMu.Unlock()
	msgs, onProjection := a.visibleInputForFold(stateSnapshot, canonical, transcriptVersion)
	viewInputHash := providerVisibleFingerprint(modelInputMessages(msgs))
	plan, ok := a.planCompactionFold(msgs, force)
	if !ok {
		if mustFree {
			return CompactionNoop, a.classifyNoFold(msgs, "no complete semantic unit can be checkpointed")
		}
		return CompactionNoop, nil
	}
	_, preliminaryFold, _ := a.partitionFoldForProjection(msgs[plan.prefixEnd:plan.foldEnd])
	if len(preliminaryFold) == 0 || (!force && !foldEconomics(preliminaryFold)) {
		if mustFree {
			return CompactionNoop, a.irreducibleForMessages(IrreducibleNoTokenSavings, msgs, "the selected semantic units cannot reclaim tokens")
		}
		return CompactionNoop, nil
	}
	fixedPrefixTokens := a.estimatedVisibleRequestTokens(msgs[:plan.prefixEnd])
	fixedPrefixLimit := a.compactTrigger()
	if plan.kind == compactionFoldActiveTurn && a.hardInputCeiling() > 0 {
		// A large but valid active request may already sit above the proactive
		// trigger. It is still recoverable as long as the immutable prefix fits
		// below the physical input ceiling.
		fixedPrefixLimit = a.hardInputCeiling()
	}
	if a.contextWindow > 0 && fixedPrefixTokens >= fixedPrefixLimit {
		return CompactionNoop, a.irreducibleForMessages(IrreducibleImmutableAnchorTooLarge, msgs,
			fmt.Sprintf("fixed prefix requires %d tokens but the safe limit is %d", fixedPrefixTokens, fixedPrefixLimit))
	}

	a.svc.sink.Emit(event.Event{Kind: event.CompactionStarted, Compaction: event.Compaction{Trigger: trigger}})
	if plan.kind == compactionFoldActiveTurn {
		instructions = activeTurnCheckpointInstructions(instructions)
	}
	if a.svc.hooks != nil {
		if hookInstr := a.svc.hooks.PreCompact(ctx, trigger); hookInstr != "" {
			if instructions != "" {
				instructions += "\n"
			}
			instructions += hookInstr
		}
	}
	purpose := summaryPurposeHistory
	if plan.kind == compactionFoldActiveTurn {
		purpose = summaryPurposeActiveCheckpoint
	}
	work, ok, err := a.summarizeCompactionFold(ctx, trigger, instructions, mustFree, stateSnapshot, msgs, onProjection, plan, purpose)
	if err != nil || !ok {
		return CompactionNoop, err
	}
	return a.installCompactionProjection(compactionInstallInput{
		trigger: trigger, state: stateSnapshot, canonical: canonical, msgs: msgs, work: work,
		transcriptVersion: transcriptVersion, projectionVersion: startProjectionVersion,
		generation: startGeneration, projectionGeneration: startProjectionGeneration,
		activeTurn: activeTurn, viewInputHash: viewInputHash,
	})
}

type compactionSummaryWork struct {
	plan                     compactionFoldPlan
	covered                  int
	bodySuffix, kept, folded []provider.Message
	retention                userTurnRetention
	result                   foldSummary
	telemetry                CompactionTelemetry
	summary                  string
	sourceTokens             int
}

func (a *Agent) summarizeCompactionFold(ctx context.Context, trigger, instructions string, mustFree bool, state CompactionState, msgs []provider.Message, onProjection bool, plan compactionFoldPlan, purpose summaryPurpose) (compactionSummaryWork, bool, error) {
	if mustFree || plan.kind == compactionFoldActiveTurn {
		plan.foldEnd = a.maximumSafeSummaryPrefixEndForPurpose(msgs, plan.summaryStart, plan.foldEnd, instructions, purpose)
		if plan.kind == compactionFoldActiveTurn {
			plan.foldEnd = a.completedActiveTurnPrefixEnd(msgs, plan.prefixEnd, plan.foldEnd)
		}
		if plan.foldEnd <= plan.prefixEnd {
			a.emitCompactionAborted(trigger)
			return compactionSummaryWork{}, false, a.irreducibleForMessages(IrreducibleSummaryRequestTooLarge, msgs,
				"no complete semantic-unit prefix leaves the minimum summary output budget")
		}
	}
	plan, covered, bodySuffix, mechanical := a.advanceRollingCheckpointFold(state, msgs, plan, instructions, purpose, onProjection)
	priorActiveCoverage := state.Projection.CoveredCount
	if state.Projection.ActiveCheckpoint != nil {
		priorActiveCoverage = state.Projection.ActiveCheckpoint.CanonicalEnd
	}
	if plan.kind == compactionFoldActiveTurn && onProjection &&
		(state.Projection.ActiveCheckpoint != nil || hasActiveTurnCheckpoint(state.Projection.Messages)) &&
		covered <= priorActiveCoverage {
		a.emitCompactionAborted(trigger)
		return compactionSummaryWork{}, false, a.irreducibleForMessages(IrreducibleNoProjectionProgress, msgs,
			fmt.Sprintf("the rolling checkpoint would not advance canonical coverage: covered=%d prior=%d fold_end=%d body=%d",
				covered, state.Projection.CoveredCount, plan.foldEnd, len(state.Projection.Messages)))
	}
	kept, folded, retention := a.partitionFoldForProjection(msgs[plan.prefixEnd:plan.foldEnd])
	if len(folded) == 0 {
		a.emitCompactionAborted(trigger)
		if mustFree {
			return compactionSummaryWork{}, false, a.irreducibleForMessages(IrreducibleNoCompletedUnit, msgs,
				"the selected range contains no foldable semantic units")
		}
		return compactionSummaryWork{}, false, nil
	}
	sourceTokens := a.estimatedVisibleRequestTokens(msgs)
	if mechanical {
		return a.mechanicalCheckpointWork(trigger, state, plan, covered, bodySuffix, kept, folded, retention, sourceTokens), true, nil
	}
	summaryFold := modelInputMessages(msgs[plan.summaryStart:plan.foldEnd])
	originalFoldHash := providerVisibleFingerprint(summaryFold)
	var err error
	summaryFold, instructions, err = a.interceptCompactionPrepare(ctx, summaryFold, instructions)
	if err != nil {
		a.emitCompactionAborted(trigger)
		return compactionSummaryWork{}, false, err
	}
	if len(summaryFold) == 0 {
		a.emitCompactionAborted(trigger)
		if mustFree {
			return compactionSummaryWork{}, false, a.irreducibleForMessages(IrreducibleNoCompletedUnit, msgs,
				"the compaction hook removed every model-visible semantic unit")
		}
		return compactionSummaryWork{}, false, nil
	}
	inputMode := SummaryInputCachePrefix
	if providerVisibleFingerprint(modelInputMessages(summaryFold)) != originalFoldHash {
		inputMode = SummaryInputExtensionRewritten
	}
	result, telemetry, err := a.foldSummaryWithTelemetryForPurpose(ctx, trigger, summaryFold, instructions, sourceTokens, inputMode, purpose)
	if err != nil {
		a.emitCompactionTelemetry(telemetry)
		a.emitCompactionAborted(trigger)
		return compactionSummaryWork{}, false, err
	}
	summary, err := a.interceptCompactionComplete(ctx, result.Text)
	if err != nil {
		telemetry.Error = err.Error()
		a.emitCompactionTelemetry(telemetry)
		a.emitCompactionAborted(trigger)
		return compactionSummaryWork{}, false, err
	}
	return compactionSummaryWork{
		plan: plan, covered: covered, bodySuffix: bodySuffix, kept: kept, folded: folded,
		retention: retention, result: result, telemetry: telemetry, summary: summary, sourceTokens: sourceTokens,
	}, true, nil
}

func (a *Agent) mechanicalCheckpointWork(trigger string, state CompactionState, plan compactionFoldPlan, covered int, bodySuffix, kept, folded []provider.Message, retention userTurnRetention, sourceTokens int) compactionSummaryWork {
	summary := "Checkpoint advanced from host-verified operation receipts."
	if previous := state.Projection.ActiveCheckpoint; previous != nil && strings.TrimSpace(previous.Narrative) != "" {
		summary = previous.Narrative
	}
	result := foldSummary{
		Text: summary, Mode: CompactionModeSummarized,
		InputMode: SummaryInputMechanicalFallback,
	}
	telemetry := compactionTelemetryFromSummary(trigger, a.CacheState(), sourceTokens, result)
	telemetry.MaintenanceMode = MaintenanceMechanicalFallback
	telemetry.FoldUnits = len(a.contextUnits(folded))
	telemetry.BreaksPromptCache = true
	telemetry.ProviderWindowSource = a.lastAdmission().Source
	return compactionSummaryWork{
		plan: plan, covered: covered, bodySuffix: bodySuffix, kept: kept, folded: folded,
		retention: retention, result: result, telemetry: telemetry, summary: summary, sourceTokens: sourceTokens,
	}
}

func (a *Agent) advanceRollingCheckpointFold(state CompactionState, msgs []provider.Message, plan compactionFoldPlan, instructions string, purpose summaryPurpose, onProjection bool) (compactionFoldPlan, int, []provider.Message, bool) {
	covered, suffix := projectionCoverageForFold(state, msgs, plan.foldEnd, onProjection)
	if plan.kind != compactionFoldActiveTurn || !onProjection || state.Projection.ActiveCheckpoint == nil ||
		covered > state.Projection.ActiveCheckpoint.CanonicalEnd {
		return plan, covered, suffix, false
	}
	bodyEnd := len(state.Projection.Messages)
	progressEnd := nextActiveTurnProgressEnd(a.contextUnits(msgs), bodyEnd)
	if progressEnd <= plan.foldEnd {
		return plan, covered, suffix, false
	}
	safeEnd := a.maximumSafeSummaryPrefixEndForPurpose(msgs, plan.summaryStart, progressEnd, instructions, purpose)
	safeEnd = a.completedActiveTurnPrefixEnd(msgs, plan.prefixEnd, safeEnd)
	if safeEnd > plan.foldEnd {
		plan.foldEnd = safeEnd
		covered, suffix = projectionCoverageForFold(state, msgs, plan.foldEnd, onProjection)
		return plan, covered, suffix, false
	}
	// The prior typed checkpoint plus one complete operation no longer fits in a
	// summary request. Advance using host-generated receipts and retain the prior
	// narrative instead of retrying the same no-progress projection.
	plan.foldEnd = progressEnd
	covered, suffix = projectionCoverageForFold(state, msgs, plan.foldEnd, onProjection)
	return plan, covered, suffix, true
}

// projectionCoverageForFold maps a working-view boundary to canonical
// coverage. A suffix inside an existing frozen body remains in the new body
// because it has no corresponding canonical tail to splice from.
func projectionCoverageForFold(state CompactionState, msgs []provider.Message, start int, onProjection bool) (int, []provider.Message) {
	if !onProjection {
		return start, nil
	}
	body := len(state.Projection.Messages)
	prior := state.Projection.CoveredCount
	if start < body {
		return prior, msgs[start:body]
	}
	return prior + (start - body), nil
}

// visibleInputForFold prefers the prior projection + new history over full
// canonical. The second return reports whether the projection was used, so
// fold boundaries can be translated back to canonical indices.
func (a *Agent) visibleInputForFold(state CompactionState, canonical []provider.Message, transcriptVersion uint64) ([]provider.Message, bool) {
	if projectionValid(state, canonical, a.currentPromptCacheKey()) {
		if projected := modelVisibleFromProjection(state.Projection, canonical); len(projected) > 0 {
			return projected, true
		}
	}
	return canonical, false
}

func checkpointProjectionMessages(msgs []provider.Message, head int, kept []provider.Message, summary string) []provider.Message {
	projMsgs := make([]provider.Message, 0, head+1+len(kept))
	projMsgs = append(projMsgs, msgs[:head]...)
	projMsgs = append(projMsgs, formatSummaryMessage(summary))
	projMsgs = append(projMsgs, kept...)
	return provider.ProjectionMessages(projMsgs)
}

// acceptCheckpointCandidate requires real savings and, for automatic
// maintenance, a result below the physical input ceiling.
func (a *Agent) acceptCheckpointCandidate(trigger string, sourceTokens, candidateTokens int) error {
	window := a.effectiveContextWindow()
	budget := RequestBudget{EffectiveWindow: window, EstimatedPrompt: candidateTokens,
		ProtocolMargin: protocolMarginForWindow(window), HardInputCeiling: a.hardInputCeiling()}
	if candidateTokens >= sourceTokens {
		return irreducible(IrreducibleNoTokenSavings, budget,
			fmt.Sprintf("checkpoint candidate would not reduce tokens (%d >= %d)", candidateTokens, sourceTokens))
	}
	hard := a.hardInputCeiling()
	if trigger != CompactionTriggerManual && hard > 0 && candidateTokens >= hard {
		return irreducible(IrreducibleNoProjectionProgress, budget,
			fmt.Sprintf("checkpoint candidate %d remains at or above physical ceiling %d", candidateTokens, hard))
	}
	return nil
}

// planFoldRegion retains the legacy tuple contract used by focused tests.
// New callers should use planCompactionFold so they can distinguish a normal
// historical fold from a rolling active-turn checkpoint.
func (a *Agent) planFoldRegion(msgs []provider.Message, force bool) (head, start int, ok bool) {
	plan, ok := a.planCompactionFold(msgs, force)
	if !ok {
		return 0, 0, false
	}
	return plan.prefixEnd, plan.foldEnd, true
}

// maximumSafeSummaryPrefixEnd returns the largest balanced contiguous prefix
// whose exact summary request leaves the collector's minimum output budget.
// The remaining middle and tail stay verbatim in the projection.
func (a *Agent) maximumSafeSummaryPrefixEnd(msgs []provider.Message, head, end int, instructions string) int {
	return a.maximumSafeSummaryPrefixEndForPurpose(msgs, head, end, instructions, summaryPurposeHistory)
}

func (a *Agent) maximumSafeSummaryPrefixEndForPurpose(msgs []provider.Message, head, end int, instructions string, purpose summaryPurpose) int {
	window := a.effectiveContextWindow()
	if window <= 0 || head < 0 || end <= head || end > len(msgs) {
		return end
	}
	policy := contextBudgetPolicyOf(a.svc.prov)
	if policy.WindowMode == provider.ContextWindowUnknown {
		// A learned overflow makes an unknown gateway shared-window. Otherwise
		// preserve the request because the configured window may be an estimate.
		if a.lastAdmission().ObservedWindow <= 0 {
			return end
		}
		policy.WindowMode = provider.ContextWindowShared
	}
	units := a.contextUnits(msgs)
	boundaries := make([]int, 0, len(units))
	for _, unit := range units {
		if unit.VisibleEnd > head && unit.VisibleEnd <= end {
			boundaries = append(boundaries, unit.VisibleEnd)
		}
	}
	if len(boundaries) == 0 {
		return head
	}
	fits := func(candidate int) bool {
		request := a.summaryRequestForPurpose(msgs[head:candidate], instructions, purpose)
		desired, minimum := a.summaryOutputEnvelope(purpose)
		budget := a.requestBudget(request, desired, minimum)
		if policy.WindowMode != provider.ContextWindowShared {
			return budget.EstimatedPrompt <= budget.HardInputCeiling
		}
		return budget.EffectiveOutput >= budget.MinimumOutput
	}
	if boundaries[len(boundaries)-1] == end && fits(end) {
		return end
	}

	low, high, best := 0, len(boundaries)-1, head
	for low <= high {
		mid := low + (high-low)/2
		candidate := boundaries[mid]
		if fits(candidate) {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return best
}

type userTurnRetention struct {
	Kept    int
	Dropped int
}

func (a *Agent) partitionFoldForProjection(region []provider.Message) (kept, fold []provider.Message, retention userTurnRetention) {
	for _, m := range region {
		if m.LocalOnly {
			continue
		}
		fold = append(fold, m)
		if m.Role == provider.RoleUser && !isCompactionSummary(m) {
			retention.Dropped++
		}
	}
	return kept, fold, retention
}

// runCompactionSummary uses the single local summarizer path for every provider.
func (a *Agent) runCompactionSummary(ctx context.Context, fold []provider.Message, instructions string) (summary, mode string, usage *provider.Usage, providerReqID string, err error) {
	return a.runCompactionSummaryForPurpose(ctx, fold, instructions, summaryPurposeHistory)
}

func (a *Agent) runCompactionSummaryForPurpose(ctx context.Context, fold []provider.Message, instructions string, purpose summaryPurpose) (summary, mode string, usage *provider.Usage, providerReqID string, err error) {
	summary, usage, err = a.summarizeOnceForPurpose(ctx, fold, instructions, purpose)
	if err != nil {
		return "", CompactionModeSummarized, usage, "", err
	}
	return summary, CompactionModeSummarized, usage, "", nil
}
