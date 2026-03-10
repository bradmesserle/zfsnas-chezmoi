package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// HandleScrubStatus returns the current scrub state for the active pool.
func HandleScrubStatus(w http.ResponseWriter, r *http.Request) {
	pool, err := system.GetPool()
	if err != nil || pool == nil {
		jsonErr(w, http.StatusNotFound, "no pool available")
		return
	}
	info, err := system.GetScrubStatus(pool.Name)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, info)
}

// HandleStartScrub starts a scrub on the active pool (admin only).
func HandleStartScrub(w http.ResponseWriter, r *http.Request) {
	pool, err := system.GetPool()
	if err != nil || pool == nil {
		jsonErr(w, http.StatusNotFound, "no pool available")
		return
	}
	if err := system.StartScrub(pool.Name); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"message": "scrub started"})
}

// HandleStopScrub cancels a running scrub (admin only).
func HandleStopScrub(w http.ResponseWriter, r *http.Request) {
	pool, err := system.GetPool()
	if err != nil || pool == nil {
		jsonErr(w, http.StatusNotFound, "no pool available")
		return
	}
	if err := system.StopScrub(pool.Name); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{"message": "scrub stopped"})
}

// HandleGetScrubSchedule returns whether the weekly auto-scrub is enabled.
func HandleGetScrubSchedule(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]bool{"weekly_scrub": appCfg.WeeklyScrub})
	}
}

// HandleSetScrubSchedule toggles the weekly auto-scrub setting (admin only).
func HandleSetScrubSchedule(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			WeeklyScrub bool `json:"weekly_scrub"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		appCfg.WeeklyScrub = req.WeeklyScrub
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to save config")
			return
		}
		jsonOK(w, map[string]bool{"weekly_scrub": appCfg.WeeklyScrub})
	}
}

// StartScrubScheduler runs a goroutine that fires a scrub every Sunday at 02:00
// when WeeklyScrub is enabled in appCfg.
func StartScrubScheduler(appCfg *config.AppConfig) {
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for now := range tick.C {
			if !appCfg.WeeklyScrub {
				continue
			}
			if now.Weekday() == time.Sunday && now.Hour() == 2 && now.Minute() == 0 {
				pool, err := system.GetPool()
				if err != nil || pool == nil {
					continue
				}
				log.Printf("[scrub] starting weekly auto-scrub on pool %s", pool.Name)
				if err := system.StartScrub(pool.Name); err != nil {
					log.Printf("[scrub] weekly scrub failed: %v", err)
				}
			}
		}
	}()
}
