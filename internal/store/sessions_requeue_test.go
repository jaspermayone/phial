package store

import (
	"context"
	"testing"
)

func TestRequeueFailedSessions(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	turns := []Turn{{Role: "user", Text: "hello"}}
	if _, err := s.PutSessions(ctx, []Session{{ID: "a", Source: "test", Turns: turns}, {ID: "b", Source: "test", Turns: turns}, {ID: "c", Source: "test", Turns: turns}}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkExtracted(ctx, "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkExtracted(ctx, "b", "llm: context canceled"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RequeueFailedSessions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("requeued %d, err %v; want 1", n, err)
	}
	pending, err := s.PendingSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, se := range pending {
		ids[se.ID] = true
		if se.ExtractError != "" {
			t.Errorf("%s still carries an error: %q", se.ID, se.ExtractError)
		}
	}
	if !ids["b"] || !ids["c"] || ids["a"] {
		t.Fatalf("pending = %v, want b and c", ids)
	}
}
