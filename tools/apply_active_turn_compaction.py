#!/usr/bin/env python3
from __future__ import annotations

import pathlib

TARGET = pathlib.Path("internal/agent/compact_projection.go")


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"{label}: expected exactly one match, found {count}")
    return text.replace(old, new, 1)


def main() -> None:
    text = TARGET.read_text(encoding="utf-8")

    text = replace_once(
        text,
        '''\thead, start, ok := a.planFoldRegion(msgs, force)
\tif !ok {
\t\treturn CompactionNoop, nil
\t}
\t_, preliminaryFold, _ := a.partitionFoldForProjection(msgs[head:start])
\tif len(preliminaryFold) == 0 || (!force && !foldEconomics(preliminaryFold)) {
\t\treturn CompactionNoop, nil
\t}
\tfixedPrefixTokens := a.estimatedVisibleRequestTokens(msgs[:head])
\tif a.contextWindow > 0 && fixedPrefixTokens >= a.compactTrigger() {
\t\treturn CompactionNoop, fmt.Errorf("%w: fixed prefix (%d tokens) already exceeds trigger (%d)", errCheckpointRejected, fixedPrefixTokens, a.compactTrigger())
\t}
''',
        '''\tplan, ok := a.planCompactionFold(msgs, force)
\tif !ok {
\t\treturn CompactionNoop, nil
\t}
\t_, preliminaryFold, _ := a.partitionFoldForProjection(msgs[plan.prefixEnd:plan.foldEnd])
\tif len(preliminaryFold) == 0 || (!force && !foldEconomics(preliminaryFold)) {
\t\treturn CompactionNoop, nil
\t}
\tfixedPrefixTokens := a.estimatedVisibleRequestTokens(msgs[:plan.prefixEnd])
\tfixedPrefixLimit := a.compactTrigger()
\tif plan.kind == compactionFoldActiveTurn && a.hardInputCeiling() > 0 {
\t\t// A large but valid active request may already sit above the proactive
\t\t// trigger. It is still recoverable as long as the immutable prefix fits
\t\t// below the physical input ceiling.
\t\tfixedPrefixLimit = a.hardInputCeiling()
\t}
\tif a.contextWindow > 0 && fixedPrefixTokens >= fixedPrefixLimit {
\t\treturn CompactionNoop, fmt.Errorf("%w: fixed prefix (%d tokens) already exceeds safe limit (%d)", errCheckpointRejected, fixedPrefixTokens, fixedPrefixLimit)
\t}
''',
        "select compaction fold plan",
    )

    text = replace_once(
        text,
        '''\ta.svc.sink.Emit(event.Event{Kind: event.CompactionStarted, Compaction: event.Compaction{Trigger: trigger}})
\tif a.svc.hooks != nil {
''',
        '''\ta.svc.sink.Emit(event.Event{Kind: event.CompactionStarted, Compaction: event.Compaction{Trigger: trigger}})
\tif plan.kind == compactionFoldActiveTurn {
\t\tinstructions = activeTurnCheckpointInstructions(instructions)
\t}
\tif a.svc.hooks != nil {
''',
        "inject active-turn checkpoint instructions",
    )

    text = replace_once(
        text,
        '''\tif mustFree {
\t\tstart = a.maximumSafeSummaryPrefixEnd(msgs, head, start, instructions)
\t\tif start <= head {
\t\t\ta.emitCompactionAborted(trigger)
\t\t\treturn CompactionNoop, fmt.Errorf("%w: no balanced prefix leaves enough room for a summary response", errCheckpointRejected)
\t\t}
\t}

\tcovered, bodySuffix := projectionCoverageForFold(stateSnapshot, msgs, start, onProjection)
\tkept, fold, retention := a.partitionFoldForProjection(msgs[head:start])
\tif len(fold) == 0 {
\t\ta.emitCompactionAborted(trigger)
\t\treturn CompactionNoop, nil
\t}
\toriginalFoldHash := providerVisibleFingerprint(modelInputMessages(fold))
\tvar err error
\tfold, instructions, err = a.interceptCompactionPrepare(ctx, fold, instructions)
\tif err != nil {
\t\ta.emitCompactionAborted(trigger)
\t\treturn CompactionNoop, err
\t}
\tif len(fold) == 0 {
\t\ta.emitCompactionAborted(trigger)
\t\treturn CompactionNoop, nil
\t}

\tsourceTokens := a.estimatedVisibleRequestTokens(msgs)
\tinputMode := SummaryInputCachePrefix
\tif providerVisibleFingerprint(modelInputMessages(fold)) != originalFoldHash {
\t\tinputMode = SummaryInputExtensionRewritten
\t}
\tres, tele, err := a.foldSummaryWithTelemetry(ctx, trigger, fold, instructions, sourceTokens, inputMode)
''',
        '''\tif mustFree || plan.kind == compactionFoldActiveTurn {
\t\tplan.foldEnd = a.maximumSafeSummaryPrefixEnd(msgs, plan.summaryStart, plan.foldEnd, instructions)
\t\tif plan.kind == compactionFoldActiveTurn {
\t\t\tplan.foldEnd = completedActiveTurnPrefixEnd(msgs, plan.prefixEnd, plan.foldEnd)
\t\t}
\t\tif plan.foldEnd <= plan.prefixEnd {
\t\t\ta.emitCompactionAborted(trigger)
\t\t\treturn CompactionNoop, fmt.Errorf("%w: no balanced completed prefix leaves enough room for a summary response", errCheckpointRejected)
\t\t}
\t}

\tcovered, bodySuffix := projectionCoverageForFold(stateSnapshot, msgs, plan.foldEnd, onProjection)
\tkept, removedFold, retention := a.partitionFoldForProjection(msgs[plan.prefixEnd:plan.foldEnd])
\tif len(removedFold) == 0 {
\t\ta.emitCompactionAborted(trigger)
\t\treturn CompactionNoop, nil
\t}
\tsummaryFold := modelInputMessages(msgs[plan.summaryStart:plan.foldEnd])
\toriginalFoldHash := providerVisibleFingerprint(summaryFold)
\tvar err error
\tsummaryFold, instructions, err = a.interceptCompactionPrepare(ctx, summaryFold, instructions)
\tif err != nil {
\t\ta.emitCompactionAborted(trigger)
\t\treturn CompactionNoop, err
\t}
\tif len(summaryFold) == 0 {
\t\ta.emitCompactionAborted(trigger)
\t\treturn CompactionNoop, nil
\t}

\tsourceTokens := a.estimatedVisibleRequestTokens(msgs)
\tinputMode := SummaryInputCachePrefix
\tif plan.kind == compactionFoldActiveTurn {
\t\tinputMode = SummaryInputNonPrefix
\t}
\tif providerVisibleFingerprint(modelInputMessages(summaryFold)) != originalFoldHash {
\t\tinputMode = SummaryInputExtensionRewritten
\t}
\tres, tele, err := a.foldSummaryWithTelemetry(ctx, trigger, summaryFold, instructions, sourceTokens, inputMode)
''',
        "prepare bounded active-turn summary",
    )

    text = replace_once(
        text,
        '''\tprojMsgs := checkpointProjectionMessages(msgs, head, kept, summary)
\tif len(bodySuffix) > 0 {
''',
        '''\tvar projMsgs []provider.Message
\tif plan.kind == compactionFoldActiveTurn {
\t\tprojMsgs = activeTurnCheckpointProjectionMessages(msgs, plan.prefixEnd, plan.foldEnd, summary)
\t} else {
\t\tprojMsgs = checkpointProjectionMessages(msgs, plan.prefixEnd, kept, summary)
\t}
\tif len(bodySuffix) > 0 {
''',
        "build mode-specific projection",
    )

    text = replace_once(
        text,
        '''\t_, err = a.commitSummaryProjection(summaryProjectionCommit{
\t\tcanonical: canonical, fold: fold, projected: projMsgs, result: res,
''',
        '''\t_, err = a.commitSummaryProjection(summaryProjectionCommit{
\t\tcanonical: canonical, fold: removedFold, projected: projMsgs, result: res,
''',
        "commit removed fold",
    )

    text = replace_once(
        text,
        '''\ta.svc.sink.Emit(event.Event{Kind: event.CompactionDone, Compaction: event.Compaction{
\t\tTrigger: trigger, Messages: len(fold), Summary: summary,
\t}})
''',
        '''\ta.svc.sink.Emit(event.Event{Kind: event.CompactionDone, Compaction: event.Compaction{
\t\tTrigger: trigger, Messages: len(removedFold), Summary: summary,
\t}})
''',
        "emit removed message count",
    )

    text = replace_once(
        text,
        '''// planFoldRegion returns [head:start] to fold; force shrinks the recent tail.
func (a *Agent) planFoldRegion(msgs []provider.Message, force bool) (head, start int, ok bool) {
\thead, start, ok = a.planCompaction(msgs, minCompactMessages, force)
\tif !ok {
\t\thead, start, ok = a.planCompaction(msgs, 1, force)
\t}
\tif !ok {
\t\treturn head, start, false
\t}
\tif active := a.activeTurnStart(msgs); active >= head && active < start {
\t\tstart = active
\t}
\treturn head, start, start > head
}
''',
        '''// planFoldRegion retains the legacy tuple contract used by focused tests.
// New callers should use planCompactionFold so they can distinguish a normal
// historical fold from a rolling active-turn checkpoint.
func (a *Agent) planFoldRegion(msgs []provider.Message, force bool) (head, start int, ok bool) {
\tplan, ok := a.planCompactionFold(msgs, force)
\tif !ok {
\t\treturn 0, 0, false
\t}
\treturn plan.prefixEnd, plan.foldEnd, true
}
''',
        "replace fold planner wrapper",
    )

    TARGET.write_text(text, encoding="utf-8")


if __name__ == "__main__":
    main()
