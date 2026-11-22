package uuid

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	tests := map[string]bool{
		"00000000-0000-4000-8000-000000000001":  true,
		"00000000-0000-4000-8000-00000000000Z":  false,
		"00000000000040008000000000000001":      false,
		"00000000-0000-4000-8000-0000000000011": false,
		"":                                      false,
		"../admin":                              false,
	}
	for value, want := range tests {
		if got := Valid(value); got != want {
			t.Errorf("Valid(%q) = %t, want %t", value, got, want)
		}
	}
}

func TestNewIsVersion4(t *testing.T) {
	id, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if !Valid(id) || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
		t.Fatalf("New() = %q, not a version 4 UUID", id)
	}
}
