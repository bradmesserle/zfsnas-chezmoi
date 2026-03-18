package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
	"zfsnas/internal/alerts"
	"zfsnas/internal/audit"
	"zfsnas/system"
)

// smtpPasswordMask is the sentinel returned by GET and recognised by PUT to mean
// "password already set — keep existing value unchanged".
const smtpPasswordMask = "••••••••"

// HandleGetAlerts returns the current alert configuration with the SMTP password masked.
func HandleGetAlerts(w http.ResponseWriter, r *http.Request) {
	cfg, err := alerts.Load()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to load alert config")
		return
	}
	// Never send the real password over the wire — replace with a fixed mask.
	if cfg.SMTP.Password != "" {
		cfg.SMTP.Password = smtpPasswordMask
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
	// If the UI sent back the mask unchanged, preserve the existing password.
	if cfg.SMTP.Password == smtpPasswordMask {
		existing, err := alerts.Load()
		if err == nil && existing.SMTP.Password != "" {
			// Re-encrypt the plaintext we just decrypted so Save() stores it properly.
			cfg.SMTP.Password = existing.SMTP.Password
		} else {
			cfg.SMTP.Password = ""
		}
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

const alertDedup = 24 * time.Hour

// isSmartUnsupported returns true when a disk has no genuine SMART failure —
// either SMART is not supported by the hardware, smartctl is unavailable,
// or the SMART cache has not been populated yet (zero-value SmartMsg).
func isSmartUnsupported(msg string) bool {
	switch msg {
	case "", "Not supported", "smartctl unavailable", "parse error":
		return true
	}
	return false
}

// isBadPoolHealth returns true for pool states that represent a problem.
func isBadPoolHealth(h string) bool {
	switch h {
	case "DEGRADED", "FAULTED", "SUSPENDED", "UNAVAIL", "REMOVED":
		return true
	}
	return false
}

// ── Shared health-event state ─────────────────────────────────────────────────
//
// These maps are shared between the background poller and pool operation handlers
// so that recovery events are written immediately when a fix action completes,
// not only at the next 5-minute poll cycle. The shared state also ensures the
// poller does not write a duplicate event for a transition already recorded by a
// handler.

var (
	healthEvMu           sync.Mutex
	healthEvPoolStates   = map[string]string{}   // pool name → last known health
	healthEvMemberStates = map[string][]string{} // pool name → last known per-member statuses
	smtpLastPoolHealths  = map[string]string{}   // pool name → health at last SMTP send
)

// LogPoolHealthEvents checks whether a pool's health or member disk statuses
// have changed since the last call and writes audit entries for every transition.
// It is safe to call from multiple goroutines and is idempotent for the same
// state — calling it twice with an unchanged pool writes nothing.
func LogPoolHealthEvents(pool *system.Pool) {
	if pool == nil {
		return
	}
	healthEvMu.Lock()
	defer healthEvMu.Unlock()

	// ── Pool-level health ─────────────────────────────────────────────────────
	prevHealth := healthEvPoolStates[pool.Name]
	currHealth := pool.Health
	currBad    := isBadPoolHealth(currHealth)
	prevBad    := isBadPoolHealth(prevHealth)

	if currBad && prevHealth != currHealth {
		// New problem, or health worsened (e.g. DEGRADED → FAULTED).
		audit.Log(audit.Entry{
			User:    "system",
			Role:    "system",
			Action:  audit.ActionPoolProblem,
			Target:  pool.Name,
			Result:  audit.ResultError,
			Details: "pool health: " + currHealth,
		})
	} else if !currBad && prevBad {
		// Pool has recovered.
		audit.Log(audit.Entry{
			User:    "system",
			Role:    "system",
			Action:  audit.ActionPoolRecovered,
			Target:  pool.Name,
			Result:  audit.ResultOK,
			Details: "pool health restored: " + currHealth,
		})
	}
	healthEvPoolStates[pool.Name] = currHealth

	// ── Per-member disk status ────────────────────────────────────────────────
	prevStatuses := healthEvMemberStates[pool.Name]
	for i, currStatus := range pool.MemberStatuses {
		var prevStatus string
		if i < len(prevStatuses) {
			prevStatus = prevStatuses[i]
		}
		dev := ""
		if i < len(pool.MemberDevices) && pool.MemberDevices[i] != "" {
			dev = pool.MemberDevices[i]
		} else if i < len(pool.Members) {
			dev = pool.Members[i]
		}
		currDiskBad := currStatus != "ONLINE"
		prevDiskBad := prevStatus != "ONLINE" && prevStatus != "" // empty = first run

		if currDiskBad && prevStatus != currStatus {
			audit.Log(audit.Entry{
				User:    "system",
				Role:    "system",
				Action:  audit.ActionDiskProblem,
				Target:  pool.Name,
				Result:  audit.ResultError,
				Details: dev + " status: " + currStatus,
			})
		} else if !currDiskBad && prevDiskBad {
			audit.Log(audit.Entry{
				User:    "system",
				Role:    "system",
				Action:  audit.ActionDiskRecovered,
				Target:  pool.Name,
				Result:  audit.ResultOK,
				Details: dev + " recovered: ONLINE",
			})
		}
	}
	healthEvMemberStates[pool.Name] = append([]string{}, pool.MemberStatuses...)
}

// StartHealthPoller launches a background goroutine that checks pool health
// and disk wearout every 5 minutes, fires SMTP alert emails on threshold
// breaches, and calls LogPoolHealthEvents for each pool to write audit entries.
func StartHealthPoller(configDir string) {
	go func() {
		lastWearoutAlerted := map[string]time.Time{}
		lastSmartAlerted   := map[string]time.Time{}

		// Brief delay so the server is fully up before the first check.
		time.Sleep(30 * time.Second)
		runHealthCheck(lastWearoutAlerted, lastSmartAlerted, configDir)

		tick := time.NewTicker(5 * time.Minute)
		defer tick.Stop()
		for range tick.C {
			runHealthCheck(lastWearoutAlerted, lastSmartAlerted, configDir)
		}
	}()
}

func runHealthCheck(
	lastWearoutAlerted map[string]time.Time,
	lastSmartAlerted   map[string]time.Time,
	configDir string,
) {
	cfg, err := alerts.Load()
	if err != nil {
		log.Printf("alerts: failed to load config: %v", err)
		return
	}

	// --- Pool health + disk member status ---
	pools, err := system.GetAllPools()
	if err == nil {
		for _, pool := range pools {
			// Capture health before the LogPoolHealthEvents call for SMTP dedup.
			healthEvMu.Lock()
			prevSmtp := smtpLastPoolHealths[pool.Name]
			healthEvMu.Unlock()

			LogPoolHealthEvents(pool)

			// SMTP alert for pool degradation — only when health changed to bad.
			if cfg.Events.PoolDegraded && isBadPoolHealth(pool.Health) && prevSmtp != pool.Health {
				healthEvMu.Lock()
				smtpLastPoolHealths[pool.Name] = pool.Health
				healthEvMu.Unlock()
				go func(h, name string) {
					if err := alerts.Send(
						"Pool health: "+h,
						"Pool Health Degraded",
						fmt.Sprintf("Pool '%s' is in state: %s", name, h),
					); err != nil {
						log.Printf("alerts: send failed: %v", err)
					}
				}(pool.Health, pool.Name)
			}
		}
	}

	// --- Disk SMART + wearout ---
	if cfg.Events.SmartError || cfg.Events.WearoutExceeded {
		disks, err := system.ListDisks(configDir)
		if err != nil {
			return
		}
		now := time.Now()
		for _, d := range disks {
			// SMART error — skip if SMART is unsupported or cache not ready,
			// and suppress repeated alerts for the same disk within 24 hours.
			if cfg.Events.SmartError && !d.SmartOK && !isSmartUnsupported(d.SmartMsg) {
				if last, seen := lastSmartAlerted[d.Name]; !seen || now.Sub(last) >= alertDedup {
					lastSmartAlerted[d.Name] = now
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
				}
			} else if d.SmartOK {
				delete(lastSmartAlerted, d.Name)
			}

			// Wearout threshold — suppress repeated alerts within 24 hours.
			if cfg.Events.WearoutExceeded && cfg.Events.WearoutThresholdPct > 0 && d.WearoutPct != nil {
				thr := cfg.Events.WearoutThresholdPct
				if *d.WearoutPct >= thr {
					if last, seen := lastWearoutAlerted[d.Name]; !seen || now.Sub(last) >= alertDedup {
						lastWearoutAlerted[d.Name] = now
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
					}
				} else {
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
