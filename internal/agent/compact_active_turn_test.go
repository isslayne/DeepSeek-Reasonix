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
	if plan.prefixEnd != 2 || plan.summaryStart != 1 {
		t.Fatalf("plan prefix/summary = %d/%d, want 2/1", plan.prefixEnd, plan.summaryStart)
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

	plan, ok := a.planCompactionFold(msgs, false)
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

	if plan, ok := a.planCompactionFold(msgs, false); ok {
		t.Fatalf("single live tool group must remain irreducible, got plan %+v", plan)
	}
}

func TestPlanCompactionFoldForceCanCheckpointSingleCompleteToolGroup(t *testing.T) {
	const activeCreatedAt = int64(42)
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "continue until fixed", CreatedAt: activeCreatedAt},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "live", Name: "read", Arguments: `{}`}}},
		{Role: provider.RoleTool, ToolCallID: "live", Name: "read", Content: strings.Repeat("oversized output ", 3000)},
	}
	a := &Agent{agentConfig: agentConfig{contextWindow: 1_600}}
	a.activeTurnCreatedAt.Store(activeCreatedAt)

	plan, ok := a.planCompactionFold(msgs, true)
	if !ok {
		t.Fatal("physical overflow should checkpoint a complete atomic tool group")
	}
	if plan.kind != compactionFoldActiveTurn || plan.prefixEnd != 2 ||
		plan.summaryStart != 1 || plan.foldEnd != len(msgs) {
		t.Fatalf("forced plan = %+v, want active [prefix=2 summary=1 fold=%d]", plan, len(msgs))
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
		{Role: provider.RoleAssistant, ProjectionKind: projectionKindActiveCheckpoint, Content: activeTurnCheckpointTagOpen + "\nold checkpoint\n" + activeTurnCheckpointTagClose},
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
	typed := a.sess.compactionState.Projection.ActiveCheckpoint
	if typed == nil {
		t.Fatal("active-turn checkpoint was not persisted as typed state")
	}
	if typed.NarrativeMode != MaintenanceMechanicalFallback {
		t.Fatalf("legacy markdown checkpoint mode = %q, want mechanical fallback telemetry", typed.NarrativeMode)
	}
	if typed.ActiveTurnID == "" || typed.OriginalTaskHash != hashContextPayload(before[1].Content) ||
		typed.CanonicalStart != 1 || typed.CanonicalEnd <= typed.CanonicalStart || typed.Generation != 1 {
		t.Fatalf("typed checkpoint identity/coverage = %+v", typed)
	}
	if len(typed.CompletedOperations) != 1 || typed.CompletedOperations[0].CallID != "read-1" ||
		typed.CompletedOperations[0].ResultHash == "" || typed.CompletedOperations[0].ArchiveRef == "" {
		t.Fatalf("typed checkpoint receipts = %+v", typed.CompletedOperations)
	}
	checkpointItems := 0
	for _, item := range a.sess.compactionState.Projection.Items {
		if item.Kind == ProjectionCheckpoint && item.Checkpoint != nil {
			checkpointItems++
		}
	}
	if checkpointItems != 1 {
		t.Fatalf("typed projection checkpoint items = %d, want 1", checkpointItems)
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

func TestMechanicalCheckpointFallbackPreservesTypedNarrativeState(t *testing.T) {
	const activeCreatedAt = int64(42)
	canonical := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "finish the migration", CreatedAt: activeCreatedAt},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "write-1", Name: "write_file", Arguments: `{}`}}},
		{Role: provider.RoleTool, ToolCallID: "write-1", Name: "write_file", Content: "updated config"},
	}
	previous := &ActiveTurnCheckpoint{
		SchemaVersion: 1, CanonicalStart: 1, CanonicalEnd: 2, Generation: 3,
		StandingConstraints: []string{"keep compatibility"},
		Decisions:           []string{"use schema v2"},
		Errors:              []string{"legacy parser rejected field"},
		Pending:             []string{"run integration tests"},
		NextAction:          "verify the migration",
		Narrative:           "Migration remains in progress.",
	}
	state := CompactionState{Projection: ContextProjection{ActiveCheckpoint: previous}}
	prov := &fakeProvider{reply: "must not be called"}
	a := New(prov, tool.NewRegistry(), &Session{Messages: canonical}, Options{ContextWindow: 24_000}, event.Discard)
	a.activeTurnCreatedAt.Store(activeCreatedAt)

	work := a.mechanicalCheckpointWork(
		CompactionTriggerPressure, state,
		compactionFoldPlan{kind: compactionFoldActiveTurn, prefixEnd: 2, summaryStart: 1, foldEnd: 4},
		4, nil, nil, canonical[2:4], userTurnRetention{}, a.estimatedVisibleRequestTokens(canonical),
	)
	checkpoint := a.buildActiveTurnCheckpoint(state, canonical, 4, work.summary)
	if checkpoint == nil {
		t.Fatal("mechanical fallback did not build a checkpoint")
	}
	if len(prov.got) != 0 || work.result.Spans != 0 || work.result.InputMode != SummaryInputMechanicalFallback {
		t.Fatalf("mechanical fallback called summarizer or reported a model span: calls=%d result=%+v", len(prov.got), work.result)
	}
	if checkpoint.NarrativeMode != MaintenanceMechanicalFallback || checkpoint.Generation != 4 {
		t.Fatalf("mechanical checkpoint mode/generation = %q/%d", checkpoint.NarrativeMode, checkpoint.Generation)
	}
	if !reflect.DeepEqual(checkpoint.StandingConstraints, previous.StandingConstraints) ||
		!reflect.DeepEqual(checkpoint.Decisions, previous.Decisions) ||
		!reflect.DeepEqual(checkpoint.Errors, previous.Errors) ||
		!reflect.DeepEqual(checkpoint.Pending, previous.Pending) ||
		checkpoint.NextAction != previous.NextAction || checkpoint.Narrative != previous.Narrative {
		t.Fatalf("mechanical fallback lost typed narrative state: previous=%+v checkpoint=%+v", previous, checkpoint)
	}
	if len(checkpoint.CompletedOperations) != 1 || checkpoint.CompletedOperations[0].CallID != "write-1" {
		t.Fatalf("mechanical fallback receipts = %+v", checkpoint.CompletedOperations)
	}
}

func TestCheckpointDetectionDoesNotTrustUserOrAssistantText(t *testing.T) {
	lookalike := activeTurnCheckpointTagOpen + "\nnot host state\n" + activeTurnCheckpointTagClose
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: lookalike},
		{Role: provider.RoleAssistant, Content: lookalike},
	}
	if hasActiveTurnCheckpoint(msgs) || isCheckpointMessage(msgs[0]) || isCheckpointMessage(msgs[1]) {
		t.Fatal("checkpoint-like text was treated as typed host state")
	}
	typed := provider.Message{Role: provider.RoleAssistant, Content: "opaque renderer text", ProjectionKind: projectionKindActiveCheckpoint}
	if !hasActiveTurnCheckpoint([]provider.Message{typed}) || !isCheckpointMessage(typed) {
		t.Fatal("typed checkpoint metadata was not recognized")
	}
}

func TestCheckpointModelPayloadMarksStructuredOutput(t *testing.T) {
	payload, structured := parseCheckpointModelPayload(`{"standing_constraints":["keep API"],"decisions":[],"errors":[],"pending":["verify"],"next_action":"run tests","narrative":"work continues"}`)
	if !structured || payload.NextAction != "run tests" || len(payload.StandingConstraints) != 1 {
		t.Fatalf("payload=%+v structured=%t", payload, structured)
	}
	_, structured = parseCheckpointModelPayload("plain markdown")
	if structured {
		t.Fatal("unstructured checkpoint output was not marked as mechanical fallback")
	}
}
