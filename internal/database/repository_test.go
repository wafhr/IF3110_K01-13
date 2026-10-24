package database

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
)

func TestNewRepositoryRejectsInvalidKey(t *testing.T) {
	r := testRepository(t)
	for _, key := range []string{"", "not-base64!", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := NewRepository(r.db, key); err == nil {
			t.Error("expected invalid encryption key error")
		}
	}
	if _, err := NewRepository(nil, ""); err == nil {
		t.Fatal("expected nil database error")
	}
}

func TestPasswordEncryption(t *testing.T) {
	r := testRepository(t)
	for _, password := range []string{"", " private-password with spaces "} {
		first, err := r.encryptPassword(password)
		if err != nil {
			t.Fatal(err)
		}
		second, err := r.encryptPassword(password)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(first, second) {
			t.Fatal("password encryption reused a nonce")
		}
		got, err := r.decryptPassword(first)
		if err != nil || got != password {
			t.Fatal("password did not round trip")
		}
		if password != "" && bytes.Contains(first, []byte(password)) {
			t.Fatal("ciphertext contains plaintext password")
		}
		first[len(first)-1] ^= 1
		if _, err := r.decryptPassword(first); !errors.Is(err, ErrStorage) {
			t.Fatalf("tampered password error = %v", err)
		}
	}
	if _, err := r.decryptPassword([]byte("short")); !errors.Is(err, ErrStorage) {
		t.Fatal("truncated ciphertext must be rejected")
	}
	other, err := NewRepository(r.db, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := r.encryptPassword("private-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.decryptPassword(encrypted); !errors.Is(err, ErrStorage) {
		t.Fatal("wrong key must not decrypt credentials")
	}
}

func TestInitialize(t *testing.T) {
	r := testRepository(t,
		databaseStep{statement: "CREATE TABLE IF NOT EXISTS managed_databases", exec: true},
		databaseStep{statement: "CREATE TABLE IF NOT EXISTS managed_databases", exec: true},
	)
	for range 2 {
		if err := r.Initialize(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	r = testRepository(t, databaseStep{statement: "CREATE TABLE", exec: true, err: errors.New("private-password")})
	assertStorageError(t, r.Initialize(context.Background()), ErrStorage)
}

func TestCreate(t *testing.T) {
	step := metadataStep("INSERT INTO managed_databases", "Main")
	var storedPassword []byte
	step.check = func(t *testing.T, args []driver.NamedValue) {
		if len(args) != 7 {
			t.Fatalf("got %d arguments, want 7", len(args))
		}
		if args[0].Value != "Main" || args[1].Value != "localhost" || args[2].Value != int64(5432) || args[6].Value != "require" {
			t.Error("normalized registration was not persisted")
		}
		var ok bool
		storedPassword, ok = args[5].Value.([]byte)
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
	password, err := r.decryptPassword(storedPassword)
	if err != nil || password != validConfig().Password {
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
	assertStorageError(t, err, ErrInvalidInput)
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"duplicate", fmt.Errorf("wrapped: %w", stateError("23505")), ErrConflict},
		{"storage", errors.New("private-password"), ErrStorage},
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
		{"missing", databaseStep{statement: "SELECT", columns: step.columns}, ErrNotFound},
		{"query failure", databaseStep{statement: "SELECT", err: errors.New("private-password")}, ErrStorage},
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
	badRow.rows[0][3] = "not-an-integer"
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
			assertStorageError(t, err, ErrStorage)
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
		if len(args) != 8 || args[0].Value != "Renamed" || args[7].Value != testDatabaseID {
			t.Fatal("update did not preserve UUID or normalize name")
		}
		encrypted, _ = args[5].Value.([]byte)
	}
	r := testRepository(t, step)
	config := validConfig()
	config.Password = ""
	got, err := r.Update(context.Background(), testDatabaseID, " Renamed ", config)
	if err != nil || got.ID != testDatabaseID || got.Name != "Renamed" || !got.CreatedAt.Equal(testCreatedAt) {
		t.Fatalf("unexpected Update result: %+v, %v", got, err)
	}
	password, err := r.decryptPassword(encrypted)
	if err != nil || password != "" {
		t.Fatal("empty password must be persisted as an explicit replacement")
	}
}

func TestUpdateErrors(t *testing.T) {
	r := testRepository(t)
	_, err := r.Update(context.Background(), testDatabaseID, "", validConfig())
	assertStorageError(t, err, ErrInvalidInput)
	for _, test := range []struct {
		name string
		step databaseStep
		want error
	}{
		{"missing", databaseStep{statement: "UPDATE", columns: metadataStep("", "").columns}, ErrNotFound},
		{"duplicate", databaseStep{statement: "UPDATE", err: stateError("23505")}, ErrConflict},
		{"query failure", databaseStep{statement: "UPDATE", err: errors.New("private-password")}, ErrStorage},
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
		{"missing", driver.RowsAffected(0), nil, ErrNotFound},
		{"storage failure", nil, errors.New("private-password"), ErrStorage},
		{"rows affected failure", badResult{}, nil, ErrStorage},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRepository(t, databaseStep{statement: "DELETE FROM managed_databases", exec: true,
				result: test.result, err: test.err, check: checkID})
			assertStorageError(t, r.Delete(context.Background(), testDatabaseID), test.want)
		})
	}
}

func TestGetConnectionConfig(t *testing.T) {
	r := testRepository(t)
	encrypted, err := r.encryptPassword(" private-password ")
	if err != nil {
		t.Fatal(err)
	}
	step := databaseStep{
		statement: "SELECT host, port, database_name, username, password_ciphertext, ssl_mode",
		columns:   []string{"host", "port", "database_name", "username", "password_ciphertext", "ssl_mode"},
		rows:      [][]driver.Value{{"localhost", int64(5433), "app", "reader", encrypted, "verify-full"}},
		check:     checkID,
	}
	r = testRepository(t, step)
	got, err := r.GetConnectionConfig(context.Background(), testDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "localhost" || got.Port != 5433 || got.DatabaseName != "app" ||
		got.Username != "reader" || got.Password != " private-password " || got.SSLMode != "verify-full" {
		t.Fatal("stored connection configuration was not recovered correctly")
	}
	for _, test := range []struct {
		name string
		step databaseStep
		want error
	}{
		{"missing", databaseStep{statement: "SELECT", columns: step.columns}, ErrNotFound},
		{"query failure", databaseStep{statement: "SELECT", err: errors.New("private-password")}, ErrStorage},
		{"corrupted credentials", databaseStep{statement: "SELECT", columns: step.columns,
			rows: [][]driver.Value{{"localhost", int64(5432), "app", "reader", []byte("corrupt"), "require"}}}, ErrStorage},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRepository(t, test.step)
			got, err := r.GetConnectionConfig(context.Background(), testDatabaseID)
			assertStorageError(t, err, test.want)
			if got != (ConnectionConfig{}) {
				t.Fatal("failed credential lookup must not return partial configuration")
			}
		})
	}
}

func TestMalformedIDsDoNotQueryDatabase(t *testing.T) {
	r := testRepository(t)
	ctx := context.Background()
	_, err := r.Get(ctx, "bad-id")
	assertStorageError(t, err, ErrNotFound)
	_, err = r.Update(ctx, "bad-id", "Main", validConfig())
	assertStorageError(t, err, ErrNotFound)
	assertStorageError(t, r.Delete(ctx, "bad-id"), ErrNotFound)
	_, err = r.GetConnectionConfig(ctx, "bad-id")
	assertStorageError(t, err, ErrNotFound)
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
