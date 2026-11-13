package access

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeUsername(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{input: "  Alice.Dev_01  ", want: "alice.dev_01"},
		{input: "abc", want: "abc"},
		{input: strings.Repeat("a", 64), want: strings.Repeat("a", 64)},
		{input: "ab", wantErr: true},
		{input: strings.Repeat("a", 65), wantErr: true},
		{input: "alice smith", wantErr: true},
		{input: "alice@example", wantErr: true},
		{input: "ālice", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := normalizeUsername(test.input)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("error = %v, want invalid input", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("normalizeUsername = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		valid    bool
	}{
		{name: "twelve characters", password: "abcdefghijkl", valid: true},
		{name: "eleven characters", password: "abcdefghijk"},
		{name: "multibyte characters count as runes", password: strings.Repeat("é", 12), valid: true},
		{name: "longest accepted", password: strings.Repeat("a", 1024), valid: true},
		{name: "longer than limit", password: strings.Repeat("a", 1025)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validatePassword(test.password)
			if test.valid != (err == nil) {
				t.Fatalf("validatePassword error = %v, want valid %t", err, test.valid)
			}
			if err != nil && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error %v is not invalid input", err)
			}
		})
	}
}

func TestNormalizeGroupName(t *testing.T) {
	if got, err := normalizeGroupName("  Data Team  "); err != nil || got != "Data Team" {
		t.Fatalf("normalizeGroupName = %q, %v", got, err)
	}
	for _, name := range []string{"", "   ", strings.Repeat("g", 101)} {
		if _, err := normalizeGroupName(name); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("name of length %d accepted", len(name))
		}
	}
	if _, err := normalizeGroupName(strings.Repeat("é", 100)); err != nil {
		t.Fatalf("100 multibyte characters rejected: %v", err)
	}
}

func TestValidUUID(t *testing.T) {
	tests := map[string]bool{
		testUserID:                              true,
		"00000000-0000-4000-8000-00000000000Z":  false,
		"00000000000040008000000000000001":      false,
		"00000000-0000-4000-8000-0000000000011": false,
		"":                                      false,
		"../admin":                              false,
	}
	for value, want := range tests {
		if got := validUUID(value); got != want {
			t.Errorf("validUUID(%q) = %t, want %t", value, got, want)
		}
	}
}

func TestInputErrorKeepsReasonAndMatchesSentinel(t *testing.T) {
	err := fmt.Errorf("create user: %w", invalidInput("username is taken"))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatal("wrapped input error does not match ErrInvalidInput")
	}
	var input *inputError
	if !errors.As(err, &input) || input.reason != "username is taken" {
		t.Fatal("input error reason is not reachable")
	}
}

func TestSQLStateMapping(t *testing.T) {
	wrapped := func(state string) error { return fmt.Errorf("insert: %w", sqlStateError(state)) }
	if !isUniqueViolation(wrapped("23505")) || isUniqueViolation(wrapped("23503")) || isUniqueViolation(errDatabaseDown) {
		t.Fatal("unique violation detection is wrong")
	}
	if !errors.Is(mapMissingOrError(wrapped("23503")), ErrNotFound) {
		t.Fatal("foreign-key violation is not mapped to not found")
	}
	if err := mapMissingOrError(errDatabaseDown); !errors.Is(err, errDatabaseDown) {
		t.Fatal("unrelated error was replaced")
	}
}
