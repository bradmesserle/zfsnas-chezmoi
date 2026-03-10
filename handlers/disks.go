package handlers

import (
	"log"
	"net/http"
	"time"
	"zfsnas/internal/config"
	"zfsnas/system"
)

var (
	diskCache      []system.DiskInfo
	diskCacheStale = true
)

// HandleListDisks returns the cached disk list, refreshing SMART data if stale.
func HandleListDisks(w http.ResponseWriter, r *http.Request) {
	if diskCacheStale || diskCache == nil {
		disks, err := system.ListDisks(config.Dir())
		if err != nil {
			log.Printf("[disks] ListDisks error: %v", err)
			jsonErr(w, http.StatusInternalServerError, "failed to list disks: "+err.Error())
			return
		}
		if disks == nil {
			disks = []system.DiskInfo{}
		}
		diskCache = disks
		diskCacheStale = false
	}
	jsonOK(w, diskCache)
}

// HandleScanDisks triggers an OS-level SCSI bus rescan for newly connected disks,
// then returns the updated disk list (without a slow SMART probe).
func HandleScanDisks(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	log.Printf("[disks] bus rescan requested by %s", sess.Username)

	if err := system.RescanDisks(); err != nil {
		log.Printf("[disks] rescan error: %v", err)
		// Non-fatal — lsblk may still see new disks after a partial rescan.
	}

	diskCacheStale = true
	disks, err := system.ListDisks(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "disk reload failed: "+err.Error())
		return
	}
	diskCache = disks
	diskCacheStale = false
	jsonOK(w, diskCache)
}

// HandleRefreshDisks forces a full SMART refresh (can take several seconds).
func HandleRefreshDisks(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	log.Printf("[disks] SMART refresh requested by %s", sess.Username)

	if err := system.RefreshSMART(config.Dir()); err != nil {
		jsonErr(w, http.StatusInternalServerError, "SMART refresh failed: "+err.Error())
		return
	}
	diskCacheStale = true

	// Re-load fresh data into cache.
	disks, err := system.ListDisks(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "disk reload failed: "+err.Error())
		return
	}
	diskCache = disks
	diskCacheStale = false

	jsonOK(w, diskCache)
}

// StartDailySmartRefresh launches a background goroutine that refreshes SMART
// data once every 24 hours (and immediately on first start if cache is absent/old).
func StartDailySmartRefresh() {
	go func() {
		// Check if cached data is missing or older than 24h.
		disks, err := system.ListDisks(config.Dir())
		needsRefresh := true
		if err == nil && len(disks) > 0 {
			// All disks share the same refresh timestamp — use the first one.
			for _, d := range disks {
				if !d.UpdatedAt.IsZero() && time.Since(d.UpdatedAt) < 24*time.Hour {
					needsRefresh = false
				}
				break
			}
		}

		if needsRefresh {
			log.Println("[disks] Starting initial SMART refresh…")
			if err := system.RefreshSMART(config.Dir()); err != nil {
				log.Printf("[disks] SMART refresh error: %v", err)
			} else {
				log.Println("[disks] SMART refresh complete.")
				diskCacheStale = true
			}
		} else {
			log.Println("[disks] SMART cache is fresh — skipping initial refresh.")
		}

		// Refresh every 24h thereafter.
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			log.Println("[disks] Running daily SMART refresh…")
			if err := system.RefreshSMART(config.Dir()); err != nil {
				log.Printf("[disks] Daily SMART refresh error: %v", err)
			} else {
				log.Println("[disks] Daily SMART refresh complete.")
				diskCacheStale = true
			}
		}
	}()
}
