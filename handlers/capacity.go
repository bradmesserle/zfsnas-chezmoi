package handlers

import (
	"net/http"
	"strconv"
	"strings"
	"zfsnas/internal/capacityrrd"
	"zfsnas/system"
)

// HandleCapacitySeries returns metadata about all currently active pools,
// datasets, and zvols. Used by the frontend to populate the selector panel.
// Response: { pools: [{key, name, usable}], datasets: [{key, name, pool}], zvols: [{key, name, pool, used, size}] }
func HandleCapacitySeries(w http.ResponseWriter, r *http.Request) {
	pools, err := system.GetAllPools()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to list pools: "+err.Error())
		return
	}

	type poolMeta struct {
		Key    string `json:"key"`
		Name   string `json:"name"`
		Usable uint64 `json:"usable"`
		Used   uint64 `json:"used"`
	}
	type dsMeta struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Pool string `json:"pool"`
		Used uint64 `json:"used"`
	}
	type zvolMeta struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Pool string `json:"pool"`
		Used uint64 `json:"used"`
		Size uint64 `json:"size"`
	}

	poolList := make([]poolMeta, 0, len(pools))
	for _, p := range pools {
		poolList = append(poolList, poolMeta{
			Key:    "pool:" + p.Name,
			Name:   p.Name,
			Usable: p.UsableSize,
			Used:   p.UsableUsed,
		})
	}

	datasets, err := system.ListAllDatasets()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to list datasets: "+err.Error())
		return
	}

	dsList := make([]dsMeta, 0, len(datasets))
	for _, d := range datasets {
		poolName := d.Name
		if idx := strings.IndexByte(d.Name, '/'); idx >= 0 {
			poolName = d.Name[:idx]
		}
		dsList = append(dsList, dsMeta{
			Key:  "ds:" + d.Name,
			Name: d.Name,
			Pool: poolName,
			Used: d.Used,
		})
	}

	zvols, _ := system.ListAllZVols()
	zvolList := make([]zvolMeta, 0, len(zvols))
	for _, z := range zvols {
		zvolList = append(zvolList, zvolMeta{
			Key:  "zv:" + z.Name,
			Name: z.Name,
			Pool: z.Pool,
			Used: z.Used,
			Size: z.Size,
		})
	}

	jsonOK(w, map[string]interface{}{
		"pools":    poolList,
		"datasets": dsList,
		"zvols":    zvolList,
	})
}

// HandleCapacityData returns time-series CapSample data for the requested series.
// Query params:
//   - keys  — comma-separated series keys (e.g. "pool:tank:used,ds:tank/media:used")
//   - tier  — 0 (5-min, default), 1 (30-min), 2 (daily)
//   - since — optional Unix timestamp; only return samples with ts >= since
func HandleCapacityData(w http.ResponseWriter, r *http.Request) {
	db := system.GetCapacityDB()
	if db == nil {
		jsonErr(w, http.StatusServiceUnavailable, "capacity collector not ready")
		return
	}

	keysParam := r.URL.Query().Get("keys")
	if keysParam == "" {
		jsonErr(w, http.StatusBadRequest, "keys parameter required")
		return
	}
	keys := strings.Split(keysParam, ",")

	tier, _ := strconv.Atoi(r.URL.Query().Get("tier"))
	if tier < 0 || tier > 2 {
		tier = 0
	}

	var since int64
	if s := r.URL.Query().Get("since"); s != "" {
		since, _ = strconv.ParseInt(s, 10, 64)
	}

	result := make(map[string][]capacityrrd.CapSample, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		samples := db.Query(tier, key)
		if since > 0 && len(samples) > 0 {
			// Find first index at or after `since`.
			cut := len(samples)
			for i, s := range samples {
				if s.TS >= since {
					cut = i
					break
				}
			}
			samples = samples[cut:]
		}
		if samples == nil {
			samples = []capacityrrd.CapSample{}
		}
		result[key] = samples
	}

	jsonOK(w, map[string]interface{}{
		"tier":   tier,
		"series": result,
	})
}

// HandleCapacityOldest returns the Unix timestamp of the oldest Tier0 sample
// across all series. Returns oldest_ts=0 if no data has been collected yet.
func HandleCapacityOldest(w http.ResponseWriter, r *http.Request) {
	db := system.GetCapacityDB()
	var oldest int64
	if db != nil {
		oldest = db.OldestTS()
	}
	jsonOK(w, map[string]int64{"oldest_ts": oldest})
}
