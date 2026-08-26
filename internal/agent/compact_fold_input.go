package agent

import (
	"context"
	"time"

	"reasonix/internal/provider"
)

// foldSummary is what compaction reports about turning a fold into a digest.
// It is populated even when the call fails, so telemetry still records how
// large the attempt was and that exactly one call was used.
type foldSummary struct {
	Text       string
	Mode       string
	RequestID  string
	Usage      *provider.Usage
	FoldTokens int
	Spans      int
	InputMode  string
	LatencyMS  int64
}

func summaryInputTokens(msgs []provider.Message) int {
	return estimateMessagesTokens(msgs)
}

func (a *Agent) guardedSummaryInputTokens(msgs []provider.Message) int {
	return a.estimatedVisibleRequestTokens(msgs)
}

func (a *Agent) summaryInputBudget(instructions string) int {
	window := a.effectiveContextWindow()
	if window <= 0 {
		window = a.contextWindow
	}
	if window <= 0 {
		return 0
	}
	return max(0, window-summaryOutputMaxTokens-estimateTextTokens(compactionInstruction)-estimateTextTokens(instructions)-protocolMarginForWindow(window))
}

// foldToSummary turns a fold region into one digest with exactly one provider
// request. Pressure-time tool pruning is durable and happens before this call;
// the summary request never performs a private second transformation.
func (a *Agent) foldToSummary(ctx context.Context, fold []provider.Message, instructions string) (foldSummary, error) {
	return a.foldToSummaryMode(ctx, fold, instructions, SummaryInputCachePrefix)
}

func (a *Agent) foldToSummaryMode(ctx context.Context, fold []provider.Message, instructions, inputMode string) (foldSummary, error) {
	return a.foldToSummaryModeForPurpose(ctx, fold, instructions, inputMode, summaryPurposeHistory)
}

func (a *Agent) foldToSummaryModeForPurpose(ctx context.Context, fold []provider.Message, instructions, inputMode string, purpose summaryPurpose) (foldSummary, error) {
	res := foldSummary{Mode: CompactionModeSummarized, Spans: 1, FoldTokens: summaryInputTokens(fold), InputMode: inputMode}
	return a.singleCallSummaryForPurpose(ctx, res, fold, instructions, purpose)
}

func (a *Agent) singleCallSummary(ctx context.Context, res foldSummary, fold []provider.Message, instructions string) (foldSummary, error) {
	return a.singleCallSummaryForPurpose(ctx, res, fold, instructions, summaryPurposeHistory)
}

func (a *Agent) singleCallSummaryForPurpose(ctx context.Context, res foldSummary, fold []provider.Message, instructions string, purpose summaryPurpose) (foldSummary, error) {
	started := time.Now()
	summary, mode, usage, reqID, err := a.runCompactionSummaryForPurpose(ctx, fold, instructions, purpose)
	res.Text, res.Mode, res.Usage, res.RequestID = summary, mode, usage, reqID
	res.LatencyMS = time.Since(started).Milliseconds()
	return res, err
}

func (a *Agent) foldSummaryWithTelemetry(ctx context.Context, trigger string, fold []provider.Message, instructions string, sourceTokens int, inputMode string) (foldSummary, CompactionTelemetry, error) {
	return a.foldSummaryWithTelemetryForPurpose(ctx, trigger, fold, instructions, sourceTokens, inputMode, summaryPurposeHistory)
}

func (a *Agent) foldSummaryWithTelemetryForPurpose(ctx context.Context, trigger string, fold []provider.Message, instructions string, sourceTokens int, inputMode string, purpose summaryPurpose) (foldSummary, CompactionTelemetry, error) {
	res, err := a.foldToSummaryModeForPurpose(ctx, fold, instructions, inputMode, purpose)
	tele := compactionTelemetryFromSummary(trigger, a.CacheState(), sourceTokens, res)
	tele.MaintenanceMode = maintenanceModeForSummary(purpose, inputMode)
	tele.FoldUnits = len(a.contextUnits(fold))
	tele.SummaryLatencyMS = res.LatencyMS
	tele.BreaksPromptCache = true
	tele.ProviderWindowSource = a.requestBudget(a.summaryRequestForPurpose(fold, instructions, purpose), 0, 0).Source
	if err != nil {
		tele.Error = err.Error()
	}
	return res, tele, err
}

func maintenanceModeForSummary(purpose summaryPurpose, inputMode string) string {
	if purpose == summaryPurposeActiveCheckpoint {
		return MaintenanceActiveCheckpoint
	}
	if inputMode == SummaryInputCachePrefix {
		return MaintenanceHistoryCacheAligned
	}
	return MaintenanceHistoryBoundedNonPrefix
}
