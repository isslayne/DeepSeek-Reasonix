package agent

import (
	"errors"
	"strings"
	"testing"

	"reasonix/internal/provider"
)

func TestRequestBudgetUsesLearnedSharedWindowAndDynamicMargin(t *testing.T) {
	prov := &policyWindowProvider{policy: provider.ContextBudgetPolicy{
		WindowMode: provider.ContextWindowShared,
		LimitMode:  provider.OutputLimitAlways,
	}}
	msgs := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("x", 80_000)}}
	a := &Agent{agentConfig: agentConfig{contextWindow: 64_000}, svc: agentServices{prov: prov}}
	a.sess.output.learned.Store(&learnedContextBudget{windowTokens: 24_000, completionBudget: 2_048})
	a.setPromptTokenCalibration(20_000, requestCalibrationShapeOf(provider.Request{Messages: msgs}))

	budget := a.requestBudget(provider.Request{Messages: msgs, MaxTokens: 2_048}, 2_048, 256)
	if budget.EffectiveWindow != 24_000 || budget.ProtocolMargin != 256 || budget.HardInputCeiling != 23_744 {
		t.Fatalf("budget window/margin = %+v", budget)
	}
	if budget.EstimatedPrompt != 20_000 || budget.EffectiveOutput != 2_048 {
		t.Fatalf("budget prompt/output = %+v", budget)
	}
}

func TestSummaryPlannerAndSenderShareMinimumOutputAdmission(t *testing.T) {
	prov := &policyWindowProvider{policy: provider.ContextBudgetPolicy{
		WindowMode: provider.ContextWindowShared,
		LimitMode:  provider.OutputLimitAlways,
	}}
	a := &Agent{agentConfig: agentConfig{contextWindow: 24_000}, svc: agentServices{prov: prov}}
	region := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("work ", 2_000)}}
	req := a.summaryRequestForPurpose(region, "", summaryPurposeActiveCheckpoint)
	desired, minimum := a.summaryOutputEnvelope(summaryPurposeActiveCheckpoint)
	if desired != activeTurnSummaryMaxTokens {
		t.Fatalf("active checkpoint desired output = %d, want %d", desired, activeTurnSummaryMaxTokens)
	}
	planned := a.requestBudget(req, desired, minimum)
	if err := a.applySummaryAdmission(&req, summaryPurposeActiveCheckpoint); err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != planned.EffectiveOutput || req.MaxTokens < minimum {
		t.Fatalf("sender max=%d planner=%+v", req.MaxTokens, planned)
	}
	if len(req.Tools) != 0 {
		t.Fatalf("active checkpoint summary sent unusable tool schemas: %+v", req.Tools)
	}
}

func TestClassifyNoFoldFailsClosedForUnsafePairing(t *testing.T) {
	a := &Agent{agentConfig: agentConfig{contextWindow: 4_000}}
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "task"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{Name: "write", Arguments: `{}`}}},
		{Role: provider.RoleTool, Name: "write", Content: "done"},
	}
	err := a.classifyNoFold(msgs, "unsafe fixture")
	var detail *IrreducibleContextError
	if !errors.As(err, &detail) || detail.Reason != IrreducibleUnsafeToolPairing {
		t.Fatalf("error = %v detail=%+v", err, detail)
	}
	if detail.LargestAtomicUnit == 0 || detail.InputHash == "" {
		t.Fatalf("missing irreducible diagnostics: %+v", detail)
	}
}

func TestMaintenanceAttemptKeyRequiresStateProgress(t *testing.T) {
	a := &Agent{}
	key := MaintenanceAttemptKey{InputHash: "sha256:test", ProjectionGen: 3, EffectiveWindow: 24_000, Trigger: CompactionTriggerOverflow}
	if !a.registerMaintenanceAttempt(key) || a.registerMaintenanceAttempt(key) {
		t.Fatal("identical maintenance attempt key was not deduplicated")
	}
	key.ProjectionGen++
	if !a.registerMaintenanceAttempt(key) {
		t.Fatal("advanced projection generation did not permit a new attempt")
	}
}
