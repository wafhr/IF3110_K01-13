package database

import "context"

// DeleteWithAccessCleanup removes a registration, its grants, and affected
// sessions atomically. It requires the access-control schema to be initialized.
// The target database itself is never modified. Administrative callers should
// use this method instead of the low-level registry-only Delete operation.
func (r *Repository) DeleteWithAccessCleanup(ctx context.Context, id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return storageError("begin database deletion", err)
	}
	defer tx.Rollback()
	var deletedID string
	if err := tx.QueryRowContext(ctx, `DELETE FROM managed_databases WHERE id = $1 RETURNING id::text`, id).Scan(&deletedID); err != nil {
		return storageError("delete database", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM access_sessions WHERE user_id IN (
			SELECT m.user_id FROM access_group_members m
			JOIN access_group_database_permissions p ON p.group_id = m.group_id
			WHERE p.database_id = $1
		)`, id); err != nil {
		return storageError("revoke database sessions", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM access_group_database_permissions WHERE database_id = $1`, id); err != nil {
		return storageError("remove database grants", err)
	}
	if err := tx.Commit(); err != nil {
		return storageError("commit database deletion", err)
	}
	return nil
}
