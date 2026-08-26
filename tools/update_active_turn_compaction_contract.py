#!/usr/bin/env python3
from __future__ import annotations

import pathlib

ACTIVE = pathlib.Path("internal/agent/compact_active_turn_test.go")
CACHEHIT = pathlib.Path("internal/agent/cachehit_e2e_test.go")
RECOVERY = pathlib.Path("internal/agent/context_recovery_tool_loop_test.go")


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"{label}: expected exactly one match, found {count}")
    return text.replace(old, new, 1)


def update_active_turn() -> None:
    text = ACTIVE.read_text(encoding="utf-8")
    text = replace_once(
        text,
        '''\tif plan.prefixEnd != 2 || plan.summaryStart != 0 {
\t\tt.Fatalf("plan prefix/summary = %d/%d, want 2/0", plan.prefixEnd, plan.summaryStart)
\t}
''',
        '''\tif plan.prefixEnd != 2 || plan.summaryStart != 1 {
\t\tt.Fatalf("plan prefix/summary = %d/%d, want 2/1", plan.prefixEnd, plan.summaryStart)
\t}
''',
        "align bounded active-turn summary start",
    )
    ACTIVE.write_text(text, encoding="utf-8")


def update_cachehit() -> None:
    text = CACHEHIT.read_text(encoding="utf-8")
    text = replace_once(text, '\t"errors"\n', "", "remove obsolete errors import")
    text = replace_once(
        text,
        '''\tif isSummarizeRequest(body) {
\t\tmsgs := decodeMessages(body)
\t\treplayed := msgs[:len(msgs)-1]
\t\tif len(m.prevMessages) > 0 && commonPrefixMsgs(m.prevMessages, replayed) != len(replayed) {
\t\t\tm.t.Errorf("summary request did not replay a byte-identical cached conversation prefix")
\t\t}
\t\twriteSSE(w, m.t,
''',
        '''\tif isSummarizeRequest(body) {
\t\tmsgs := decodeMessages(body)
\t\treplayed := msgs[:len(msgs)-1]
\t\tvar final struct {
\t\t\tContent string `json:"content"`
\t\t}
\t\tif err := json.Unmarshal(msgs[len(msgs)-1], &final); err != nil {
\t\t\tm.t.Errorf("decode summary instruction: %v", err)
\t\t}
\t\tactiveCheckpoint := strings.Contains(final.Content, activeTurnCheckpointInstruction)
\t\tif len(m.prevMessages) > 0 {
\t\t\tif activeCheckpoint {
\t\t\t\t// Emergency active-turn recovery intentionally trades one cache reset
\t\t\t\t// for a bounded request. Its immutable semantic prefix is the system
\t\t\t\t// message plus the original active user task, both byte-identical.
\t\t\t\tif len(m.prevMessages) < 2 || len(replayed) < 2 ||
\t\t\t\t\t!bytes.Equal(m.prevMessages[0], replayed[0]) ||
\t\t\t\t\t!bytes.Equal(m.prevMessages[1], replayed[1]) {
\t\t\t\t\tm.t.Errorf("active-turn summary did not retain the byte-identical system/task anchor")
\t\t\t\t}
\t\t\t} else if commonPrefixMsgs(m.prevMessages, replayed) != len(replayed) {
\t\t\t\tm.t.Errorf("history summary request did not replay a byte-identical cached conversation prefix")
\t\t\t}
\t\t}
\t\twriteSSE(w, m.t,
''',
        "split history-cache and active-turn summary contracts",
    )
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
// groups while keeping exactly one current checkpoint and semantic task anchor.
func TestSmallWindowUsesActiveTurnCheckpoints(t *testing.T) {
\tmock := &mockDeepSeek{t: t, withTools: true, reasoning: longReasoning, toolRounds: 30}
\tsrv := httptest.NewServer(http.HandlerFunc(mock.handler))
\tdefer srv.Close()

\ta, sink := newAgent(t, srv.URL, mock.tools(), 900 /*window tok*/, 4 /*recentKeep*/)
\trequest := strings.Repeat("please consider this requirement. ", 6)

\tif err := a.Run(context.Background(), request); err != nil {
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

\tvar originalTask, checkpoints, continuations int
\tfor _, msg := range visibleContext(a) {
\t\tswitch {
\t\tcase msg.Role == provider.RoleUser && msg.Content == request:
\t\t\toriginalTask++
\t\tcase strings.HasPrefix(strings.TrimSpace(msg.Content), activeTurnCheckpointTagOpen):
\t\t\tcheckpoints++
\t\tcase msg.Content == activeTurnContinuation:
\t\t\tcontinuations++
\t\t}
\t}
\tif originalTask != 1 || checkpoints != 1 || continuations != 1 {
\t\tt.Fatalf("visible task/checkpoint/continuation = %d/%d/%d, want 1/1/1",
\t\t\toriginalTask, checkpoints, continuations)
\t}
}
''',
        "update small-window checkpoint contract",
    )
    CACHEHIT.write_text(text, encoding="utf-8")


def update_recovery() -> None:
    text = RECOVERY.read_text(encoding="utf-8")
    text = replace_once(
        text,
        '''\terr := a.Run(context.Background(), "keep using the tool until the provider says the task is done")
\tif err == nil {
\t\tt.Fatal("overflow without new projection progress unexpectedly retried")
\t}

\tprov.mu.Lock()
\tdefer prov.mu.Unlock()
\tif prov.overflows != 2 {
\t\tt.Fatalf("provider overflows = %d, want 2", prov.overflows)
\t}
\tif prov.requestsAt[3] != 2 || prov.requestsAt[6] != 1 {
\t\tt.Fatalf("requests at overflow points = %v, want one retry after progress and none without progress", prov.requestsAt)
\t}
\tif prov.summaries > 1 || applied > 1 {
\t\tt.Fatalf("summaries=%d applied=%d, want no repeated summary without new projection input", prov.summaries, applied)
\t}
''',
        '''\tif err := a.Run(context.Background(), "keep using the tool until the provider says the task is done"); err != nil {
\t\tt.Fatalf("Run: %v", err)
\t}

\tprov.mu.Lock()
\tdefer prov.mu.Unlock()
\tif prov.overflows != 2 {
\t\tt.Fatalf("provider overflows = %d, want 2", prov.overflows)
\t}
\tif prov.requestsAt[3] != 2 || prov.requestsAt[6] != 2 {
\t\tt.Fatalf("requests at overflow points = %v, want one retry after each checkpoint makes progress", prov.requestsAt)
\t}
\tif prov.summaries < 2 || applied < 2 {
\t\tt.Fatalf("summaries=%d applied=%d, want rolling checkpoints to recover both overflows", prov.summaries, applied)
\t}
''',
        "update repeated-overflow recovery contract",
    )
    RECOVERY.write_text(text, encoding="utf-8")


def main() -> None:
    update_active_turn()
    update_cachehit()
    update_recovery()


if __name__ == "__main__":
    main()
