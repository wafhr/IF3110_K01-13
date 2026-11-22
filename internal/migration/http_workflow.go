package migration

import (
	"net/http"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/httpjson"
)

func (h *HTTPHandler) workflowService() *WorkflowService {
	if h.service == nil {
		return nil
	}
	workflow, _ := h.service.execution.Approvals.(*WorkflowService)
	return workflow
}

func (h *HTTPHandler) submitReview(w http.ResponseWriter, r *http.Request) {
	workflow := h.workflowService()
	if workflow == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "workflow unavailable")
		return
	}
	var input struct {
		Action Action `json:"action"`
	}
	if !httpjson.Decode(w, r, &input) {
		return
	}
	plan, err := h.service.PrepareAction(r.Context(), r.PathValue("databaseID"), r.PathValue("migrationID"), input.Action)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	result, err := workflow.SubmitReview(r.Context(), access.CurrentUser(r.Context()).ID, plan)
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]Workflow{"workflow": result})
}

func (h *HTTPHandler) getWorkflow(w http.ResponseWriter, r *http.Request) {
	workflow := h.workflowService()
	if workflow == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "workflow unavailable")
		return
	}
	result, err := workflow.GetWorkflow(r.Context(), access.CurrentUser(r.Context()).ID, r.PathValue("databaseID"), r.PathValue("workflowID"))
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]Workflow{"workflow": result})
}

func (h *HTTPHandler) getLatestWorkflow(w http.ResponseWriter, r *http.Request) {
	workflow := h.workflowService()
	if workflow == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "workflow unavailable")
		return
	}
	result, err := workflow.GetLatestWorkflow(r.Context(), access.CurrentUser(r.Context()).ID, r.PathValue("databaseID"), r.PathValue("migrationID"))
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]Workflow{"workflow": result})
}

func (h *HTTPHandler) approveWorkflow(w http.ResponseWriter, r *http.Request) {
	workflow := h.workflowService()
	if workflow == nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "workflow unavailable")
		return
	}
	result, err := workflow.Approve(r.Context(), access.CurrentUser(r.Context()).ID, r.PathValue("databaseID"), r.PathValue("workflowID"))
	if err != nil {
		migrationError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]Workflow{"workflow": result})
}
