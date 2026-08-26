package provider

import "testing"

func TestModelMessagesStripsTypedProjectionMetadata(t *testing.T) {
	stored := []Message{{
		Role: RoleUser, Content: "maintenance control",
		ProjectionKind: "synthetic_control", Synthetic: true,
	}}
	wire := ModelMessages(stored)
	if len(wire) != 1 || wire[0].ProjectionKind != "" || wire[0].Synthetic {
		t.Fatalf("provider projection leaked host metadata: %+v", wire)
	}
	if stored[0].ProjectionKind == "" || !stored[0].Synthetic {
		t.Fatal("ModelMessages mutated stored projection metadata")
	}
	persisted := ProjectionMessages(stored)
	if persisted[0].ProjectionKind != "synthetic_control" || !persisted[0].Synthetic {
		t.Fatalf("stored projection lost typed metadata: %+v", persisted)
	}
}
