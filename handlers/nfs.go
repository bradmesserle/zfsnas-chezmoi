package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"

	"github.com/gorilla/mux"
)

// HandleGetNFSSessions returns active NFS mounts grouped by export path.
func HandleGetNFSSessions(w http.ResponseWriter, r *http.Request) {
	if !system.IsNFSInstalled() {
		jsonOK(w, map[string][]system.ShareClient{})
		return
	}
	exports, _ := system.ListNFSShares(config.Dir())
	jsonOK(w, system.GetNFSSessions(exports))
}

// HandleNFSStatus returns NFS installation and service status.
func HandleNFSStatus(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]interface{}{
		"installed": system.IsNFSInstalled(),
		"status":    system.NFSStatus(),
	})
}

// HandleNFSService starts, stops, or restarts the nfs-server systemd unit.
func HandleNFSService(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := system.ControlNFS(req.Action); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{
		"status": system.NFSStatus(),
	})
}

// HandleListNFSShares returns all configured NFS exports.
func HandleListNFSShares(w http.ResponseWriter, r *http.Request) {
	shares, err := system.ListNFSShares(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, shares)
}

// HandleCreateNFSShare adds a new NFS export.
func HandleCreateNFSShare(w http.ResponseWriter, r *http.Request) {
	var req system.NFSShare
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	req.Client = strings.TrimSpace(req.Client)
	if req.Path == "" {
		jsonErr(w, http.StatusBadRequest, "path is required")
		return
	}
	if req.Client == "" {
		req.Client = "*"
	}
	req.ID = newID()

	shares, err := system.ListNFSShares(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	shares = append(shares, req)
	if err := system.SaveNFSShares(config.Dir(), shares); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = system.ChmodSharePath(req.Path)

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionCreateNFSShare,
		Target: req.Path,
		Result: audit.ResultOK,
	})
	jsonCreated(w, req)
}

// HandleUpdateNFSShare replaces an existing NFS export by ID.
func HandleUpdateNFSShare(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	var req system.NFSShare
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.ID = id
	req.Path = strings.TrimSpace(req.Path)
	req.Client = strings.TrimSpace(req.Client)
	if req.Client == "" {
		req.Client = "*"
	}

	shares, err := system.ListNFSShares(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	found := false
	for i, s := range shares {
		if s.ID == id {
			shares[i] = req
			found = true
			break
		}
	}
	if !found {
		jsonErr(w, http.StatusNotFound, "share not found")
		return
	}
	if err := system.SaveNFSShares(config.Dir(), shares); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:    sess.Username,
		Role:    sess.Role,
		Action:  audit.ActionUpdateNFSShare,
		Target:  req.Path,
		Result:  audit.ResultOK,
		Details: "updated",
	})
	jsonOK(w, req)
}

// HandleDeleteNFSShare removes an NFS export by ID.
func HandleDeleteNFSShare(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	shares, err := system.ListNFSShares(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	newShares := shares[:0]
	var target string
	for _, s := range shares {
		if s.ID == id {
			target = s.Path
			continue
		}
		newShares = append(newShares, s)
	}
	if target == "" {
		jsonErr(w, http.StatusNotFound, "share not found")
		return
	}
	if err := system.SaveNFSShares(config.Dir(), newShares); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionDeleteNFSShare,
		Target: target,
		Result: audit.ResultOK,
	})
	jsonOK(w, map[string]string{"message": "NFS share removed"})
}
