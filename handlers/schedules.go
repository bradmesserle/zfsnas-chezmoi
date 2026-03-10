package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
	"zfsnas/internal/audit"
	"zfsnas/internal/scheduler"
	"zfsnas/system"

	"github.com/gorilla/mux"
)

// policyWithNext is the API response shape — policy + computed next_run.
type policyWithNext struct {
	scheduler.Policy
	NextRun time.Time `json:"next_run,omitempty"`
}

// HandleListSchedules returns all snapshot policies with their next run time.
func HandleListSchedules(w http.ResponseWriter, r *http.Request) {
	policies, err := scheduler.LoadPolicies()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	result := make([]policyWithNext, len(policies))
	for i, p := range policies {
		result[i] = policyWithNext{Policy: p, NextRun: scheduler.NextRun(p, now)}
	}
	jsonOK(w, result)
}

// HandleCreateSchedule creates a new snapshot policy.
func HandleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var p scheduler.Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if p.Dataset == "" {
		jsonErr(w, http.StatusBadRequest, "dataset is required")
		return
	}
	if p.Frequency == "" {
		p.Frequency = "daily"
	}
	if p.Retention < 1 {
		p.Retention = 7
	}
	if p.Label == "" {
		p.Label = "auto"
	}
	p.ID = newID()

	policies, err := scheduler.LoadPolicies()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	policies = append(policies, p)
	if err := scheduler.SavePolicies(policies); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:    sess.Username,
		Role:    sess.Role,
		Action:  audit.ActionCreateSchedule,
		Target:  p.Dataset,
		Result:  audit.ResultOK,
		Details: fmt.Sprintf("%s, retain %d", p.Frequency, p.Retention),
	})
	jsonCreated(w, p)
}

// HandleUpdateSchedule replaces a snapshot policy by ID.
func HandleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var p scheduler.Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	p.ID = id
	if p.Retention < 1 {
		p.Retention = 1
	}
	if p.Label == "" {
		p.Label = "auto"
	}

	policies, err := scheduler.LoadPolicies()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	found := false
	for i, existing := range policies {
		if existing.ID == id {
			p.LastRun = existing.LastRun
			p.LastStatus = existing.LastStatus
			p.LastError = existing.LastError
			policies[i] = p
			found = true
			break
		}
	}
	if !found {
		jsonErr(w, http.StatusNotFound, "schedule not found")
		return
	}
	if err := scheduler.SavePolicies(policies); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionUpdateSchedule,
		Target: p.Dataset,
		Result: audit.ResultOK,
	})
	jsonOK(w, p)
}

// HandleDeleteSchedule removes a snapshot policy by ID.
func HandleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	policies, err := scheduler.LoadPolicies()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	newPolicies := policies[:0]
	var target string
	for _, p := range policies {
		if p.ID == id {
			target = p.Dataset
			continue
		}
		newPolicies = append(newPolicies, p)
	}
	if target == "" {
		jsonErr(w, http.StatusNotFound, "schedule not found")
		return
	}
	if err := scheduler.SavePolicies(newPolicies); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionDeleteSchedule,
		Target: target,
		Result: audit.ResultOK,
	})
	jsonOK(w, map[string]string{"message": "schedule deleted"})
}

// HandleRunScheduleNow manually triggers a snapshot policy immediately.
func HandleRunScheduleNow(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	policies, err := scheduler.LoadPolicies()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	idx := -1
	for i := range policies {
		if policies[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		jsonErr(w, http.StatusNotFound, "schedule not found")
		return
	}
	if err := execScheduledSnapshot(&policies[idx]); err != nil {
		_ = scheduler.SavePolicies(policies)
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = scheduler.SavePolicies(policies)
	jsonOK(w, map[string]string{"message": "snapshot created"})
}

// StartScheduler launches a background goroutine that fires due policies every minute.
func StartScheduler() {
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for now := range tick.C {
			tickPolicies(now)
		}
	}()
}

func tickPolicies(now time.Time) {
	policies, err := scheduler.LoadPolicies()
	if err != nil {
		log.Printf("[scheduler] load error: %v", err)
		return
	}
	changed := false
	for i := range policies {
		p := &policies[i]
		if !p.Enabled || !scheduler.IsDue(*p, now) {
			continue
		}
		if err := execScheduledSnapshot(p); err != nil {
			log.Printf("[scheduler] policy %s (%s) failed: %v", p.ID, p.Dataset, err)
		}
		changed = true
	}
	if changed {
		_ = scheduler.SavePolicies(policies)
	}
}

func execScheduledSnapshot(p *scheduler.Policy) error {
	label := p.Label
	if label == "" {
		label = "auto"
	}
	name, err := system.CreateSnapshot(p.Dataset, label)
	p.LastRun = time.Now()
	if err != nil {
		p.LastStatus = "error"
		p.LastError = err.Error()
		audit.Log(audit.Entry{
			Action:  audit.ActionCreateSnapshot,
			Target:  p.Dataset,
			Result:  audit.ResultError,
			Details: fmt.Sprintf("scheduler %s: %v", p.ID, err),
		})
		return err
	}
	p.LastStatus = "ok"
	p.LastError = ""
	audit.Log(audit.Entry{
		Action:  audit.ActionCreateSnapshot,
		Target:  name,
		Result:  audit.ResultOK,
		Details: fmt.Sprintf("scheduled policy %s", p.ID),
	})
	if p.Retention > 0 {
		pruneSnapshots(p.Dataset, label, p.Retention)
	}
	return nil
}

func pruneSnapshots(dataset, labelPrefix string, keep int) {
	snaps, err := system.ListSnapshots(dataset)
	if err != nil {
		return
	}
	var ours []system.Snapshot
	for _, s := range snaps {
		if s.Dataset == dataset &&
			(strings.HasPrefix(s.SnapName, labelPrefix+"-") || s.SnapName == labelPrefix) {
			ours = append(ours, s)
		}
	}
	if len(ours) <= keep {
		return
	}
	sort.Slice(ours, func(i, j int) bool {
		return ours[i].Creation.Before(ours[j].Creation)
	})
	for _, s := range ours[:len(ours)-keep] {
		if err := system.DestroySnapshot(s.Name); err != nil {
			log.Printf("[scheduler] prune %s: %v", s.Name, err)
		}
	}
}
