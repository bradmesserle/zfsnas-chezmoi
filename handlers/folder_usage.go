package handlers

import (
	"log"
	"net/http"
	"time"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// HandleGetFolderUsage returns the stored folder usage scan for a dataset.
// GET /api/capacity/folder-usage?dataset=tank/media
func HandleGetFolderUsage(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dataset := r.URL.Query().Get("dataset")
		if dataset == "" {
			jsonErr(w, http.StatusBadRequest, "dataset parameter required")
			return
		}
		usage, err := system.LoadFolderUsage(appCfg.ConfigDir, dataset)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if usage == nil {
			jsonErr(w, http.StatusNotFound, "no scan available for this dataset")
			return
		}
		jsonOK(w, usage)
	}
}

// HandleRefreshFolderUsage triggers a background du scan for the given dataset.
// POST /api/capacity/folder-usage/refresh?dataset=tank/media
func HandleRefreshFolderUsage(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dataset := r.URL.Query().Get("dataset")
		if dataset == "" {
			jsonErr(w, http.StatusBadRequest, "dataset parameter required")
			return
		}

		// Resolve the dataset mountpoint from a live ZFS query.
		datasets, err := system.ListAllDatasets()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to list datasets: "+err.Error())
			return
		}
		var mountpoint string
		for _, d := range datasets {
			if d.Name == dataset {
				mountpoint = d.Mountpoint
				break
			}
		}
		if mountpoint == "" || mountpoint == "none" || mountpoint == "legacy" {
			jsonErr(w, http.StatusBadRequest, "dataset has no accessible mountpoint")
			return
		}

		sess, _ := SessionFromRequest(r)
		user := ""
		if sess != nil {
			user = sess.Username
		}

		// Log scan start in the activity bar.
		audit.Log(audit.Entry{
			Timestamp: time.Now(),
			User:      user,
			Action:    audit.ActionFolderScan,
			Target:    dataset,
			Result:    audit.ResultOK,
			Details:   "folder usage scan started",
		})

		configDir := appCfg.ConfigDir

		go func() {
			_, err := system.ScanDatasetFolders(dataset, mountpoint, configDir)
			if err != nil {
				log.Printf("folder scan %s: %v", dataset, err)
				audit.Log(audit.Entry{
					Timestamp: time.Now(),
					User:      user,
					Action:    audit.ActionFolderScan,
					Target:    dataset,
					Result:    audit.ResultError,
					Details:   err.Error(),
				})
				return
			}
			audit.Log(audit.Entry{
				Timestamp: time.Now(),
				User:      user,
				Action:    audit.ActionFolderScan,
				Target:    dataset,
				Result:    audit.ResultOK,
				Details:   "folder usage scan complete",
			})
		}()

		jsonOK(w, map[string]string{"status": "scan started"})
	}
}
