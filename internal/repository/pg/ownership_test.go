package pg

import (
	"context"
	"testing"

	"github.com/anuragxxd/conquest/internal/ownership"
)

func TestOwnerFilters_UnscopedAddsNothing(t *testing.T) {
	for name, filter := range map[string]func(context.Context, int) (string, []any){
		"runs":  runOwnerFilter,
		"tasks": taskOwnerFilter,
	} {
		clause, args := filter(context.Background(), 2)
		if clause != "" || args != nil {
			t.Errorf("%s: unscoped filter = %q %v, want none", name, clause, args)
		}
	}
}

func TestOwnerFilters_Scoped(t *testing.T) {
	ctx := ownership.NewContext(context.Background(), ownership.Owner{ID: "iss|alice"})
	tests := []struct {
		name   string
		filter func(context.Context, int) (string, []any)
		want   string
	}{
		{"runs", runOwnerFilter, " AND owner = $3"},
		{"tasks", taskOwnerFilter, " AND EXISTS (SELECT 1 FROM runs WHERE runs.run_id = tasks.run_id AND runs.owner = $3)"},
	}
	for _, tt := range tests {
		clause, args := tt.filter(ctx, 3)
		if clause != tt.want {
			t.Errorf("%s: clause = %q, want %q", tt.name, clause, tt.want)
		}
		if len(args) != 1 || args[0] != "iss|alice" {
			t.Errorf("%s: args = %v, want [iss|alice]", tt.name, args)
		}
	}
}

func TestOwnerFilters_EmptyOwnerStillFilters(t *testing.T) {
	// A scoped context with an empty owner ID must match nothing rather
	// than everything: owner = '' matches neither NULL nor a real owner.
	ctx := ownership.NewContext(context.Background(), ownership.Owner{})
	clause, args := runOwnerFilter(ctx, 2)
	if clause == "" || len(args) != 1 || args[0] != "" {
		t.Fatalf("filter = %q %v, want an owner predicate on \"\"", clause, args)
	}
}
