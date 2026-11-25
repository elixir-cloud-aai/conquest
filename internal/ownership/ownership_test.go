package ownership

import (
	"context"
	"testing"
)

func TestFromContext_Unscoped(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("background context must be unscoped")
	}
}

func TestNewContext_RoundTrip(t *testing.T) {
	want := Owner{ID: "https://issuer.example/|alice", Context: []byte(`{"client_id":"web"}`)}
	got, ok := FromContext(NewContext(context.Background(), want))
	if !ok {
		t.Fatal("scoped context reported unscoped")
	}
	if got.ID != want.ID || string(got.Context) != string(want.Context) {
		t.Fatalf("owner = %+v, want %+v", got, want)
	}
}

func TestNewContext_EmptyIDStaysScoped(t *testing.T) {
	// An empty owner must not fall back to "unscoped" (that would show
	// every run); the repository matches no run for it instead.
	got, ok := FromContext(NewContext(context.Background(), Owner{}))
	if !ok || got.ID != "" {
		t.Fatalf("owner = %+v, ok = %v; want empty owner, scoped", got, ok)
	}
}
