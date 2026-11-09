package telemetry_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
)

func TestRedactorStringDetectsCredentialFormats(t *testing.T) {
	redactor := telemetry.NewRedactor()
	for _, value := range []string{
		"password=fixture-password", `PASSWORD='secret with spaces'`, `password="unterminated value`,
		`{"api_key":"fixture-key"}`, "DATABASE_ENCRYPTION_KEY=fixture-key", "session_id=fixture-session",
		"Authorization: Bearer fixture-token", "Basic Zml4dHVyZTpwYXNzd29yZA==", "Cookie: platform_session=fixture-cookie; Path=/",
		"connect postgres://user:encoded%40password@localhost/app failed", "connect user:password with spaces@tcp(localhost:3306)/app failed",
		"https://example.com/callback?access_token=fixture-token", "query=SELECT private_data FROM accounts",
	} {
		if got := redactor.String(value); got != telemetry.Redacted {
			t.Fatalf("format credential belum disamarkan: %q", got)
		}
	}
	for _, value := range []string{"password authentication failed", "sslmode=require host=localhost", "migration completed", "https://example.com/public"} {
		if got := redactor.String(value); got != value {
			t.Fatalf("pesan operasional berubah: %q", got)
		}
	}
	var zero telemetry.Redactor
	if got := zero.String("password=fixture"); got != telemetry.Redacted {
		t.Fatal("zero value redactor tidak aman")
	}
}

func TestRedactorCopiesAndOrdersKnownSecrets(t *testing.T) {
	secrets := []string{"fixture-short", "fixture-short-long", "", "postgres://alice:encoded%40password@localhost/app"}
	redactor := telemetry.NewRedactor(secrets...)
	secrets[0] = "changed-after-construction"
	for input, want := range map[string]string{
		"failed with fixture-short-long": "failed with " + telemetry.Redacted,
		"failed with fixture-short":      "failed with " + telemetry.Redacted,
		"failed with encoded@password":   "failed with " + telemetry.Redacted,
	} {
		if got := redactor.String(input); got != want {
			t.Fatalf("secret literal tidak disamarkan: %q", got)
		}
	}
	if got := redactor.String("ordinary event"); got != "ordinary event" {
		t.Fatal("secret kosong menyamarkan semua teks")
	}
}

type credentialFixture struct {
	Username string         `json:"user_name"`
	Password string         `json:"Password"`
	Active   bool           `json:"active"`
	Data     map[string]any `json:"metadata"`
}

type credentialLogValue string

func (v credentialLogValue) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token", string(v)), slog.String("status", "valid"))
}

type panickingJSONValue struct{}

func (panickingJSONValue) MarshalJSON() ([]byte, error) { panic("fixture-marshaler-secret") }

type panickingError struct{}

func (panickingError) Error() string { panic("fixture-error-secret") }

func TestRedactorSanitizesStructuredAndUnsupportedValues(t *testing.T) {
	redactor := telemetry.NewRedactor("fixture-known-secret")
	cyclic := make(map[string]any)
	cyclic["self"] = cyclic
	for _, test := range []struct {
		name  string
		input any
		want  string
	}{
		{"nested struct", credentialFixture{Username: "fixture-user", Password: "fixture-password", Active: true, Data: map[string]any{"client.secret": "fixture-secret", "status": "valid"}}, `{"Password":"[REDACTED]","active":true,"metadata":{"client.secret":"[REDACTED]","status":"valid"},"user_name":"[REDACTED]"}`},
		{"raw JSON", json.RawMessage(`{"authorization":"fixture-token","id":9223372036854775807}`), `{"authorization":"[REDACTED]","id":9223372036854775807}`},
		{"error", errors.New("failed with fixture-known-secret"), `"failed with [REDACTED]"`},
		{"nil", nil, `null`},
		{"unsupported", make(chan string), `"[REDACTED]"`},
		{"cycle", cyclic, `"[REDACTED]"`},
		{"panic marshaler", panickingJSONValue{}, `"[REDACTED]"`},
		{"panic error", panickingError{}, `"[REDACTED]"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			attr := redactor.ReplaceAttr(nil, slog.Any("details", test.input))
			encoded, err := json.Marshal(attr.Value.Any())
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.want {
				t.Fatalf("hasil redaction = %s; ingin %s", encoded, test.want)
			}
		})
	}
	for _, key := range []string{"password", "PasswordHash", "DATABASE_ENCRYPTION_KEY", "platform.database_url", "sql_up", "parameters", "body"} {
		attr := redactor.ReplaceAttr(nil, slog.Any(key, panickingJSONValue{}))
		if attr.Value.String() != telemetry.Redacted {
			t.Fatalf("field sensitif %s diproses sebelum disamarkan", key)
		}
	}
}

func TestLoggerResolvesLogValuerBeforeRedaction(t *testing.T) {
	var output strings.Builder
	telemetry.NewLogger(&output, "server").Info("session_checked", "details", credentialLogValue("fixture-secret-token"))
	if strings.Contains(output.String(), "fixture-secret-token") || !strings.Contains(output.String(), "valid") {
		t.Fatal("LogValuer melewati redaction")
	}
}

func TestSanitizeSQLHidesLiteralsIdentifiersAndComments(t *testing.T) {
	for _, test := range []struct {
		system, query string
		hidden        []string
	}{
		{"postgresql", "SELECT private_field FROM private_table WHERE private_field = 'fixture-secret' AND private_id = $1 /* fixture-comment */", []string{"private_field", "private_table", "private_id", "fixture-secret", "fixture-comment"}},
		{"postgresql", `DO $body$ BEGIN RAISE NOTICE 'fixture-dollar-secret'; END $body$`, []string{"fixture-dollar-secret", "RAISE"}},
		{"postgresql", `CREATE ROLE private_user PASSWORD 'fixture-password'`, []string{"private_user", "fixture-password"}},
		{"postgresql", `SELECT E'fixture-escape-secret\\value', 12345, \"private_identifier\"`, []string{"fixture-escape-secret", "12345", "private_identifier"}},
		{"mysql", "SELECT `private_field` FROM `private_table` WHERE id = \"fixture-double-secret\" # fixture-comment", []string{"private_field", "private_table", "fixture-double-secret", "fixture-comment"}},
		{"mysql", "SELECT 'fixture-unterminated", []string{"fixture-unterminated"}},
		{"mysql", "CREATE USER private_user IDENTIFIED BY 'fixture-password'", []string{"private_user", "fixture-password"}},
		{"mysql", "SELECT /* comment */ 0xDEADBEEF; /*! fixture-executable-comment */", []string{"DEADBEEF", "fixture-executable-comment"}},
	} {
		t.Run(test.system+test.query, func(t *testing.T) {
			got := telemetry.SanitizeSQL(test.query, test.system)
			for _, hidden := range test.hidden {
				if strings.Contains(strings.ToLower(got), strings.ToLower(hidden)) {
					t.Fatalf("SQL sensitif masih muncul: %q", got)
				}
			}
			if got == "" {
				t.Fatal("template SQL hilang")
			}
		})
	}
	if got := telemetry.SanitizeSQL("SELECT name FROM users WHERE id = $1", "postgresql"); got != "SELECT ? FROM ? WHERE ? = ?" {
		t.Fatalf("template = %q", got)
	}
}
