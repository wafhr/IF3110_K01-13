package targetdb

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

type Registry interface {
	GetConnectionConfig(context.Context, string) (database.ConnectionConfig, error)
}

type Opener interface {
	Open(context.Context, database.ConnectionConfig) (*sql.DB, error)
}

type poolEntry struct {
	gate   chan struct{}
	db     *sql.DB
	config database.ConnectionConfig
}

// Manager owns its pools. Callers must authorize database access before Get or
// Check, use context-aware SQL methods, and never close a pool returned by Get.
type Manager struct {
	registry Registry
	opener   Opener
	mu       sync.Mutex
	pools    map[string]*poolEntry
	closed   bool
}

func NewManager(registry Registry, opener Opener) (*Manager, error) {
	if registry == nil || opener == nil {
		return nil, errors.New("target database manager requires a registry and connector")
	}
	return &Manager{registry: registry, opener: opener, pools: make(map[string]*poolEntry)}, nil
}

// Get loads the latest configuration and reuses its pool, replacing that pool
// when connection settings change. Connections are opened lazily and pinged on
// creation. Independent databases can establish connections concurrently.
func (m *Manager) Get(ctx context.Context, id string) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	entry := m.pools[id]
	if entry == nil {
		entry = &poolEntry{gate: make(chan struct{}, 1)}
		m.pools[id] = entry
	}
	m.mu.Unlock()
	select {
	case entry.gate <- struct{}{}:
		defer func() { <-entry.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.activeError(id, entry); err != nil {
		return nil, err
	}
	config, err := m.registry.GetConnectionConfig(ctx, id)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) || entry.db == nil {
			m.remove(id, entry)
			_ = closeEntry(entry)
		}
		return nil, ConnectionError(err)
	}
	if entry.db != nil && entry.config == config {
		return entry.db, nil
	}
	if err := closeEntry(entry); err != nil {
		return nil, err
	}
	pool, err := m.opener.Open(ctx, config)
	if err != nil {
		return nil, ConnectionError(err)
	}
	if err := m.activeError(id, entry); err != nil {
		_ = pool.Close()
		return nil, err
	}
	entry.db, entry.config = pool, config
	return pool, nil
}

// Check verifies a saved registration using a temporary pool. It always tries a
// fresh connection, even if Get has already cached a pool for the same target.
func (m *Manager) Check(ctx context.Context, id string) error {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	config, err := m.registry.GetConnectionConfig(ctx, id)
	if err != nil {
		return ConnectionError(err)
	}
	return m.Test(ctx, config)
}

// Test verifies unsaved settings without modifying the registry or pool cache.
func (m *Manager) Test(ctx context.Context, config database.ConnectionConfig) error {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	pool, err := m.opener.Open(ctx, config)
	if err != nil {
		return ConnectionError(err)
	}
	return ConnectionError(pool.Close())
}

// Invalidate closes and removes a target's cached pool after an admin update or
// deletion. Existing SQL operations are allowed to finish by database/sql.
func (m *Manager) Invalidate(id string) error {
	m.mu.Lock()
	entry := m.pools[id]
	delete(m.pools, id)
	m.mu.Unlock()
	if entry == nil {
		return nil
	}
	entry.gate <- struct{}{}
	defer func() { <-entry.gate }()
	return closeEntry(entry)
}

// Close shuts down all cached pools and rejects future Get, Check, and Test
// calls. It is safe to call more than once.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	entries := m.pools
	m.pools = make(map[string]*poolEntry)
	m.mu.Unlock()
	var result error
	for _, entry := range entries {
		entry.gate <- struct{}{}
		result = errors.Join(result, closeEntry(entry))
		<-entry.gate
	}
	return result
}

func (m *Manager) activeError(id string, entry *poolEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if m.pools[id] != entry {
		return ErrConnection
	}
	return nil
}

func (m *Manager) remove(id string, entry *poolEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pools[id] == entry {
		delete(m.pools, id)
	}
}

func closeEntry(entry *poolEntry) error {
	if entry.db == nil {
		return nil
	}
	err := entry.db.Close()
	entry.db, entry.config = nil, database.ConnectionConfig{}
	return ConnectionError(err)
}
