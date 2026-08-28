package agent

import (
	"context"
	"testing"

	"reasonix/internal/provider"
)

type overflowSummaryProvider struct {
	requests []provider.Request
}

func (p *overflowSummaryProvider) Name() string { return "overflow-summary" }

func (p *overflowSummaryProvider) ContextBudgetPolicy() provider.ContextBudgetPolicy {
	return provider.ContextBudgetPolicy{
		WindowMode: provider.ContextWindowShared,
		LimitMode:  provider.OutputLimitAlways,
	}
}

func (p *overflowSummaryProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	p.requests = append(p.requests, req)
	return chunks(
		provider.Chunk{Type: provider.ChunkText, Text: "compact durable summary"},
		provider.Chunk{Type: provider.ChunkDone},
	), nil
}

func TestOverflowSummarizesLargestAdmissibleContiguousPrefix(t *testing.T) {
	sess := foldableSessionOverForce(140)
	prov := &overflowSummaryProvider{}
	a := agentOverForceWindow(t, prov, sess, 60_000)
	msgs := sess.Snapshot()
	head, plannedEnd, ok := a.planFoldRegion(msgs, true)
	if !ok {
		t.Fatal("fixture has no foldable prefix")
	}
	safeEnd := a.maximumSafeSummaryPrefixEnd(msgs, head, plannedEnd, "")
	if safeEnd <= head || safeEnd >= plannedEnd {
		t.Fatalf("safe fold end = %d, want a non-empty prefix smaller than planned end %d", safeEnd, plannedEnd)
	}

	if err := prepareContext(context.Background(), a, CompactionTriggerOverflow); err != nil {
		t.Fatalf("overflow recovery: %v", err)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("summary requests = %d, want 1", len(prov.requests))
	}
	req := prov.requests[0]
	budget := a.requestBudget(req, req.MaxTokens, 256)
	if budget.EffectiveOutput < budget.MinimumOutput {
		t.Fatalf("summary request budget = %+v, want an admissible minimum output", budget)
	}
	if receipt := a.sess.compactionState.LastReceipt; receipt == nil || receipt.CoveredCount != safeEnd {
		t.Fatalf("receipt = %+v, want covered prefix %d", receipt, safeEnd)
	}
	if current := a.contextManager().currentPrepared(); current.InputTokens >= a.hardInputCeiling() {
		t.Fatalf("recovered projection tokens = %d, hard ceiling = %d", current.InputTokens, a.hardInputCeiling())
	}
}

func TestMaximumSafeSummaryPrefixKeepsToolPairsTogether(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "old task"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "call-1", Name: "read", Arguments: `{}`}}},
		{Role: provider.RoleTool, ToolCallID: "call-1", Name: "read", Content: "first result"},
		{Role: provider.RoleTool, ToolCallID: "call-1", Name: "read", Content: "second result"},
		{Role: provider.RoleUser, Content: "recent task"},
	}
	a := &Agent{
		agentConfig: agentConfig{contextWindow: 100_000},
		svc:         agentServices{prov: &overflowSummaryProvider{}},
		sess:        sessionRuntime{conversation: &Session{Messages: msgs}},
	}
	promptThroughUser := a.estimatedRequestTokens(a.summaryRequest(msgs[1:2], ""))
	base := promptThroughUser + 256
	a.contextWindow = base + protocolMarginForWindow(base) + 1
	end := a.maximumSafeSummaryPrefixEnd(msgs, 1, len(msgs)-1, "")
	if end != 2 {
		t.Fatalf("fold boundary = %d, want 2 so the assistant call and both results stay in the tail", end)
	}
}
