package agent

import (
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

type compactionInstallInput struct {
	trigger                              string
	state                                CompactionState
	canonical, msgs                      []provider.Message
	work                                 compactionSummaryWork
	transcriptVersion, projectionVersion uint64
	generation, projectionGeneration     uint64
	activeTurn                           int64
	viewInputHash                        string
}

func (a *Agent) installCompactionProjection(input compactionInstallInput) (CompactionOutcome, error) {
	work := input.work
	telemetry := work.telemetry
	var projected []provider.Message
	var activeCheckpoint *ActiveTurnCheckpoint
	if work.plan.kind == compactionFoldActiveTurn {
		activeCheckpoint = a.buildActiveTurnCheckpoint(input.state, input.canonical, work.covered, work.summary)
		if activeCheckpoint == nil {
			a.emitCompactionAborted(input.trigger)
			return CompactionNoop, a.irreducibleForMessages(IrreducibleCheckpointInvalid, input.msgs,
				"active checkpoint canonical coverage is unavailable")
		}
		if previous := input.state.Projection.ActiveCheckpoint; previous != nil &&
			(activeCheckpoint.CanonicalEnd <= previous.CanonicalEnd ||
				activeCheckpoint.CoveredSourceHash == previous.CoveredSourceHash ||
				activeCheckpoint.Generation <= previous.Generation) {
			a.emitCompactionAborted(input.trigger)
			return CompactionNoop, a.irreducibleForMessages(IrreducibleNoProjectionProgress, input.msgs,
				"active checkpoint coverage, source hash, or generation did not advance")
		}
		projected = activeTurnCheckpointProjectionMessagesTyped(input.msgs, work.plan.prefixEnd, work.plan.foldEnd, *activeCheckpoint)
		if activeCheckpoint.NarrativeMode == MaintenanceMechanicalFallback {
			telemetry.MaintenanceMode = MaintenanceMechanicalFallback
		}
	} else {
		projected = checkpointProjectionMessages(input.msgs, work.plan.prefixEnd, work.kept, work.summary)
	}
	if len(work.bodySuffix) > 0 {
		projected = append(projected, provider.ProjectionMessages(work.bodySuffix)...)
	}
	spliced := append(append([]provider.Message(nil), projected...), input.canonical[work.covered:]...)
	projectionTokens := a.estimatedVisibleRequestTokens(spliced)
	telemetry.ProjectionTokens = projectionTokens
	telemetry.CoveredCanonicalFrom = input.state.Projection.CoveredCount
	telemetry.CoveredCanonicalTo = work.covered
	telemetry.ProjectionGeneration = input.projectionGeneration + 1
	telemetry.CacheGeneration = input.state.CacheGeneration + 1
	telemetry.UserTurnsKept, telemetry.UserTurnsDropped = work.retention.Kept, work.retention.Dropped
	a.emitCompactionTelemetry(telemetry)
	if err := a.acceptCheckpointCandidate(input.trigger, work.sourceTokens, projectionTokens); err != nil {
		a.emitCompactionAborted(input.trigger)
		return CompactionNoop, err
	}
	outputHash := providerVisibleFingerprint(modelInputMessages(spliced))
	_, err := a.commitSummaryProjection(summaryProjectionCommit{
		canonical: input.canonical, fold: work.folded, projected: projected, result: work.result,
		transcriptVersion: input.transcriptVersion, projectionVersion: input.projectionVersion,
		generation: input.generation, activeTurn: input.activeTurn, trigger: input.trigger,
		projectionGeneration: input.projectionGeneration, cacheGeneration: input.state.CacheGeneration,
		maintenanceMode:      telemetry.MaintenanceMode,
		providerWindowSource: telemetry.ProviderWindowSource,
		summary:              work.summary, inputHash: input.viewInputHash, outputHash: outputHash,
		sourceTokens: work.sourceTokens, projectionTokens: projectionTokens,
		covered: work.covered, coveredFrom: input.state.Projection.CoveredCount,
		toolReceipts:     append([]ToolCallReceipt(nil), input.state.Projection.ToolReceipts...),
		projectionItems:  projectionItemsFromMessages(projected, activeCheckpoint),
		activeCheckpoint: activeCheckpoint,
	})
	if err != nil {
		a.emitCompactionAborted(input.trigger)
		return CompactionNoop, err
	}
	a.svc.sink.Emit(event.Event{Kind: event.CompactionDone, Compaction: event.Compaction{
		Trigger: input.trigger, Messages: len(work.folded), Summary: work.summary,
	}})
	return CompactionInstalled, nil
}
