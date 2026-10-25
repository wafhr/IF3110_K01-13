package database

import "context"

// ListAccessible resolves group grants in one query. View + Edit wins when
// several groups grant access; Platform Admin status does not bypass grants.
func (r *Repository) ListAccessible(ctx context.Context, userID string) ([]AccessibleDatabase, error) {
	if !validID(userID) {
		return nil, ErrNotFound
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT d.id::text, d.name, d.dbms, d.database_name,
		       CASE WHEN bool_or(p.permission = 'view_edit') THEN 'view_edit' ELSE 'view_only' END
		FROM managed_databases d
		JOIN access_group_database_permissions p ON p.database_id = d.id
		JOIN access_group_members m ON m.group_id = p.group_id
		JOIN access_users u ON u.id = m.user_id AND u.is_active
		WHERE m.user_id = $1
		GROUP BY d.id, d.name, d.dbms, d.database_name
		ORDER BY d.name, d.id`, userID)
	if err != nil {
		return nil, storageError("list accessible databases", err)
	}
	defer rows.Close()
	databases := make([]AccessibleDatabase, 0)
	for rows.Next() {
		var item AccessibleDatabase
		if err := rows.Scan(&item.ID, &item.Name, &item.DBMS, &item.DatabaseName, &item.Permission); err != nil {
			return nil, storageError("read accessible database", err)
		}
		databases = append(databases, item)
	}
	if err := rows.Err(); err != nil {
		return nil, storageError("list accessible databases", err)
	}
	return databases, nil
}
