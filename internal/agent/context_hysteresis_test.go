package agent

import (
	"context"
	"strings"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func TestPressureMaintenanceHonorsPersistedRearmHysteresis(t *testing.T) {
	prov := &countingProvider{reply: "durable history checkpoint"}
	sess := NewSession("system")
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "long task"})
	a := New(prov, tool.NewRegistry(), sess, Options{ContextWindow: 10_000, CompactRatio: 0.80}, event.Discard)
	for i := 0; i < 100 && a.estimatedVisibleRequestTokens(a.modelVisibleMessages()) < a.compactTrigger(); i++ {
		sess.Add(provider.Message{Role: provider.RoleAssistant, Content: strings.Repeat("completed work ", 150)})
		sess.Add(provider.Message{Role: provider.RoleUser, Content: "continue"})
	}
	est := a.estimatedVisibleRequestTokens(a.modelVisibleMessages())
	if est < a.compactTrigger() || est >= a.hardInputCeiling() {
		t.Fatalf("fixture tokens=%d trigger=%d hard=%d", est, a.compactTrigger(), a.hardInputCeiling())
	}
	a.sess.compactionState.MaintenanceRearmAtTokens = est + 100
	if _, err := a.contextManager().Prepare(context.Background(), ContextPreparePolicy{Trigger: CompactionTriggerPressure}); err != nil {
		t.Fatal(err)
	}
	if len(prov.got) != 0 {
		t.Fatalf("maintenance ran before hysteresis rearm: %d summary requests", len(prov.got))
	}

	sess.Add(provider.Message{Role: provider.RoleAssistant, Content: strings.Repeat("new work ", 200)})
	sess.Add(provider.Message{Role: provider.RoleUser, Content: "continue"})
	if got := a.estimatedVisibleRequestTokens(a.modelVisibleMessages()); got < a.sess.compactionState.MaintenanceRearmAtTokens {
		t.Fatalf("fixture did not cross rearm: %d < %d", got, a.sess.compactionState.MaintenanceRearmAtTokens)
	}
	if _, err := a.contextManager().Prepare(context.Background(), ContextPreparePolicy{Trigger: CompactionTriggerPressure}); err != nil {
		t.Fatal(err)
	}
	if len(prov.got) == 0 {
		t.Fatal("maintenance did not resume after new canonical work crossed rearm")
	}
	status := a.ContextMaintenanceSnapshot()
	if status.ProjectionGeneration == 0 || status.CacheGeneration == 0 || status.MaintenanceRearmAtTokens <= status.ProjectedTokens {
		t.Fatalf("post-maintenance status = %+v", status)
	}
}
