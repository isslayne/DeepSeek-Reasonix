package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

type overflowLoopTool struct{ output string }

func (overflowLoopTool) Name() string            { return "grow_context" }
func (overflowLoopTool) Description() string     { return "Return a large deterministic result." }
func (overflowLoopTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (overflowLoopTool) ReadOnly() bool          { return true }
func (t overflowLoopTool) Execute(context.Context, json.RawMessage) (string, error) {
	return t.output, nil
}

// repeatedOverflowProvider forces independent context-limit recoveries every
// third tool call. Summary requests never advance the tool loop, so the test
// exercises the production recovery/compaction wiring.
type repeatedOverflowProvider struct {
	mu           sync.Mutex
	toolCalls    int
	overflows    int
	summaries    int
	rejectedAt   map[int]bool
	requestsAt   map[int]int
	maxToolCalls int
}

func (p *repeatedOverflowProvider) Name() string { return "repeated-overflow" }
func (p *repeatedOverflowProvider) ContextBudgetPolicy() provider.ContextBudgetPolicy {
	return provider.ContextBudgetPolicy{
		WindowMode:       provider.ContextWindowShared,
		AutoOutputTokens: 1024,
		MaxOutputTokens:  1024,
		LimitMode:        provider.OutputLimitAlways,
	}
}

func (p *repeatedOverflowProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(req.Messages) > 0 && strings.Contains(req.Messages[len(req.Messages)-1].Content, "Compact the preceding conversation prefix") {
		p.summaries++
		return chunks(
			provider.Chunk{Type: provider.ChunkText, Text: "- goal: finish the tool loop\n- pending: continue"},
			provider.Chunk{Type: provider.ChunkDone},
		), nil
	}
	p.requestsAt[p.toolCalls]++

	if p.toolCalls > 0 && p.toolCalls%3 == 0 && !p.rejectedAt[p.toolCalls] {
		p.rejectedAt[p.toolCalls] = true
		p.overflows++
		return nil, &provider.ContextLimitError{
			APIError:         &provider.APIError{Provider: p.Name(), Status: 400, Body: "context limit exceeded"},
			WindowTokens:     24_000,
			PromptTokens:     23_000,
			CompletionTokens: 2_000,
			RequestedTokens:  25_000,
		}
	}

	if p.toolCalls >= p.maxToolCalls {
		return chunks(
			provider.Chunk{Type: provider.ChunkText, Text: "Done."},
			provider.Chunk{Type: provider.ChunkDone},
		), nil
	}

	p.toolCalls++
	return chunks(
		provider.Chunk{Type: provider.ChunkToolCall, ToolCall: &provider.ToolCall{
			ID: fmt.Sprintf("grow-%d", p.toolCalls), Name: "grow_context", Arguments: `{}`,
		}},
		provider.Chunk{Type: provider.ChunkDone},
	), nil
}

func chunks(items ...provider.Chunk) <-chan provider.Chunk {
	ch := make(chan provider.Chunk, len(items))
	for _, item := range items {
		ch <- item
	}
	close(ch)
	return ch
}

func TestToolLoopRetriesOnlyAfterOverflowMaintenanceProgress(t *testing.T) {
	testRepeatedOverflowRecovery(t, 2)
}

func TestActiveCheckpointRecoversFiveOverflows(t *testing.T) {
	testRepeatedOverflowRecovery(t, 5)
}

func testRepeatedOverflowRecovery(t *testing.T, overflowCount int) {
	t.Helper()
	prov := &repeatedOverflowProvider{
		rejectedAt: make(map[int]bool), requestsAt: make(map[int]int), maxToolCalls: overflowCount * 3,
	}
	reg := tool.NewRegistry()
	reg.Add(overflowLoopTool{output: strings.Repeat("large deterministic tool output. ", 700)})

	applied := 0
	var projectionGenerations, cacheGenerations []uint64
	var maintenanceModes []string
	sink := event.FuncSink(func(e event.Event) {
		if e.Kind == event.ContextMaintenanceEvent && e.Maintenance != nil &&
			e.Maintenance.Status == "applied" && e.Maintenance.Action == "summary" {
			applied++
			projectionGenerations = append(projectionGenerations, e.Maintenance.ProjectionGeneration)
			cacheGenerations = append(cacheGenerations, e.Maintenance.CacheGeneration)
			maintenanceModes = append(maintenanceModes, e.Maintenance.Mode)
		}
	})
	a := New(prov, reg, NewSession("system"), Options{
		ContextWindow:   100_000,
		CompactRatio:    defaultCompactRatio,
		MaxOutputTokens: 1024,
	}, sink)

	err := a.Run(context.Background(), "keep using the tool until the provider says the task is done")
	if err != nil {
		prov.mu.Lock()
		overflows, toolCalls, summaries := prov.overflows, prov.toolCalls, prov.summaries
		prov.mu.Unlock()
		projection := a.sess.compactionState.Projection
		t.Fatalf("rolling checkpoint recovery after %d overflows, %d tool calls, %d summaries: %v; version=%d covered=%d",
			overflows, toolCalls, summaries, err, projection.ProjectionVersion, projection.CoveredCount)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if prov.overflows != overflowCount {
		t.Fatalf("provider overflows = %d, want %d", prov.overflows, overflowCount)
	}
	for overflow := 1; overflow <= overflowCount; overflow++ {
		if point := overflow * 3; prov.requestsAt[point] != 2 {
			t.Fatalf("requests at overflow points = %v, want two at %d", prov.requestsAt, point)
		}
	}
	if applied < overflowCount || prov.summaries > applied {
		t.Fatalf("summaries=%d applied=%d, want at least one progressing checkpoint per overflow", prov.summaries, applied)
	}
	if overflowCount >= 5 && prov.summaries >= applied {
		t.Fatalf("five-overflow replay never exercised no-call mechanical fallback: summaries=%d applied=%d", prov.summaries, applied)
	}
	if !strictlyIncreasing(projectionGenerations, applied) || !strictlyIncreasing(cacheGenerations, applied) {
		t.Fatalf("projection/cache generations did not advance monotonically: %v/%v", projectionGenerations, cacheGenerations)
	}
	wantModes := make([]string, applied)
	for i := range wantModes {
		wantModes[i] = MaintenanceMechanicalFallback
	}
	if !reflect.DeepEqual(maintenanceModes, wantModes) {
		t.Fatalf("maintenance modes = %v", maintenanceModes)
	}
}

func strictlyIncreasing(values []uint64, want int) bool {
	if len(values) != want {
		return false
	}
	for i := 1; i < len(values); i++ {
		if values[i] <= values[i-1] {
			return false
		}
	}
	return true
}
