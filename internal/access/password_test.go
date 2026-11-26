package access

import (
	"strings"
	"testing"
)

func TestHashPasswordProducesSaltedPBKDF2Hash(t *testing.T) {
	first, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(first, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" || parts[1] != "600000" {
		t.Fatalf("unexpected hash format %q", first)
	}
	if first == second {
		t.Fatal("equal passwords produced the same hash")
	}
	if strings.Contains(first, "correct horse battery") {
		t.Fatal("hash contains the plaintext password")
	}
}

func TestVerifyPassword(t *testing.T) {
	encoded, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(encoded, "$")
	replace := func(index int, value string) string {
		changed := append([]string(nil), parts...)
		changed[index] = value
		return strings.Join(changed, "$")
	}
	tests := []struct {
		name     string
		encoded  string
		password string
		want     bool
	}{
		{name: "matching password", encoded: encoded, password: "correct horse battery", want: true},
		{name: "wrong password", encoded: encoded, password: "correct horse battery!"},
		{name: "empty password", encoded: encoded, password: ""},
		{name: "unknown algorithm", encoded: replace(0, "bcrypt"), password: "correct horse battery"},
		{name: "iterations below minimum", encoded: replace(1, "99999"), password: "correct horse battery"},
		{name: "iterations above maximum", encoded: replace(1, "2000001"), password: "correct horse battery"},
		{name: "non numeric iterations", encoded: replace(1, "many"), password: "correct horse battery"},
		{name: "invalid salt encoding", encoded: replace(2, "%%%"), password: "correct horse battery"},
		{name: "short salt", encoded: replace(2, "AAAA"), password: "correct horse battery"},
		{name: "short key", encoded: replace(3, "AAAA"), password: "correct horse battery"},
		{name: "missing segment", encoded: strings.Join(parts[:3], "$"), password: "correct horse battery"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := verifyPassword(test.encoded, test.password); got != test.want {
				t.Fatalf("verifyPassword = %t, want %t", got, test.want)
			}
		})
	}
}
