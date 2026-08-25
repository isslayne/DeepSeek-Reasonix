package agent

import (
	"strings"

	"reasonix/internal/provider"
)

const (
	activeTurnCheckpointTagOpen  = "<active-turn-checkpoint>"
	activeTurnCheckpointTagClose = "</active-turn-checkpoint>"
)

const activeTurnCheckpointInstruction = `This compaction is a rolling checkpoint inside one user turn that is still running.
The original user request remains verbatim outside the checkpoint. Preserve exact completed work, tool outcomes, files, identifiers, decisions, unresolved errors, and the next concrete action. Do not imply that the task or turn is complete. Do not omit a later user correction if one appears in the fold.`

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
	foldEnd := completedActiveTurnPrefixEnd(msgs, foldStart, plannedEnd)
	if foldEnd <= foldStart {
		return compactionFoldPlan{}, false
	}
	_, completedFold, _ := a.partitionFoldForProjection(msgs[foldStart:foldEnd])
	if len(completedFold) == 0 || (!force && !foldEconomics(completedFold)) {
		return compactionFoldPlan{}, false
	}
	plan := compactionFoldPlan{
		kind:         compactionFoldActiveTurn,
		prefixEnd:    foldStart,
		summaryStart: active,
		foldEnd:      foldEnd,
	}
	return plan, plan.valid(msgs)
}

// completedActiveTurnPrefixEnd returns the largest prefix ending on a safe
// message boundary. It never crosses a later user-authored turn and never
// splits an assistant tool_calls record from its complete result group.
func completedActiveTurnPrefixEnd(msgs []provider.Message, start, limit int) int {
	if start < 0 {
		start = 0
	}
	if limit > len(msgs) {
		limit = len(msgs)
	}
	if start >= limit {
		return start
	}

	safe := start
	for i := start; i < limit; {
		m := msgs[i]
		if m.LocalOnly {
			i++
			safe = i
			continue
		}
		if m.Role == provider.RoleUser && !isCompactionSummary(m) {
			break
		}
		if m.Role == provider.RoleTool {
			break
		}
		if m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0 {
			j := i + 1
			for j < limit && msgs[j].Role == provider.RoleTool && !msgs[j].LocalOnly {
				j++
			}
			if !toolCallGroupComplete(m.ToolCalls, msgs[i+1:j]) {
				break
			}
			safe = j
			i = j
			continue
		}
		safe = i + 1
		i++
	}
	return safe
}

func toolCallGroupComplete(calls []provider.ToolCall, results []provider.Message) bool {
	if len(calls) == 0 {
		return true
	}
	if len(results) < len(calls) {
		return false
	}

	distinct := true
	seenCalls := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if call.ID == "" {
			distinct = false
			break
		}
		if _, exists := seenCalls[call.ID]; exists {
			distinct = false
			break
		}
		seenCalls[call.ID] = struct{}{}
	}
	if !distinct {
		return len(results) >= len(calls)
	}

	answered := make(map[string]struct{}, len(results))
	for _, result := range results {
		if result.Role == provider.RoleTool {
			answered[result.ToolCallID] = struct{}{}
		}
	}
	for _, call := range calls {
		if _, ok := answered[call.ID]; !ok {
			return false
		}
	}
	return true
}

func activeTurnCheckpointInstructions(instructions string) string {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return activeTurnCheckpointInstruction
	}
	return instructions + "\n\n" + activeTurnCheckpointInstruction
}

func formatActiveTurnCheckpoint(summary string) provider.Message {
	return provider.Message{
		Role: provider.RoleAssistant,
		Content: activeTurnCheckpointTagOpen + "\n" + strings.TrimSpace(summary) +
			"\n" + activeTurnCheckpointTagClose,
	}
}

func activeTurnCheckpointProjectionMessages(msgs []provider.Message, prefixEnd, foldEnd int, summary string) []provider.Message {
	proj := make([]provider.Message, 0, prefixEnd+2)
	proj = append(proj, msgs[:prefixEnd]...)
	proj = append(proj, formatActiveTurnCheckpoint(summary))
	// When the retained tail already starts with a user message it supplies the
	// continuation boundary. Otherwise add a synthetic user marker so the next
	// sampling request ends in a legal user→assistant/tool continuation shape.
	if foldEnd >= len(msgs) || msgs[foldEnd].Role != provider.RoleUser {
		proj = append(proj, provider.Message{Role: provider.RoleUser, Content: activeTurnContinuation})
	}
	return provider.ProjectionMessages(proj)
}
