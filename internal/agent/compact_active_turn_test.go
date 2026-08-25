package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func TestPlanCompactionFoldFallsBackToActiveTurnCheckpoint(t *testing.T) {
	const activeCreatedAt = int64(42)
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "inspect the APK", CreatedAt: activeCreatedAt},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "read-1", Name: "read", Arguments: `{}`}}},
		{Role: provider.RoleTool, ToolCallID: "read-1", Name: "read", Content: strings.Repeat("tool output ", 4000)},
		{Role: provider.RoleAssistant, Content: strings.Repeat("analysis ", 2000)},
		{Role: provider.RoleAssistant, Content: "recent live tail"},
	}
	a := &Agent{agentConfig: agentConfig{contextWindow: 50_000}}
	a.activeTurnCreatedAt.Store(activeCreatedAt)

	plan, ok := a.planCompactionFold(msgs, false)
	if !ok {
		t.Fatal("expected a completed active-turn prefix to be foldable")
	}
	if plan.kind != compactionFoldActiveTurn {
		t.Fatalf("plan kind = %v, want active-turn checkpoint", plan.kind)
	}
	if plan.prefixEnd != 2 || plan.summaryStart != 0 {
		t.Fatalf("plan prefix/summary = %d/%d, want 2/0", plan.prefixEnd, plan.summaryStart)
	}
	if plan.foldEnd <= plan.prefixEnd || plan.foldEnd >= len(msgs) {
		t.Fatalf("fold end = %d, want completed work with a recent tail retained", plan.foldEnd)
	}
}

func TestPlanCompactionFoldRetainsNewestToolGroup(t *testing.T) {
	const activeCreatedAt = int64(42)
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "continue until fixed", CreatedAt: activeCreatedAt},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "old", Name: "read", Arguments: `{}`}}},
		{Role: provider.RoleTool, ToolCallID: "old", Name: "read", Content: strings.Repeat("old output ", 3000)},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "live", Name: "grep", Arguments: `{}`}}},
		{Role: provider.RoleTool, ToolCallID: "live", Name: "grep", Content: strings.Repeat("latest output ", 3000)},
	}
	a := &Agent{agentConfig: agentConfig{contextWindow: 8_000}}
	a.activeTurnCreatedAt.Store(activeCreatedAt)

	plan, ok := a.planCompactionFold(msgs, true)
	if !ok {
		t.Fatal("expected the older completed tool group to be foldable")
	}
	if plan.kind != compactionFoldActiveTurn {
		t.Fatalf("plan kind = %v, want active-turn checkpoint", plan.kind)
	}
	if plan.foldEnd != 4 {
		t.Fatalf("fold end = %d, want newest tool group retained from index 4", plan.foldEnd)
	}
	if msgs[plan.foldEnd].Role != provider.RoleAssistant || len(msgs[plan.foldEnd].ToolCalls) == 0 {
		t.Fatalf("retained tail does not begin with the newest assistant tool call: %+v", msgs[plan.foldEnd])
	}
}

func TestPlanCompactionFoldRejectsSingleIrreducibleToolGroup(t *testing.T) {
	const activeCreatedAt = int64(42)
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "continue until fixed", CreatedAt: activeCreatedAt},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "live", Name: "read", Arguments: `{}`}}},
		{Role: provider.RoleTool, ToolCallID: "live", Name: "read", Content: strings.Repeat("oversized output ", 3000)},
	}
	a := &Agent{agentConfig: agentConfig{contextWindow: 1_600}}
	a.activeTurnCreatedAt.Store(activeCreatedAt)

	if plan, ok := a.planCompactionFold(msgs, true); ok {
		t.Fatalf("single live tool group must remain irreducible, got plan %+v", plan)
	}
}

func TestPlanCompactionFoldPrefersCompletedHistory(t *testing.T) {
	const activeCreatedAt = int64(42)
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "older task"},
		{Role: provider.RoleAssistant, Content: strings.Repeat("older work ", 4000)},
		{Role: provider.RoleUser, Content: "current task", CreatedAt: activeCreatedAt},
		{Role: provider.RoleAssistant, Content: strings.Repeat("current work ", 4000)},
		{Role: provider.RoleAssistant, Content: "recent live tail"},
	}
	a := &Agent{agentConfig: agentConfig{contextWindow: 50_000}}
	a.activeTurnCreatedAt.Store(activeCreatedAt)

	plan, ok := a.planCompactionFold(msgs, false)
	if !ok {
		t.Fatal("expected historical prefix to be foldable")
	}
	if plan.kind != compactionFoldHistory {
		t.Fatalf("plan kind = %v, want historical fold first", plan.kind)
	}
	if plan.prefixEnd != 1 || plan.foldEnd != 3 {
		t.Fatalf("history plan = [%d:%d], want [1:3]", plan.prefixEnd, plan.foldEnd)
	}
}

func TestCompletedActiveTurnPrefixEndKeepsToolGroupsAtomic(t *testing.T) {
	calls := []provider.ToolCall{
		{ID: "a", Name: "read", Arguments: `{}`},
		{ID: "b", Name: "grep", Arguments: `{}`},
	}
	base := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "task", CreatedAt: 42},
		{Role: provider.RoleAssistant, ToolCalls: calls},
	}

	incomplete := append([]provider.Message(nil), base...)
	incomplete = append(incomplete, provider.Message{Role: provider.RoleTool, ToolCallID: "a", Name: "read", Content: "one result"})
	if got := completedActiveTurnPrefixEnd(incomplete, 2, len(incomplete)); got != 2 {
		t.Fatalf("incomplete group end = %d, want 2", got)
	}

	complete := append([]provider.Message(nil), incomplete...)
	complete = append(complete,
		provider.Message{Role: provider.RoleTool, ToolCallID: "b", Name: "grep", Content: "second result"},
		provider.Message{Role: provider.RoleAssistant, Content: "completed analysis"},
	)
	if got := completedActiveTurnPrefixEnd(complete, 2, len(complete)); got != len(complete) {
		t.Fatalf("complete group end = %d, want %d", got, len(complete))
	}

	withSteer := append(complete, provider.Message{Role: provider.RoleUser, Content: "change direction", CreatedAt: 43})
	if got := completedActiveTurnPrefixEnd(withSteer, 2, len(withSteer)); got != len(complete) {
		t.Fatalf("fold crossed later user turn: end = %d, want %d", got, len(complete))
	}
}

func TestActiveTurnCheckpointProjectionRollsPriorCheckpointForward(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "original task", CreatedAt: 42},
		{Role: provider.RoleAssistant, Content: activeTurnCheckpointTagOpen + "\nold checkpoint\n" + activeTurnCheckpointTagClose},
		{Role: provider.RoleUser, Content: activeTurnContinuation},
		{Role: provider.RoleAssistant, Content: "completed after the old checkpoint"},
		{Role: provider.RoleAssistant, Content: "new live tail"},
	}
	projection := activeTurnCheckpointProjectionMessages(msgs, 2, 5, "new checkpoint")
	var checkpoints, continuations int
	for _, msg := range projection {
		if strings.HasPrefix(msg.Content, activeTurnCheckpointTagOpen) {
			checkpoints++
			if strings.Contains(msg.Content, "old checkpoint") {
				t.Fatal("prior active-turn checkpoint accumulated instead of rolling forward")
			}
		}
		if msg.Content == activeTurnContinuation {
			continuations++
		}
	}
	if checkpoints != 1 || continuations != 1 {
		t.Fatalf("checkpoint/continuation = %d/%d, want 1/1: %+v", checkpoints, continuations, projection)
	}
}

func TestActiveTurnCheckpointCompactionPreservesTaskAndCanonicalTranscript(t *testing.T) {
	const activeCreatedAt = int64(42)
	largeToolResult := strings.Repeat("tool-result ", 4000)
	largeAnalysis := strings.Repeat("analysis ", 2500)
	recentTail := "recent live tail must stay verbatim"
	sess := &Session{Messages: []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "inspect the APK and continue until fixed", CreatedAt: activeCreatedAt},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "read-1", Name: "read", Arguments: `{}`}}},
		{Role: provider.RoleTool, ToolCallID: "read-1", Name: "read", Content: largeToolResult},
		{Role: provider.RoleAssistant, Content: largeAnalysis},
		{Role: provider.RoleAssistant, Content: recentTail},
	}}
	before := sess.Snapshot()
	prov := &fakeProvider{reply: "## Goal\nKeep inspecting the APK.\n\n## Pending & next step\nContinue from the retained tail."}
	a := New(prov, tool.NewRegistry(), sess, Options{ContextWindow: 50_000}, event.Discard)
	a.activeTurnCreatedAt.Store(activeCreatedAt)

	outcome, err := a.compactToProjection(context.Background(), CompactionTriggerPressure, "", false, false)
	if err != nil {
		t.Fatalf("active-turn checkpoint: %v", err)
	}
	if outcome != CompactionInstalled {
		t.Fatalf("outcome = %v, want CompactionInstalled", outcome)
	}
	if got := sess.Snapshot(); !reflect.DeepEqual(got, before) {
		t.Fatal("active-turn checkpoint rewrote the canonical transcript")
	}

	visible := visibleContext(a)
	var originalTask, checkpoints, continuations int
	for _, msg := range visible {
		switch {
		case msg.Role == provider.RoleUser && msg.CreatedAt == activeCreatedAt && msg.Content == before[1].Content:
			originalTask++
		case strings.HasPrefix(msg.Content, activeTurnCheckpointTagOpen):
			checkpoints++
		case msg.Content == activeTurnContinuation:
			continuations++
		}
		if strings.Contains(msg.Content, largeToolResult) || strings.Contains(msg.Content, largeAnalysis) {
			t.Fatalf("completed active-turn bulk survived verbatim in projection: role=%s", msg.Role)
		}
	}
	if originalTask != 1 || checkpoints != 1 || continuations != 1 {
		t.Fatalf("projection task/checkpoint/continuation = %d/%d/%d, want 1/1/1: %+v", originalTask, checkpoints, continuations, visible)
	}
	if got := visible[len(visible)-1].Content; got != recentTail {
		t.Fatalf("recent tail = %q, want %q", got, recentTail)
	}
	if len(prov.got) == 0 || !strings.Contains(renderTranscript(prov.got), before[1].Content) {
		t.Fatal("summary request did not include the original active task")
	}
}
