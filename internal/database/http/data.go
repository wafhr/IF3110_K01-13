package databasehttp

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

func (h *ViewerHandler) tableData(w http.ResponseWriter, r *http.Request) {
	options, err := pagination(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid pagination parameters")
		return
	}
	page, err := h.schemas.Data(r.Context(), r.PathValue("databaseID"), r.PathValue("schema"), r.PathValue("table"), options)
	if err != nil {
		writeViewerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": page})
}

func pagination(rawQuery string) (database.PageOptions, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return database.PageOptions{}, database.ErrInvalidInput
	}
	options := database.PageOptions{}
	for key, entries := range values {
		if len(entries) != 1 || entries[0] == "" {
			return database.PageOptions{}, database.ErrInvalidInput
		}
		switch key {
		case "limit":
			value, err := strconv.ParseInt(entries[0], 10, 32)
			if err != nil || value < 1 || value > database.MaxPageLimit {
				return database.PageOptions{}, database.ErrInvalidInput
			}
			options.Limit = int(value)
		case "offset":
			value, err := strconv.ParseInt(entries[0], 10, 64)
			if err != nil {
				return database.PageOptions{}, database.ErrInvalidInput
			}
			options.Offset = value
		default:
			return database.PageOptions{}, database.ErrInvalidInput
		}
	}
	return database.NormalizePageOptions(options)
}
