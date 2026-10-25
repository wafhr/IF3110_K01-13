package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
)

//go:embed schema.sql
var registrySchema string

const metadataColumns = `id::text, name, dbms, host, port, database_name, username, ssl_mode, created_at, updated_at`

// Repository persists target configurations in the platform database. Callers
// must apply access control before exposing these operations through HTTP.
type Repository struct {
	db     *sql.DB
	cipher cipher.AEAD
}

// NewRepository requires a base64-encoded 32-byte AES key. The same key must be
// preserved across restarts to decrypt previously saved connection passwords.
func NewRepository(db *sql.DB, encryptionKey string) (*Repository, error) {
	if db == nil {
		return nil, errors.New("database registry requires a platform database")
	}
	key, err := base64.StdEncoding.DecodeString(encryptionKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("DATABASE_ENCRYPTION_KEY must be a base64-encoded 32-byte key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("initialize database credential encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("initialize database credential encryption")
	}
	return &Repository{db: db, cipher: aead}, nil
}

// Initialize is idempotent and does not connect to any target database.
func (r *Repository) Initialize(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, registrySchema); err != nil {
		return storageError("initialize registry", err)
	}
	if _, err := r.db.ExecContext(ctx, `ALTER TABLE managed_databases ADD COLUMN IF NOT EXISTS dbms TEXT NOT NULL DEFAULT 'postgres' CHECK (dbms IN ('postgres', 'mysql'))`); err != nil {
		return storageError("upgrade database registry", err)
	}
	if _, err := r.db.ExecContext(ctx, `DO $$ BEGIN
		ALTER TABLE managed_databases DROP CONSTRAINT IF EXISTS managed_databases_database_name_check;
		ALTER TABLE managed_databases ADD CONSTRAINT managed_databases_database_name_check CHECK (
			(dbms = 'postgres' AND char_length(database_name) BETWEEN 1 AND 63) OR
			(dbms = 'mysql' AND char_length(database_name) BETWEEN 1 AND 64));
	END $$`); err != nil {
		return storageError("upgrade database name constraint", err)
	}
	return nil
}

func (r *Repository) Create(ctx context.Context, name string, config ConnectionConfig) (Database, error) {
	name, config, err := normalizeRegistration(name, config)
	if err != nil {
		return Database{}, err
	}
	password, err := r.encryptPassword(config.Password)
	if err != nil {
		return Database{}, err
	}
	database, err := scanDatabase(r.db.QueryRowContext(ctx, `
		INSERT INTO managed_databases (name, dbms, host, port, database_name, username, password_ciphertext, ssl_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+metadataColumns,
		name, config.DBMS, config.Host, config.Port, config.DatabaseName, config.Username, password, config.SSLMode))
	if err != nil {
		return Database{}, storageError("create database", err)
	}
	return database, nil
}

func (r *Repository) Get(ctx context.Context, id string) (Database, error) {
	if !validID(id) {
		return Database{}, ErrNotFound
	}
	database, err := scanDatabase(r.db.QueryRowContext(ctx, `SELECT `+metadataColumns+` FROM managed_databases WHERE id = $1`, id))
	if err != nil {
		return Database{}, storageError("get database", err)
	}
	return database, nil
}

// List returns all registered databases. It is intended for platform management;
// a user-facing database list must additionally filter by group-derived access.
func (r *Repository) List(ctx context.Context) ([]Database, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+metadataColumns+` FROM managed_databases ORDER BY name, id`)
	if err != nil {
		return nil, storageError("list databases", err)
	}
	defer rows.Close()
	databases := make([]Database, 0)
	for rows.Next() {
		database, err := scanDatabase(rows)
		if err != nil {
			return nil, storageError("read database metadata", err)
		}
		databases = append(databases, database)
	}
	if err := rows.Err(); err != nil {
		return nil, storageError("list databases", err)
	}
	return databases, nil
}

// Update replaces all connection fields, including the password. An empty
// password intentionally replaces the old one; it does not mean "unchanged".
func (r *Repository) Update(ctx context.Context, id, name string, config ConnectionConfig) (Database, error) {
	if !validID(id) {
		return Database{}, ErrNotFound
	}
	name, config, err := normalizeRegistration(name, config)
	if err != nil {
		return Database{}, err
	}
	password, err := r.encryptPassword(config.Password)
	if err != nil {
		return Database{}, err
	}
	database, err := scanDatabase(r.db.QueryRowContext(ctx, `
		UPDATE managed_databases SET name = $1, dbms = $2, host = $3, port = $4, database_name = $5,
		username = $6, password_ciphertext = $7, ssl_mode = $8, updated_at = now()
		WHERE id = $9 RETURNING `+metadataColumns,
		name, config.DBMS, config.Host, config.Port, config.DatabaseName, config.Username, password, config.SSLMode, id))
	if err != nil {
		return Database{}, storageError("update database", err)
	}
	return database, nil
}

// Delete removes a registration only; it never drops the actual target database.
func (r *Repository) Delete(ctx context.Context, id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM managed_databases WHERE id = $1`, id)
	if err != nil {
		return storageError("delete database", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return storageError("delete database", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// GetConnectionConfig returns decrypted credentials for backend connectors and
// migration execution. Do not expose its result in HTTP responses or logs.
func (r *Repository) GetConnectionConfig(ctx context.Context, id string) (ConnectionConfig, error) {
	if !validID(id) {
		return ConnectionConfig{}, ErrNotFound
	}
	var config ConnectionConfig
	var encrypted []byte
	err := r.db.QueryRowContext(ctx, `
		SELECT dbms, host, port, database_name, username, password_ciphertext, ssl_mode
		FROM managed_databases WHERE id = $1`, id).
		Scan(&config.DBMS, &config.Host, &config.Port, &config.DatabaseName, &config.Username, &encrypted, &config.SSLMode)
	if err != nil {
		return ConnectionConfig{}, storageError("get connection configuration", err)
	}
	config.Password, err = r.decryptPassword(encrypted)
	if err != nil {
		return ConnectionConfig{}, err
	}
	return config, nil
}

func scanDatabase(row interface{ Scan(...any) error }) (Database, error) {
	var database Database
	err := row.Scan(&database.ID, &database.Name, &database.DBMS, &database.Host, &database.Port,
		&database.DatabaseName, &database.Username, &database.SSLMode, &database.CreatedAt, &database.UpdatedAt)
	return database, err
}

func (r *Repository) encryptPassword(password string) ([]byte, error) {
	nonce := make([]byte, r.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: encrypt database credentials", ErrStorage)
	}
	return r.cipher.Seal(nonce, nonce, []byte(password), nil), nil
}

func (r *Repository) decryptPassword(encrypted []byte) (string, error) {
	nonceSize := r.cipher.NonceSize()
	if len(encrypted) < nonceSize+r.cipher.Overhead() {
		return "", fmt.Errorf("%w: decrypt database credentials", ErrStorage)
	}
	password, err := r.cipher.Open(nil, encrypted[:nonceSize], encrypted[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("%w: decrypt database credentials", ErrStorage)
	}
	return string(password), nil
}

// Driver errors can contain SQL values or connection details. Preserve a stable
// error category without propagating those details to responses or application logs.
func storageError(operation string, err error) error {
	for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cancellation) {
			return cancellation
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "23505" {
		return ErrConflict
	}
	return fmt.Errorf("%w: %s", ErrStorage, operation)
}
