package handlers

import (
	"net/http"
	"zfsnas/internal/audit"
)

// HandleAuditLog returns all audit log entries.
func HandleAuditLog(w http.ResponseWriter, r *http.Request) {
	entries, err := audit.Read()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to read audit log")
		return
	}
	jsonOK(w, entries)
}
