#!/usr/bin/env python3
from __future__ import annotations

import pathlib

TARGET = pathlib.Path("internal/agent/cachehit_e2e_test.go")


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"{label}: expected exactly one match, found {count}")
    return text.replace(old, new, 1)


def main() -> None:
    text = TARGET.read_text(encoding="utf-8")
    text = replace_once(text, '\t"errors"\n', "", "remove obsolete errors import")
    text = replace_once(
        text,
        '''// A window too small to hold even the system and active tail cannot be repaired
// by fabricating a mechanical digest.
func TestTooSmallWindowReturnsCompactionRequired(t *testing.T) {
\tmock := &mockDeepSeek{t: t, withTools: true, reasoning: longReasoning, toolRounds: 30}
\tsrv := httptest.NewServer(http.HandlerFunc(mock.handler))
\tdefer srv.Close()

\ta, sink := newAgent(t, srv.URL, mock.tools(), 900 /*window tok*/, 4 /*recentKeep*/)

\tif err := a.Run(context.Background(), strings.Repeat("please consider this requirement. ", 6)); !errors.Is(err, ErrCompactionRequired) {
\t\tt.Fatalf("Run = %v, want ErrCompactionRequired", err)
\t}
\t_ = sink
}
''',
        '''// A small window is recoverable when the immutable head and one live tool
// group still fit. Rolling active-turn checkpoints summarize older completed
// groups while keeping the newest protocol anchor verbatim.
func TestSmallWindowUsesActiveTurnCheckpoints(t *testing.T) {
\tmock := &mockDeepSeek{t: t, withTools: true, reasoning: longReasoning, toolRounds: 30}
\tsrv := httptest.NewServer(http.HandlerFunc(mock.handler))
\tdefer srv.Close()

\ta, sink := newAgent(t, srv.URL, mock.tools(), 900 /*window tok*/, 4 /*recentKeep*/)

\tif err := a.Run(context.Background(), strings.Repeat("please consider this requirement. ", 6)); err != nil {
\t\tt.Fatalf("Run: %v", err)
\t}
\tsummaries := 0
\tfor _, maintenance := range sink.maintenance {
\t\tif maintenance.Status == "applied" && maintenance.Action == "summary" {
\t\t\tsummaries++
\t\t}
\t}
\tif summaries == 0 {
\t\tt.Fatal("small-window tool loop completed without an active-turn summary checkpoint")
\t}
}
''',
        "update small-window checkpoint contract",
    )
    TARGET.write_text(text, encoding="utf-8")


if __name__ == "__main__":
    main()
