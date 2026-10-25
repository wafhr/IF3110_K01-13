package database_test

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

func TestNewRepositoryRejectsInvalidKey(t *testing.T) {
	db := testDatabase(t)
	for _, key := range []string{"", "not-base64!", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := database.NewRepository(db, key); err == nil {
			t.Error("expected invalid encryption key error")
		}
	}
	if _, err := database.NewRepository(nil, ""); err == nil {
		t.Fatal("expected nil database error")
	}
}

func TestPasswordEncryption(t *testing.T) {
	for _, password := range []string{"", " private-password with spaces "} {
		first := createAndCaptureCiphertext(t, password)
		second := createAndCaptureCiphertext(t, password)
		if bytes.Equal(first, second) {
			t.Fatal("password encryption reused a nonce")
		}
		if password != "" && bytes.Contains(first, []byte(password)) {
			t.Fatal("ciphertext contains plaintext password")
		}
		config, err := readConnectionConfig(t, first, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
		if err != nil {
			t.Fatal("password did not round trip:", err)
		}
		if config.Password != password {
			t.Fatal("password did not round trip")
		}
		tampered := append([]byte(nil), first...)
		tampered[len(tampered)-1] ^= 1
		if _, err := readConnectionConfig(t, tampered, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))); !errors.Is(err, database.ErrStorage) {
			t.Fatalf("tampered password error = %v", err)
		}
		if _, err := readConnectionConfig(t, []byte("short"), base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))); !errors.Is(err, database.ErrStorage) {
			t.Fatal("truncated ciphertext must be rejected")
		}
		if _, err := readConnectionConfig(t, first, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))); !errors.Is(err, database.ErrStorage) {
			t.Fatal("wrong key must not decrypt credentials")
		}
	}
}

func TestInitialize(t *testing.T) {
	r := testRepository(t,
		databaseStep{statement: "CREATE TABLE IF NOT EXISTS managed_databases", exec: true},
		databaseStep{statement: "ALTER TABLE managed_databases ADD COLUMN IF NOT EXISTS dbms", exec: true},
		databaseStep{statement: "DO $$ BEGIN", exec: true},
		databaseStep{statement: "CREATE TABLE IF NOT EXISTS managed_databases", exec: true},
		databaseStep{statement: "ALTER TABLE managed_databases ADD COLUMN IF NOT EXISTS dbms", exec: true},
		databaseStep{statement: "DO $$ BEGIN", exec: true},
	)
	for range 2 {
		if err := r.Initialize(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	r = testRepository(t, databaseStep{statement: "CREATE TABLE", exec: true, err: errors.New("private-password")})
	assertStorageError(t, r.Initialize(context.Background()), database.ErrStorage)
}

func TestCreate(t *testing.T) {
	step := metadataStep("INSERT INTO managed_databases", "Main")
	var storedPassword []byte
	step.check = func(t *testing.T, args []driver.NamedValue) {
		if len(args) != 8 {
			t.Fatalf("got %d arguments, want 8", len(args))
		}
		if args[0].Value != "Main" || args[1].Value != "postgres" || args[2].Value != "localhost" || args[3].Value != int64(5432) || args[7].Value != "require" {
			t.Error("normalized registration was not persisted")
		}
		var ok bool
		storedPassword, ok = args[6].Value.([]byte)
		if !ok || bytes.Contains(storedPassword, []byte("secret-password")) {
			t.Fatal("password must be stored as ciphertext")
		}
	}
	r := testRepository(t, step)
	got, err := r.Create(context.Background(), " Main ", validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != testDatabaseID || got.Name != "Main" || !got.CreatedAt.Equal(testCreatedAt) {
		t.Fatalf("unexpected public metadata: %+v", got)
	}
	config, err := readConnectionConfig(t, storedPassword, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil || config.Password != validConfig().Password {
		t.Fatal("stored password cannot be recovered")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "password") {
		t.Fatal("public database metadata contains password")
	}
}

func TestCreateErrors(t *testing.T) {
	r := testRepository(t)
	_, err := r.Create(context.Background(), "", validConfig())
	assertStorageError(t, err, database.ErrInvalidInput)
	for _, name := range []string{"  ", strings.Repeat("a", 101), "main\x00", string([]byte{255})} {
		if _, err := r.Create(context.Background(), name, validConfig()); !errors.Is(err, database.ErrInvalidInput) {
			t.Errorf("Create(%q) error = %v, want invalid input", name, err)
		}
	}
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"duplicate", fmt.Errorf("wrapped: %w", stateError("23505")), database.ErrConflict},
		{"storage", errors.New("private-password"), database.ErrStorage},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRepository(t, databaseStep{statement: "INSERT INTO managed_databases", err: test.err})
			_, err := r.Create(context.Background(), "Main", validConfig())
			assertStorageError(t, err, test.want)
		})
	}
}

func TestGet(t *testing.T) {
	step := metadataStep("FROM managed_databases WHERE id", "Main")
	step.check = checkID
	r := testRepository(t, step)
	got, err := r.Get(context.Background(), testDatabaseID)
	if err != nil || got.ID != testDatabaseID || got.Name != "Main" {
		t.Fatalf("unexpected Get result: %+v, %v", got, err)
	}
	for _, test := range []struct {
		name string
		step databaseStep
		want error
	}{
		{"missing", databaseStep{statement: "SELECT", columns: step.columns}, database.ErrNotFound},
		{"query failure", databaseStep{statement: "SELECT", err: errors.New("private-password")}, database.ErrStorage},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRepository(t, test.step)
			_, err := r.Get(context.Background(), testDatabaseID)
			assertStorageError(t, err, test.want)
		})
	}
}

func TestList(t *testing.T) {
	step := metadataStep("ORDER BY name, id", "First")
	second := append([]driver.Value(nil), step.rows[0]...)
	second[0], second[1] = "00000000-0000-4000-8000-000000000002", "Second"
	step.rows = append(step.rows, second)
	r := testRepository(t, step)
	got, err := r.List(context.Background())
	if err != nil || len(got) != 2 || got[0].Name != "First" || got[1].Name != "Second" {
		t.Fatalf("unexpected list result: %+v, %v", got, err)
	}
	r = testRepository(t, databaseStep{statement: "SELECT", columns: step.columns})
	got, err = r.List(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatal("empty list should be a non-nil slice")
	}
}

func TestListErrors(t *testing.T) {
	badRow := metadataStep("SELECT", "Main")
	badRow.rows[0][4] = "not-an-integer"
	for _, test := range []struct {
		name string
		step databaseStep
	}{
		{"query failure", databaseStep{statement: "SELECT", err: errors.New("private-password")}},
		{"scan failure", badRow},
		{"row iteration failure", databaseStep{statement: "SELECT", columns: []string{"id"}, nextErr: errors.New("private-password")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRepository(t, test.step)
			got, err := r.List(context.Background())
			assertStorageError(t, err, database.ErrStorage)
			if got != nil {
				t.Fatal("failed listing must not return partial results")
			}
		})
	}
}

func TestUpdate(t *testing.T) {
	step := metadataStep("UPDATE managed_databases", "Renamed")
	var encrypted []byte
	step.check = func(t *testing.T, args []driver.NamedValue) {
		if len(args) != 9 || args[0].Value != "Renamed" || args[1].Value != "postgres" || args[8].Value != testDatabaseID {
			t.Fatal("update did not preserve UUID or normalize name")
		}
		encrypted, _ = args[6].Value.([]byte)
	}
	r := testRepository(t, step)
	config := validConfig()
	config.Password = ""
	got, err := r.Update(context.Background(), testDatabaseID, " Renamed ", config)
	if err != nil || got.ID != testDatabaseID || got.Name != "Renamed" || !got.CreatedAt.Equal(testCreatedAt) {
		t.Fatalf("unexpected Update result: %+v, %v", got, err)
	}
	storedConfig, err := readConnectionConfig(t, encrypted, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil || storedConfig.Password != "" {
		t.Fatal("empty password must be persisted as an explicit replacement")
	}
}

func TestUpdateErrors(t *testing.T) {
	r := testRepository(t)
	_, err := r.Update(context.Background(), testDatabaseID, "", validConfig())
	assertStorageError(t, err, database.ErrInvalidInput)
	for _, test := range []struct {
		name string
		step databaseStep
		want error
	}{
		{"missing", databaseStep{statement: "UPDATE", columns: metadataStep("", "").columns}, database.ErrNotFound},
		{"duplicate", databaseStep{statement: "UPDATE", err: stateError("23505")}, database.ErrConflict},
		{"query failure", databaseStep{statement: "UPDATE", err: errors.New("private-password")}, database.ErrStorage},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRepository(t, test.step)
			_, err := r.Update(context.Background(), testDatabaseID, "Main", validConfig())
			assertStorageError(t, err, test.want)
		})
	}
}

func TestDelete(t *testing.T) {
	for _, test := range []struct {
		name   string
		result driver.Result
		err    error
		want   error
	}{
		{"deleted", driver.RowsAffected(1), nil, nil},
		{"missing", driver.RowsAffected(0), nil, database.ErrNotFound},
		{"storage failure", nil, errors.New("private-password"), database.ErrStorage},
		{"rows affected failure", badResult{}, nil, database.ErrStorage},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRepository(t, databaseStep{statement: "DELETE FROM managed_databases", exec: true,
				result: test.result, err: test.err, check: checkID})
			assertStorageError(t, r.Delete(context.Background(), testDatabaseID), test.want)
		})
	}
}

func TestGetConnectionConfig(t *testing.T) {
	encrypted := createAndCaptureCiphertext(t, " private-password ")
	step := databaseStep{
		statement: "SELECT dbms, host, port, database_name, username, password_ciphertext, ssl_mode",
		columns:   []string{"dbms", "host", "port", "database_name", "username", "password_ciphertext", "ssl_mode"},
		rows:      [][]driver.Value{{"postgres", "localhost", int64(5433), "app", "reader", encrypted, "verify-full"}},
		check:     checkID,
	}
	r := testRepository(t, step)
	got, err := r.GetConnectionConfig(context.Background(), testDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DBMS != database.PostgreSQL || got.Host != "localhost" || got.Port != 5433 || got.DatabaseName != "app" ||
		got.Username != "reader" || got.Password != " private-password " || got.SSLMode != "verify-full" {
		t.Fatal("stored connection configuration was not recovered correctly")
	}
	for _, test := range []struct {
		name string
		step databaseStep
		want error
	}{
		{"missing", databaseStep{statement: "SELECT", columns: step.columns}, database.ErrNotFound},
		{"query failure", databaseStep{statement: "SELECT", err: errors.New("private-password")}, database.ErrStorage},
		{"corrupted credentials", databaseStep{statement: "SELECT", columns: step.columns,
			rows: [][]driver.Value{{"postgres", "localhost", int64(5432), "app", "reader", []byte("corrupt"), "require"}}}, database.ErrStorage},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRepository(t, test.step)
			got, err := r.GetConnectionConfig(context.Background(), testDatabaseID)
			assertStorageError(t, err, test.want)
			if got != (database.ConnectionConfig{}) {
				t.Fatal("failed credential lookup must not return partial configuration")
			}
		})
	}
}

func TestMalformedIDsDoNotQueryDatabase(t *testing.T) {
	r := testRepository(t)
	ctx := context.Background()
	_, err := r.Get(ctx, "bad-id")
	assertStorageError(t, err, database.ErrNotFound)
	_, err = r.Update(ctx, "bad-id", "Main", validConfig())
	assertStorageError(t, err, database.ErrNotFound)
	assertStorageError(t, r.Delete(ctx, "bad-id"), database.ErrNotFound)
	_, err = r.GetConnectionConfig(ctx, "bad-id")
	assertStorageError(t, err, database.ErrNotFound)
}

func createAndCaptureCiphertext(t *testing.T, password string) []byte {
	t.Helper()
	step := metadataStep("INSERT INTO managed_databases", "Main")
	var ciphertext []byte
	step.check = func(t *testing.T, args []driver.NamedValue) {
		if len(args) != 8 {
			t.Fatalf("got %d arguments, want 8", len(args))
		}
		var ok bool
		ciphertext, ok = args[6].Value.([]byte)
		if !ok {
			t.Fatal("password must be stored as ciphertext")
		}
		ciphertext = append([]byte(nil), ciphertext...)
	}
	r := testRepository(t, step)
	config := validConfig()
	config.Password = password
	if _, err := r.Create(context.Background(), "Main", config); err != nil {
		t.Fatal(err)
	}
	return ciphertext
}

func readConnectionConfig(t *testing.T, ciphertext []byte, key string) (database.ConnectionConfig, error) {
	t.Helper()
	step := databaseStep{
		statement: "SELECT dbms, host, port, database_name, username, password_ciphertext, ssl_mode",
		columns:   []string{"dbms", "host", "port", "database_name", "username", "password_ciphertext", "ssl_mode"},
		rows:      [][]driver.Value{{"postgres", "localhost", int64(5432), "app", "reader", ciphertext, "require"}},
		check:     checkID,
	}
	r := testRepositoryWithKey(t, key, step)
	return r.GetConnectionConfig(context.Background(), testDatabaseID)
}

func checkID(t *testing.T, args []driver.NamedValue) {
	t.Helper()
	if len(args) != 1 || args[0].Value != testDatabaseID {
		t.Fatal("database UUID must be passed as a query parameter")
	}
}

func assertStorageError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
	if got != nil && strings.Contains(got.Error(), "private-password") {
		t.Fatal("error disclosed private credentials")
	}
}
