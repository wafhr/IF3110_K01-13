package app

import (
	"context"
	"database/sql"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb/drivers"
)

// NewMigrationService builds the migration service the same way for the server
// and the worker, so a submission and its execution are checked by the same
// permission and approval providers. The workflow module's approval provider
// is added to the dependencies here. The worker passes a nil queue because it
// only executes jobs.
func NewMigrationService(platform *sql.DB, registry *database.Repository, connector *drivers.Connector,
	definitions *migration.Repository, queue migration.ActionQueue) (*migration.Service, migration.TargetOpener) {
	openTarget := func(ctx context.Context, id string) (*sql.DB, database.DBMS, error) {
		connection, err := registry.GetConnectionConfig(ctx, id)
		if err != nil {
			return nil, "", err
		}
		target, err := connector.Open(ctx, connection)
		return target, connection.DBMS, err
	}
	service := migration.NewServiceWithExecution(openTarget, definitions, migration.ExecutionDependencies{
		Authorizer: access.NewService(platform),
		Queue:      queue,
	})
	return service, openTarget
}
