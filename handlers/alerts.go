package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
	"zfsnas/internal/alerts"
	"zfsnas/internal/audit"
	"zfsnas/system"
)

// HandleGetAlerts returns the current alert configuration.
func HandleGetAlerts(w http.ResponseWriter, r *http.Request) {
	cfg, err := alerts.Load()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to load alert config")
		return
	}
	jsonOK(w, cfg)
}

// HandleUpdateAlerts saves the alert configuration (admin only).
func HandleUpdateAlerts(w http.ResponseWriter, r *http.Request) {
	var cfg alerts.AlertConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := alerts.Save(&cfg); err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to save alert config")
		return
	}
	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:    sess.Username,
		Role:    sess.Role,
		Action:  audit.ActionUpdateSettings,
		Result:  audit.ResultOK,
		Details: "alert settings updated",
	})
	jsonOK(w, map[string]string{"message": "alert settings saved"})
}

// HandleTestAlert sends a test email using the current SMTP configuration.
func HandleTestAlert(w http.ResponseWriter, r *http.Request) {
	if err := alerts.Send(
		"Test Alert",
		"Manual Test",
		"This is a test alert sent from the ZFS NAS management portal.",
	); err != nil {
		jsonErr(w, http.StatusBadGateway, "failed to send test email: "+err.Error())
		return
	}
	jsonOK(w, map[string]string{"message": "test email sent"})
}

// StartHealthPoller launches a background goroutine that checks pool health
// and disk wearout every 5 minutes and fires alert emails on threshold breaches.
func StartHealthPoller(configDir string) {
	go func() {
		var lastPoolHealth string
		lastWearoutAlerted := map[string]bool{}
		lastSmartAlerted := map[string]bool{}

		// Brief delay so the server is fully up before the first check.
		time.Sleep(30 * time.Second)
		runHealthCheck(&lastPoolHealth, lastWearoutAlerted, lastSmartAlerted, configDir)

		tick := time.NewTicker(5 * time.Minute)
		defer tick.Stop()
		for range tick.C {
			runHealthCheck(&lastPoolHealth, lastWearoutAlerted, lastSmartAlerted, configDir)
		}
	}()
}

func runHealthCheck(
	lastPoolHealth *string,
	lastWearoutAlerted map[string]bool,
	lastSmartAlerted map[string]bool,
	configDir string,
) {
	cfg, err := alerts.Load()
	if err != nil {
		log.Printf("alerts: failed to load config: %v", err)
		return
	}

	// --- Pool health ---
	if cfg.Events.PoolDegraded {
		if pool, err := system.GetPool(); err == nil && pool != nil {
			health := pool.Health
			if (health == "DEGRADED" || health == "FAULTED") && *lastPoolHealth != health {
				go func(h, name string) {
					if err := alerts.Send(
						"Pool health: "+h,
						"Pool Health Degraded",
						fmt.Sprintf("Pool '%s' is in state: %s", name, h),
					); err != nil {
						log.Printf("alerts: send failed: %v", err)
					}
				}(health, pool.Name)
			}
			*lastPoolHealth = health
		}
	}

	// --- Disk SMART + wearout ---
	if cfg.Events.SmartError || cfg.Events.WearoutExceeded {
		disks, err := system.ListDisks(configDir)
		if err != nil {
			return
		}
		for _, d := range disks {
			// SMART error
			if cfg.Events.SmartError && !d.SmartOK && !lastSmartAlerted[d.Name] {
				lastSmartAlerted[d.Name] = true
				name, msg := d.Name, d.SmartMsg
				go func() {
					if err := alerts.Send(
						"SMART error on "+name,
						"SMART Error Detected",
						fmt.Sprintf("Disk %s reports a SMART error: %s", name, msg),
					); err != nil {
						log.Printf("alerts: send failed: %v", err)
					}
				}()
			} else if d.SmartOK {
				delete(lastSmartAlerted, d.Name)
			}

			// Wearout threshold
			if cfg.Events.WearoutExceeded && cfg.Events.WearoutThresholdPct > 0 && d.WearoutPct != nil {
				thr := cfg.Events.WearoutThresholdPct
				if *d.WearoutPct >= thr && !lastWearoutAlerted[d.Name] {
					lastWearoutAlerted[d.Name] = true
					name, pct := d.Name, *d.WearoutPct
					go func() {
						if err := alerts.Send(
							fmt.Sprintf("Disk wearout: %s at %d%%", name, pct),
							"Disk Wearout Threshold Exceeded",
							fmt.Sprintf("Disk %s has reached %d%% wearout (threshold: %d%%)", name, pct, thr),
						); err != nil {
							log.Printf("alerts: send failed: %v", err)
						}
					}()
				} else if *d.WearoutPct < thr {
					delete(lastWearoutAlerted, d.Name)
				}
			}
		}
	}

	// --- Failed login threshold ---
	if cfg.Events.FailedLoginAlert && cfg.Events.FailedLoginThreshold > 0 {
		count := alerts.FailedLoginCount()
		if count >= int64(cfg.Events.FailedLoginThreshold) {
			alerts.ResetFailedLogins()
			go func(n int64) {
				if err := alerts.Send(
					fmt.Sprintf("Failed logins: %d attempts", n),
					"Failed Login Threshold Exceeded",
					fmt.Sprintf("%d failed login attempts were detected since the last reset.", n),
				); err != nil {
					log.Printf("alerts: send failed: %v", err)
				}
			}(count)
		}
	}
}
