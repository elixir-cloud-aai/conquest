package pg

import (
	"fmt"
	"testing"
	"time"
)

func TestParseCursor_RoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 25, 6, 53, 47, 123456000, time.UTC)
	token := fmt.Sprintf("%d:%s", created.UnixNano(), "run-1")

	got, id, err := parseCursor(token)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(created) || id != "run-1" {
		t.Fatalf("cursor = %v %q, want %v run-1", got, id, created)
	}
}

func TestParseCursor_Malformed(t *testing.T) {
	for _, token := range []string{"", "no-colon", "abc:run-1"} {
		if _, _, err := parseCursor(token); err == nil {
			t.Errorf("%q accepted", token)
		}
	}
}
