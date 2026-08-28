package agent

import (
	"slices"
	"strings"

	"reasonix/internal/provider"
)

const (
	activeTurnCheckpointTagOpen  = "<active-turn-checkpoint>"
	activeTurnCheckpointTagClose = "</active-turn-checkpoint>"
)

const activeTurnCheckpointInstruction = `This compaction is a rolling checkpoint inside one user turn that is still running.
The original user request remains verbatim outside the checkpoint. Preserve exact completed work, files, identifiers, decisions, unresolved errors, and the next concrete action. Do not imply that the task or turn is complete. Do not omit a later user correction if one appears in the fold.
Return exactly one JSON object with these keys: standing_constraints (array of strings), decisions (array of strings), errors (array of strings), pending (array of strings), next_action (string), narrative (string). Do not include call IDs, hashes, archive references, coverage, or generation: the host attaches those verified fields. Output JSON only. Do not call tools.`

const activeTurnContinuation = `<compaction-summary>
The original user request is still active. Continue from the active-turn checkpoint above without repeating completed work. Treat the retained recent messages below as the live tail of the same turn.
</compaction-summary>`

type compactionFoldKind uint8

const (
	compactionFoldHistory compactionFoldKind = iota
	compactionFoldActiveTurn
)

type compactionFoldPlan struct {
	kind         compactionFoldKind
	prefixEnd    int
	summaryStart int
	foldEnd      int
}

func (p compactionFoldPlan) valid(msgs []provider.Message) bool {
	return p.prefixEnd >= 0 && p.summaryStart >= 0 && p.summaryStart <= p.prefixEnd &&
		p.prefixEnd < p.foldEnd && p.foldEnd <= len(msgs)
}

// planCompactionFold first preserves the existing cache-friendly behavior:
// completed historical turns are folded before any part of the live turn. When
// no economical historical prefix remains, it falls back to a rolling
// checkpoint over the completed portion of the active turn. The active user
// request itself remains verbatim in the projection.
func (a *Agent) planCompactionFold(msgs []provider.Message, force bool) (compactionFoldPlan, bool) {
	head, plannedEnd, ok := a.planCompaction(msgs, minCompactMessages, force)
	if !ok {
		head, plannedEnd, ok = a.planCompaction(msgs, 1, force)
	}
	if !ok {
		return compactionFoldPlan{}, false
	}

	active := a.activeTurnStart(msgs)
	if active < head || active >= plannedEnd {
		plan := compactionFoldPlan{
			kind:         compactionFoldHistory,
			prefixEnd:    head,
			summaryStart: head,
			foldEnd:      plannedEnd,
		}
		return plan, plan.valid(msgs)
	}

	// Prefer the old-turn prefix while it still pays for a summary. This keeps
	// active-turn checkpointing as a fallback instead of changing the common
	// append-only/cache-reset cadence.
	if active > head {
		_, historicalFold, _ := a.partitionFoldForProjection(msgs[head:active])
		if len(historicalFold) > 0 && (force || foldEconomics(historicalFold)) {
			plan := compactionFoldPlan{
				kind:         compactionFoldHistory,
				prefixEnd:    head,
				summaryStart: head,
				foldEnd:      active,
			}
			return plan, plan.valid(msgs)
		}
	}

	foldStart := active + 1
	foldEnd := a.completedActiveTurnFoldEnd(msgs, foldStart, plannedEnd)
	if force {
		// Physical overflow may checkpoint the newest complete atomic group.
		foldEnd = a.completedActiveTurnPrefixEnd(msgs, foldStart, plannedEnd)
	}
	if foldEnd <= foldStart {
		return compactionFoldPlan{}, false
	}
	_, completedFold, _ := a.partitionFoldForProjection(msgs[foldStart:foldEnd])
	if len(completedFold) == 0 || (!force && !foldEconomics(completedFold)) {
		return compactionFoldPlan{}, false
	}
	plan := compactionFoldPlan{
		kind:      compactionFoldActiveTurn,
		prefixEnd: foldStart,
		// Bound the summary to the active task plus completed work; the request
		// builder adds the system anchor exactly once.
		summaryStart: active,
		foldEnd:      foldEnd,
	}
	return plan, plan.valid(msgs)
}

// completedActiveTurnFoldEnd retains the newest provider-visible resume unit.
func (a *Agent) completedActiveTurnFoldEnd(msgs []provider.Message, start, limit int) int {
	return completedActiveTurnFoldEndWithOptions(msgs, start, limit, a.contextUnitBuildOptions())
}

func completedActiveTurnFoldEndWithOptions(msgs []provider.Message, start, limit int, options contextUnitBuildOptions) int {
	safe, lastUnitStart := completedActiveTurnBoundary(msgs, start, limit, options)
	if safe <= start {
		return start
	}
	if hasProviderVisibleMessage(msgs[safe:]) {
		return safe
	}
	if lastUnitStart >= start {
		return lastUnitStart
	}
	return start
}

// completedActiveTurnPrefixEnd returns the largest prefix ending on a safe
// message boundary. It never crosses a later user-authored turn and never
// splits an assistant tool_calls record from its complete result group.
func completedActiveTurnPrefixEnd(msgs []provider.Message, start, limit int) int {
	safe, _ := completedActiveTurnBoundary(msgs, start, limit, contextUnitBuildOptions{})
	return safe
}

func (a *Agent) completedActiveTurnPrefixEnd(msgs []provider.Message, start, limit int) int {
	safe, _ := completedActiveTurnBoundary(msgs, start, limit, a.contextUnitBuildOptions())
	return safe
}

// nextActiveTurnProgressEnd returns the first complete provider-visible unit
// after start. It skips local/synthetic bookkeeping, but never crosses a real
// user steer or an incomplete tool group.
func nextActiveTurnProgressEnd(units []ContextUnit, start int) int {
	safe := start
	for _, unit := range units {
		if unit.VisibleEnd <= start {
			continue
		}
		if unit.VisibleStart != safe || (unit.Kind == UnitUserTurn && unit.UserAuthored) ||
			(unit.Kind == UnitToolGroup && !unit.Complete) {
			return start
		}
		safe = unit.VisibleEnd
		if unit.ProviderVisible && unit.Kind != UnitSyntheticControl {
			return safe
		}
	}
	return start
}

func (a *Agent) contextUnitBuildOptions() contextUnitBuildOptions {
	if a == nil {
		return contextUnitBuildOptions{}
	}
	return contextUnitBuildOptions{allowPositionalToolPairing: provider.SupportsPositionalToolPairing(a.svc.prov)}
}

func completedActiveTurnBoundary(msgs []provider.Message, start, limit int, options contextUnitBuildOptions) (safe, lastUnitStart int) {
	if start < 0 {
		start = 0
	}
	if limit > len(msgs) {
		limit = len(msgs)
	}
	if start >= limit {
		return start, -1
	}

	units := buildContextUnits(msgs, options)
	if !contextUnitBoundary(units, start) {
		return start, -1
	}
	safe = start
	lastUnitStart = -1
	for _, unit := range units {
		if unit.VisibleEnd <= start {
			continue
		}
		if unit.VisibleStart != safe || unit.VisibleEnd > limit {
			break
		}
		if unit.Kind == UnitUserTurn && unit.UserAuthored {
			break
		}
		if unit.Kind == UnitToolGroup && !unit.Complete {
			break
		}
		safe = unit.VisibleEnd
		if unit.ProviderVisible {
			lastUnitStart = unit.VisibleStart
		}
	}
	return safe, lastUnitStart
}

func hasProviderVisibleMessage(msgs []provider.Message) bool {
	for _, msg := range msgs {
		if !msg.LocalOnly {
			return true
		}
	}
	return false
}

func hasActiveTurnCheckpoint(msgs []provider.Message) bool {
	return slices.ContainsFunc(msgs, isActiveTurnCheckpointMessage)
}

func isActiveTurnCheckpointMessage(msg provider.Message) bool {
	return msg.ProjectionKind == projectionKindActiveCheckpoint
}

func activeTurnCheckpointInstructions(instructions string) string {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return activeTurnCheckpointInstruction
	}
	return instructions + "\n\n" + activeTurnCheckpointInstruction
}

func activeTurnCheckpointProjectionMessages(msgs []provider.Message, prefixEnd, foldEnd int, summary string) []provider.Message {
	checkpoint := ActiveTurnCheckpoint{SchemaVersion: 1, Narrative: strings.TrimSpace(summary)}
	return activeTurnCheckpointProjectionMessagesTyped(msgs, prefixEnd, foldEnd, checkpoint)
}

func activeTurnCheckpointProjectionMessagesTyped(msgs []provider.Message, prefixEnd, foldEnd int, checkpoint ActiveTurnCheckpoint) []provider.Message {
	proj := make([]provider.Message, 0, prefixEnd+2)
	proj = append(proj, msgs[:prefixEnd]...)
	proj = append(proj, formatTypedActiveTurnCheckpoint(checkpoint))
	// When the retained tail already starts with a user message it supplies the
	// continuation boundary. Otherwise add a synthetic user marker so the next
	// sampling request ends in a legal user→assistant/tool continuation shape.
	if foldEnd >= len(msgs) || msgs[foldEnd].Role != provider.RoleUser {
		proj = append(proj, provider.Message{
			Role: provider.RoleUser, Content: activeTurnContinuation,
			ProjectionKind: projectionKindSyntheticControl, Synthetic: true,
		})
	}
	return provider.ProjectionMessages(proj)
}
