package pg

import (
	"context"
	"testing"

	"github.com/anuragxxd/conquest/internal/ownership"
)

func TestOwnerFilters_UnscopedAddsNothing(t *testing.T) {
	for name, filter := range map[string]func(context.Context, ...any) (string, []any){
		"runs":  runOwnerFilter,
		"tasks": taskOwnerFilter,
	} {
		clause, args := filter(context.Background(), "id")
		if clause != "" || len(args) != 1 || args[0] != "id" {
			t.Errorf("%s: unscoped filter = %q %v, want none and [id]", name, clause, args)
		}
	}
}

func TestOwnerFilters_Scoped(t *testing.T) {
	ctx := ownership.NewContext(context.Background(), ownership.Owner{ID: "iss|alice"})
	tests := []struct {
		name   string
		filter func(context.Context, ...any) (string, []any)
		want   string
	}{
		{"runs", runOwnerFilter, " AND owner = $3"},
		{"tasks", taskOwnerFilter, " AND EXISTS (SELECT 1 FROM runs WHERE runs.run_id = tasks.run_id AND runs.owner = $3)"},
	}
	for _, tt := range tests {
		clause, args := tt.filter(ctx, "a", 2)
		if clause != tt.want {
			t.Errorf("%s: clause = %q, want %q", tt.name, clause, tt.want)
		}
		if len(args) != 3 || args[0] != "a" || args[1] != 2 || args[2] != "iss|alice" {
			t.Errorf("%s: args = %v, want [a 2 iss|alice]", tt.name, args)
		}
	}
}

func TestOwnerFilters_EmptyOwnerStillFilters(t *testing.T) {
	// A scoped context with an empty owner ID must match nothing rather
	// than everything: owner = '' matches neither NULL nor a real owner.
	ctx := ownership.NewContext(context.Background(), ownership.Owner{})
	clause, args := runOwnerFilter(ctx)
	if clause != " AND owner = $1" || len(args) != 1 || args[0] != "" {
		t.Fatalf("filter = %q %v, want an owner predicate on \"\"", clause, args)
	}
}
