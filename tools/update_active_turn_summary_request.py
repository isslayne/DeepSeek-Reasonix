#!/usr/bin/env python3
from __future__ import annotations

from pathlib import Path

COMPACT = Path("internal/agent/compact.go")
PROJECTION = Path("internal/agent/compact_projection.go")


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"{label}: expected exactly one match, found {count}")
    return text.replace(old, new, 1)


def update_compact() -> None:
    text = COMPACT.read_text(encoding="utf-8")
    text = replace_once(
        text,
        "\tsummaryOutputMaxTokens = 8192 // max digest output; further clipped by remaining candidate space\n",
        """\tsummaryOutputMaxTokens    = 8192 // max digest output; further clipped by remaining candidate space
\tactiveTurnSummaryMaxTokens = 2048 // bounded emergency checkpoint output; ordinary summaries keep 8K
""",
        "active-turn summary output budget",
    )
    text = replace_once(
        text,
        '''\tvar schemas []provider.ToolSchema
\tif a.svc.tools != nil {
\t\tschemas = a.svc.tools.Schemas()
\t}
\treturn provider.Request{
\t\tMessages:    messages,
\t\tTools:       schemas,
\t\tMaxTokens:   summaryOutputMaxTokens,
\t\tTemperature: provider.OptionalTemperature(a.temperature),
\t}
''',
        '''\tactiveCheckpoint := isActiveTurnCheckpointSummary(instructions)
\tvar schemas []provider.ToolSchema
\t// A rolling checkpoint is a bounded maintenance request, not an agent turn.
\t// Tool schemas can consume thousands of provider tokens and are unusable here
\t// because the compaction instruction explicitly forbids tool calls. Omitting
\t// them only for active-turn checkpoints makes the planner and sender share the
\t// same learned-window budget while ordinary history summaries retain their
\t// byte-stable cache-aligned request shape.
\tif a.svc.tools != nil && !activeCheckpoint {
\t\tschemas = a.svc.tools.Schemas()
\t}
\tmaxTokens := summaryOutputMaxTokens
\tif activeCheckpoint {
\t\tmaxTokens = activeTurnSummaryMaxTokens
\t}
\treturn provider.Request{
\t\tMessages:    messages,
\t\tTools:       schemas,
\t\tMaxTokens:   maxTokens,
\t\tTemperature: provider.OptionalTemperature(a.temperature),
\t}
''',
        "bounded active-turn summary request shape",
    )
    text = replace_once(
        text,
        '''// summarize asks the executor's own provider to distill a replayed prefix into
// a briefing. instructions is optional /compact focus + PreCompact text.
// Named returns so defer can attach RequestCount and still return usage.
func (a *Agent) summarize(ctx context.Context, region []provider.Message, instructions string) (summary string, usage *provider.Usage, err error) {
''',
        '''func isActiveTurnCheckpointSummary(instructions string) bool {
\treturn strings.Contains(instructions, activeTurnCheckpointInstruction)
}

// applyActiveTurnSummaryAdmission gives the emergency maintenance request its
// own bounded output envelope. Generic sampling admission always reserves 8K
// tokens for a user-facing answer; applying that reserve to a 1-2K checkpoint
// can make the summary request itself impossible inside a learned 24K window.
// Planning uses the same activeTurnSummaryMaxTokens + protocol reserve below.
func (a *Agent) applyActiveTurnSummaryAdmission(req *provider.Request) error {
\tif a == nil || req == nil {
\t\treturn nil
\t}
\twindow := a.effectiveContextWindow()
\tif window <= 0 {
\t\treturn nil
\t}
\tprompt := a.estimatedRequestTokens(*req)
\tavailable := window - prompt - protocolReserveTokens
\tif available < 256 {
\t\treturn fmt.Errorf("%w: estimated active-turn summary prompt %d leaves no checkpoint output budget", ErrCompactionRequired, prompt)
\t}
\tmaxTokens := req.MaxTokens
\tif maxTokens <= 0 || maxTokens > available {
\t\tmaxTokens = available
\t}
\tpolicy := contextBudgetPolicyOf(a.svc.prov)
\tif policy.MaxOutputTokens > 0 && maxTokens > policy.MaxOutputTokens {
\t\tmaxTokens = policy.MaxOutputTokens
\t}
\tif learned := a.learnedCompletionBudget(); learned > 0 && maxTokens > learned {
\t\tmaxTokens = learned
\t}
\tif maxTokens < 256 {
\t\treturn fmt.Errorf("summary output budget too small (%d tokens)", maxTokens)
\t}
\treq.MaxTokens = maxTokens
\treturn nil
}

// summarize asks the executor's own provider to distill a replayed prefix into
// a briefing. instructions is optional /compact focus + PreCompact text.
// Named returns so defer can attach RequestCount and still return usage.
func (a *Agent) summarize(ctx context.Context, region []provider.Message, instructions string) (summary string, usage *provider.Usage, err error) {
''',
        "active-turn summary admission helper",
    )
    text = replace_once(
        text,
        '''\treq := a.summaryRequest(region, instructions)
\tif err := a.applyAdmissionToRequest(&req); err != nil {
\t\treturn "", usage, err
\t}
''',
        '''\treq := a.summaryRequest(region, instructions)
\tif isActiveTurnCheckpointSummary(instructions) {
\t\terr = a.applyActiveTurnSummaryAdmission(&req)
\t} else {
\t\terr = a.applyAdmissionToRequest(&req)
\t}
\tif err != nil {
\t\treturn "", usage, err
\t}
''',
        "route active-turn summary admission",
    )
    COMPACT.write_text(text, encoding="utf-8")


def update_projection() -> None:
    text = PROJECTION.read_text(encoding="utf-8")
    text = replace_once(
        text,
        '''\tmaxPromptTokens := a.hardInputCeiling()
\tif policy.WindowMode == provider.ContextWindowShared {
\t\tmaxPromptTokens = window - outputBudgetReserve - 256
\t}
''',
        '''\tmaxPromptTokens := a.hardInputCeiling()
\tif policy.WindowMode == provider.ContextWindowShared {
\t\treserve := outputBudgetReserve
\t\tif isActiveTurnCheckpointSummary(instructions) {
\t\t\treserve = activeTurnSummaryMaxTokens
\t\t}
\t\tmaxPromptTokens = window - reserve - protocolReserveTokens
\t}
''',
        "align active-turn planner with summary admission",
    )
    PROJECTION.write_text(text, encoding="utf-8")


def main() -> None:
    update_compact()
    update_projection()


if __name__ == "__main__":
    main()
