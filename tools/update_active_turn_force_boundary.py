#!/usr/bin/env python3
from __future__ import annotations

from pathlib import Path

ACTIVE = Path("internal/agent/compact_active_turn.go")
TESTS = Path("internal/agent/compact_active_turn_test.go")


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"{label}: expected exactly one match, found {count}")
    return text.replace(old, new, 1)


def main() -> None:
    text = ACTIVE.read_text(encoding="utf-8")
    text = replace_once(
        text,
        "\tfoldEnd := completedActiveTurnFoldEnd(msgs, foldStart, plannedEnd)\n",
        """\tfoldEnd := completedActiveTurnFoldEnd(msgs, foldStart, plannedEnd)
\tif force {
\t\t// Physical-overflow recovery must maximize reclaimed context. Once a
\t\t// tool-call/result group is complete it is safe to checkpoint; retaining
\t\t// the newest complete group can itself consume the learned provider
\t\t// window and make the retry impossible. Normal pressure compaction still
\t\t// keeps that newest group verbatim for cache/protocol locality.
\t\tfoldEnd = completedActiveTurnPrefixEnd(msgs, foldStart, plannedEnd)
\t}
""",
        "active-turn force boundary",
    )
    ACTIVE.write_text(text, encoding="utf-8")

    text = TESTS.read_text(encoding="utf-8")

    start = text.index("func TestPlanCompactionFoldRetainsNewestToolGroup")
    end = text.index("func TestPlanCompactionFoldRejectsSingleIrreducibleToolGroup", start)
    region = text[start:end]
    region = replace_once(
        region,
        "plan, ok := a.planCompactionFold(msgs, true)",
        "plan, ok := a.planCompactionFold(msgs, false)",
        "retain-newest pressure contract",
    )
    text = text[:start] + region + text[end:]

    start = text.index("func TestPlanCompactionFoldRejectsSingleIrreducibleToolGroup")
    end = text.index("func TestPlanCompactionFoldPrefersCompletedHistory", start)
    region = text[start:end]
    region = replace_once(
        region,
        "a.planCompactionFold(msgs, true)",
        "a.planCompactionFold(msgs, false)",
        "single-group pressure contract",
    )
    force_test = r"""func TestPlanCompactionFoldForceCanCheckpointSingleCompleteToolGroup(t *testing.T) {
\tconst activeCreatedAt = int64(42)
\tmsgs := []provider.Message{
\t\t{Role: provider.RoleSystem, Content: "system"},
\t\t{Role: provider.RoleUser, Content: "continue until fixed", CreatedAt: activeCreatedAt},
\t\t{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "live", Name: "read", Arguments: `{}`}}},
\t\t{Role: provider.RoleTool, ToolCallID: "live", Name: "read", Content: strings.Repeat("oversized output ", 3000)},
\t}
\ta := &Agent{agentConfig: agentConfig{contextWindow: 1_600}}
\ta.activeTurnCreatedAt.Store(activeCreatedAt)

\tplan, ok := a.planCompactionFold(msgs, true)
\tif !ok {
\t\tt.Fatal("physical overflow should checkpoint a complete atomic tool group")
\t}
\tif plan.kind != compactionFoldActiveTurn || plan.prefixEnd != 2 ||
\t\tplan.summaryStart != 1 || plan.foldEnd != len(msgs) {
\t\tt.Fatalf("forced plan = %+v, want active [prefix=2 summary=1 fold=%d]", plan, len(msgs))
\t}
}

"""
    text = text[:start] + region + force_test + text[end:]
    TESTS.write_text(text, encoding="utf-8")


if __name__ == "__main__":
    main()
