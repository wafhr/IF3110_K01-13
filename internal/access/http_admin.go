package access

import (
	"errors"
	"net/http"
	"strings"

	"github.com/wafhr/IF3110_K01-13/internal/httpjson"
)

func (h *HTTPHandler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.service.ListUsers(r.Context())
	if err != nil {
		internalError(w, r.Context(), "list users")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"users": users})
}

func (h *HTTPHandler) createUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		PlatformAdmin bool   `json:"platform_admin"`
	}
	if !httpjson.Decode(w, r, &input) {
		return
	}
	user, err := h.service.CreateUser(r.Context(), input.Username, input.Password, input.PlatformAdmin)
	if errors.Is(err, ErrConflict) {
		httpjson.Error(w, http.StatusConflict, "username already exists")
		return
	}
	if err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"user": user})
}

func (h *HTTPHandler) updateUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Active        *bool `json:"active"`
		PlatformAdmin *bool `json:"platform_admin"`
	}
	if !httpjson.Decode(w, r, &input) {
		return
	}
	if input.Active == nil || input.PlatformAdmin == nil {
		httpjson.Error(w, http.StatusBadRequest, "active and platform_admin are required")
		return
	}
	actor := currentUser(r.Context())
	if strings.EqualFold(actor.ID, r.PathValue("userID")) && (!*input.Active || !*input.PlatformAdmin) {
		httpjson.Error(w, http.StatusConflict, "cannot disable or demote the current admin")
		return
	}
	if err := h.service.UpdateUser(r.Context(), r.PathValue("userID"), *input.Active, *input.PlatformAdmin); err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.service.ListGroups(r.Context())
	if err != nil {
		internalError(w, r.Context(), "list groups")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"groups": groups})
}

func (h *HTTPHandler) createGroup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !httpjson.Decode(w, r, &input) {
		return
	}
	group, err := h.service.CreateGroup(r.Context(), input.Name)
	if errors.Is(err, ErrConflict) {
		httpjson.Error(w, http.StatusConflict, "group name already exists")
		return
	}
	if err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"group": group})
}

func (h *HTTPHandler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := h.service.DeleteGroup(r.Context(), r.PathValue("groupID")); err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) setGroupLeader(w http.ResponseWriter, r *http.Request) {
	var input struct {
		UserID string `json:"user_id"`
	}
	if !httpjson.Decode(w, r, &input) {
		return
	}
	if err := h.service.SetGroupLeader(r.Context(), r.PathValue("groupID"), input.UserID); err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) listGroupMembers(w http.ResponseWriter, r *http.Request) {
	members, err := h.service.ListGroupMembers(r.Context(), r.PathValue("groupID"))
	if err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"members": members})
}

func (h *HTTPHandler) listDatabaseGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := h.service.ListDatabaseGrants(r.Context(), r.PathValue("groupID"))
	if err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"databases": grants})
}

func (h *HTTPHandler) addMember(w http.ResponseWriter, r *http.Request) {
	if err := h.service.SetMembership(r.Context(), r.PathValue("groupID"), r.PathValue("userID"), true); err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) removeMember(w http.ResponseWriter, r *http.Request) {
	if err := h.service.SetMembership(r.Context(), r.PathValue("groupID"), r.PathValue("userID"), false); err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) setPermission(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Permission Permission `json:"permission"`
	}
	if !httpjson.Decode(w, r, &input) {
		return
	}
	if err := h.service.SetDatabasePermission(r.Context(), r.PathValue("groupID"), r.PathValue("databaseID"), input.Permission); err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) deletePermission(w http.ResponseWriter, r *http.Request) {
	if err := h.service.DeleteDatabasePermission(r.Context(), r.PathValue("groupID"), r.PathValue("databaseID")); err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) databaseAccess(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r.Context())
	permission, err := h.service.EffectivePermission(r.Context(), user.ID, r.PathValue("databaseID"))
	if err != nil {
		writeServiceError(w, r.Context(), err)
		return
	}
	if permission == "" {
		httpjson.Error(w, http.StatusForbidden, "no database access")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]Permission{"permission": permission})
}
