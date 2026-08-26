#!/usr/bin/env python3
from __future__ import annotations

from pathlib import Path

COMPACT = Path("internal/agent/compact.go")


def main() -> None:
    text = COMPACT.read_text(encoding="utf-8")
    old = '''\tvar schemas []provider.ToolSchema
\tif a.svc.tools != nil {
\t\tschemas = a.svc.tools.Schemas()
\t}
'''
    new = '''\tvar schemas []provider.ToolSchema
\t// A rolling checkpoint is a bounded maintenance request, not an agent turn.
\t// Tool schemas can consume thousands of provider tokens and are unusable here
\t// because the compaction instruction explicitly forbids tool calls. Omitting
\t// them only for active-turn checkpoints makes the planner and sender share the
\t// same learned-window budget while ordinary history summaries retain their
\t// byte-stable cache-aligned request shape.
\tif a.svc.tools != nil && !strings.Contains(instructions, activeTurnCheckpointInstruction) {
\t\tschemas = a.svc.tools.Schemas()
\t}
'''
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"summary schema block: expected one match, found {count}")
    COMPACT.write_text(text.replace(old, new, 1), encoding="utf-8")


if __name__ == "__main__":
    main()
