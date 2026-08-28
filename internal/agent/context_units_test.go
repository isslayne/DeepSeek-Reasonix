package agent

import (
	"testing"

	"reasonix/internal/provider"
)

func TestBuildContextUnitsCompleteParallelToolGroup(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "a", Name: "read", Arguments: `{}`}, {ID: "b", Name: "grep", Arguments: `{}`}}},
		{Role: provider.RoleTool, ToolCallID: "b", Name: "grep", Content: "B"},
		{Role: provider.RoleTool, ToolCallID: "a", Name: "read", Content: "A"},
	}
	units := buildContextUnits(msgs, contextUnitBuildOptions{})
	if len(units) != 1 || units[0].Kind != UnitToolGroup || !units[0].Complete {
		t.Fatalf("units = %+v, want one complete tool group", units)
	}
	if units[0].ToolGroup == nil || units[0].ToolGroup.PairingMode != "id" || len(units[0].ToolGroup.Calls) != 2 {
		t.Fatalf("receipt = %+v", units[0].ToolGroup)
	}
	if units[0].ToolGroup.Calls[0].ResultHash != hashContextPayload("A") || units[0].ToolGroup.Calls[1].ResultHash != hashContextPayload("B") {
		t.Fatalf("results were not paired by ID: %+v", units[0].ToolGroup.Calls)
	}
}

func TestBuildContextUnitsIncompleteToolGroupFailsClosed(t *testing.T) {
	tests := map[string][]provider.Message{
		"missing result": {
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "a"}, {ID: "b"}}},
			{Role: provider.RoleTool, ToolCallID: "a", Content: "A"},
		},
		"duplicate call ids": {
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "dup"}, {ID: "dup"}}},
			{Role: provider.RoleTool, ToolCallID: "dup", Content: "A"},
			{Role: provider.RoleTool, ToolCallID: "dup", Content: "B"},
		},
		"empty call ids": {
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: ""}, {ID: ""}}},
			{Role: provider.RoleTool, Content: "A"},
			{Role: provider.RoleTool, Content: "B"},
		},
		"duplicate result": {
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "a"}, {ID: "b"}}},
			{Role: provider.RoleTool, ToolCallID: "a", Content: "A"},
			{Role: provider.RoleTool, ToolCallID: "a", Content: "again"},
		},
	}
	for name, msgs := range tests {
		t.Run(name, func(t *testing.T) {
			units := buildContextUnits(msgs, contextUnitBuildOptions{})
			if len(units) != 1 || units[0].Complete {
				t.Fatalf("units = %+v, want one incomplete group", units)
			}
		})
	}
}

func TestBuildContextUnitsAllowsExplicitPositionalPairing(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{Name: "a"}, {Name: "b"}}},
		{Role: provider.RoleTool, Name: "a", Content: "A"},
		{Role: provider.RoleTool, Name: "b", Content: "B"},
	}
	strict := buildContextUnits(msgs, contextUnitBuildOptions{})
	positional := buildContextUnits(msgs, contextUnitBuildOptions{allowPositionalToolPairing: true})
	if strict[0].Complete {
		t.Fatal("strict mode accepted empty IDs")
	}
	if !positional[0].Complete || positional[0].ToolGroup.PairingMode != "positional" {
		t.Fatalf("positional unit = %+v", positional[0])
	}
}

func TestBuildContextUnitsSeparatesSyntheticAndUserAuthoredTurns(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "real task"},
		{Role: provider.RoleUser, Content: activeTurnContinuation},
		{Role: provider.RoleAssistant, Content: "display only", LocalOnly: true},
	}
	units := buildContextUnits(msgs, contextUnitBuildOptions{})
	if len(units) != 4 || units[0].Kind != UnitSystem || units[1].Kind != UnitUserTurn ||
		!units[1].UserAuthored || units[2].Kind != UnitSyntheticControl || units[2].UserAuthored ||
		units[3].Kind != UnitSyntheticControl || units[3].ProviderVisible {
		t.Fatalf("unexpected units: %+v", units)
	}
}
