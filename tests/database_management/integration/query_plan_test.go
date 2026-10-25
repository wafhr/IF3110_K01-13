//go:build integration

package integration_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/database"
)

func TestQueryPlanPreviewIntegration(t *testing.T) {
	f := newFixture(t)
	admin := f.login(t, "admin", adminPassword)
	var registered struct {
		Database database.Database `json:"database"`
	}
	decode(t, admin.do(t, "POST", "/admin/databases", registration("Plan preview target", f.settings), 201), &registered)
	var user struct {
		User access.User `json:"user"`
	}
	decode(t, admin.do(t, "POST", "/admin/users", map[string]string{"username": "editor", "password": memberPassword}, 201), &user)
	var group struct {
		Group access.Group `json:"group"`
	}
	decode(t, admin.do(t, "POST", "/admin/groups", map[string]string{"name": "Plan editors"}, 201), &group)
	admin.do(t, "PUT", "/admin/groups/"+group.Group.ID+"/members/"+user.User.ID, nil, 204)
	admin.do(t, "PUT", "/admin/groups/"+group.Group.ID+"/databases/"+registered.Database.ID, map[string]string{"permission": "view_edit"}, 204)
	execFixture(t, f.target, "GRANT UPDATE ON public.customers TO "+pgx.Identifier{f.createdRole}.Sanitize())
	editor := f.login(t, "editor", memberPassword)
	endpoint := "/databases/" + registered.Database.ID + "/migrations/explain"

	for _, analyze := range []bool{false, true} {
		input := map[string]any{"sql": "UPDATE public.customers SET name = 'preview-only' WHERE id = 1"}
		mode := "explain"
		if analyze {
			input["analyze"], mode = true, "explain_analyze"
		}
		var response struct {
			QueryPlan struct {
				Mode       string `json:"mode"`
				RolledBack bool   `json:"rolled_back"`
				Plan       []struct {
					Plan struct {
						NodeType        string   `json:"Node Type"`
						Operation       string   `json:"Operation"`
						ActualRows      *float64 `json:"Actual Rows"`
						ActualTotalTime *float64 `json:"Actual Total Time"`
					} `json:"Plan"`
					ExecutionTime *float64 `json:"Execution Time"`
				} `json:"plan"`
			} `json:"query_plan"`
		}
		decode(t, editor.do(t, "POST", endpoint, input, 200), &response)
		result := response.QueryPlan
		if result.Mode != mode || !result.RolledBack || len(result.Plan) != 1 {
			t.Fatalf("unexpected %s response: %+v", mode, result)
		}
		plan := result.Plan[0]
		if plan.Plan.NodeType != "ModifyTable" || plan.Plan.Operation != "Update" {
			t.Fatalf("%s did not return an UPDATE plan: %+v", mode, plan)
		}
		if analyze {
			if plan.Plan.ActualRows == nil || plan.Plan.ActualTotalTime == nil || plan.ExecutionTime == nil {
				t.Fatal("ANALYZE did not return actual execution statistics")
			}
		} else if plan.Plan.ActualRows != nil || plan.Plan.ActualTotalTime != nil || plan.ExecutionTime != nil {
			t.Fatal("plain EXPLAIN unexpectedly returned actual execution statistics")
		}
		assertCount(t, f.target, `SELECT count(*) FROM public.customers WHERE id = 1 AND name = 'First customer'`, 1)
		assertCount(t, f.target, `SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname = 'public' AND tablename = 'platform_migrations'`, 0)
	}
}
