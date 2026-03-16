# Version 6.0.0 Plan — Storage Capacity Trend Charts

## Overview

Two major deliverables:

1. **Capacity RRD Collector** — a background goroutine that samples per-pool and per-dataset capacity every 5 minutes and writes into a multi-resolution round-robin database (`config/capacity.rrd.json`) with three tiers: 5-min/1-week, 30-min/1-month, daily/5-years. Each tier stores `{min, avg, max}` aggregates per series.

2. **Capacity Trend Page** — a new "Capacity Trend" item in the Overview sidebar section that renders a stacked area Chart.js chart with pool/dataset selection, time-range picker, and a dashed red usable-capacity ceiling line.

No new Go module dependencies — stdlib + gorilla/mux + gorilla/websocket only (Chart.js is already vendored).

---

## Feature 1: Multi-Resolution Capacity RRD

### Design Rationale

The existing `internal/rrd/rrd.go` is a single-resolution, single-value-per-sample circular buffer (288 slots, `{ts, v}`). The new capacity store needs:

- **Three resolutions**: 5-min raw, 30-min aggregates, daily aggregates
- **Min/avg/max per bucket** (not just a single float64) — required to surface trend volatility at coarser granularities
- **Dynamic series** — series are keyed by pool/dataset name and will appear or disappear as pools come and go
- **Separate file** from `metrics.rrd.json` to avoid coupling

Because these requirements differ substantially from the existing `rrd.DB`, a new package `internal/capacityrrd/` is the cleanest approach. It does not replace or modify `internal/rrd/`.

### New Package: `internal/capacityrrd/capacityrrd.go`

#### Constants

```go
const (
    // Tier 0: 5-minute samples, 1 week retention
    Tier0Slots    = 2016  // 7 * 24 * 12
    Tier0Duration = 5 * time.Minute

    // Tier 1: 30-minute aggregates, 30 days retention
    Tier1Slots    = 1440  // 30 * 24 * 2
    Tier1Duration = 30 * time.Minute

    // Tier 2: daily aggregates, 5 years retention
    Tier2Slots    = 1825  // 365 * 5
    Tier2Duration = 24 * time.Hour
)
```

#### Data Structures

```go
// CapSample is one aggregated slot in a tier.
type CapSample struct {
    TS  int64   `json:"ts"`  // Unix epoch seconds, start of bucket
    Min float64 `json:"min"` // bytes (min over bucket)
    Avg float64 `json:"avg"` // bytes (mean over bucket)
    Max float64 `json:"max"` // bytes (max over bucket)
    N   int     `json:"n"`   // number of raw samples aggregated into this slot
}

// tierData holds one resolution tier's data for all series.
// Each tier is stored as a flat circular buffer per key.
type tierData struct {
    Slots    int                    `json:"slots"`    // max capacity
    Duration int64                  `json:"duration"` // bucket width in seconds
    Series   map[string][]CapSample `json:"series"`   // key → ordered samples (oldest first)
}

// dbData is the full on-disk representation.
type dbData struct {
    Tier0 tierData `json:"tier0"` // 5-min raw
    Tier1 tierData `json:"tier1"` // 30-min aggregates
    Tier2 tierData `json:"tier2"` // daily aggregates
}

// DB is a thread-safe multi-resolution capacity round-robin database.
type DB struct {
    mu             sync.Mutex
    path           string
    data           dbData
    // Pending raw samples within the current open Tier1/Tier2 buckets (in-memory only).
    pending        map[string][]float64 // key → values within current Tier1 bucket
    pendingT1Start time.Time
    pendingT2      map[string][]float64 // key → values within current Tier2 (daily) bucket
    pendingT2Start time.Time
}
```

The `tierData.Series[key]` array is stored sorted chronologically (oldest first). On `Record()`, a new sample is appended; if `len > Slots`, the first element is dropped (O(n) but n ≤ 2016, writes happen only every 5 min — acceptable).

#### Public API

```go
// Open loads the DB from disk, creating a new empty DB if the file does not exist.
func Open(path string) (*DB, error)

// Record adds one 5-minute raw sample for the named series.
// It also triggers aggregation into Tier1 and Tier2 when bucket boundaries are crossed.
func (db *DB) Record(key string, value float64, now time.Time)

// Query returns all stored samples for the given tier and key, in chronological order.
// tier: 0=5min, 1=30min, 2=daily
func (db *DB) Query(tier int, key string) []CapSample

// Keys returns the names of all series present in Tier0 (the master list).
func (db *DB) Keys() []string

// Flush persists the DB to disk atomically (write temp file + rename).
func (db *DB) Flush() error

// DeleteKey removes all data for the given series key across all tiers.
func (db *DB) DeleteKey(key string)
```

#### Aggregation Logic

When `Record("pool:tank:used", v, now)` is called:

1. Write raw sample into Tier0 circular buffer.
2. Append `v` to `pending[key]` (in-memory accumulator for the current 30-min window).
3. If `now.Truncate(30*time.Minute) != pendingT1Start` (bucket boundary crossed):
   - For each key in `pending`, compute `min`, `avg`, `max`, write one `CapSample` to Tier1.
   - Reset `pending` and `pendingT1Start = now.Truncate(30*time.Minute)`.
4. Similarly track `pendingT2` for daily aggregation. When `now.Truncate(24*time.Hour)` advances, write one Tier2 sample per key from the daily accumulator.
5. A single `Record` call may thus also append to Tier1 and/or Tier2 when boundaries cross.

Edge case — first boot: `pendingT1Start` is zero. Initialize to `now.Truncate(30*time.Minute)` and start accumulating normally.

Edge case — server restart mid-bucket: in-memory pending slices are lost. The current partial bucket restarts with only post-restart samples. This is acceptable; Tier1/Tier2 lose at most one bucket boundary on restart.

#### Series Naming Convention

Series keys follow a structured colon-delimited scheme:

| Key pattern | Meaning |
|-------------|---------|
| `pool:<name>:used` | Pool used bytes |
| `pool:<name>:avail` | Pool available bytes |
| `pool:<name>:usable` | Pool total usable bytes (ceiling) |
| `pool:<name>:compratio` | Pool compression ratio (float, e.g. 1.24) |
| `ds:<pool>/<path>:used` | Dataset used bytes |
| `ds:<pool>/<path>:avail` | Dataset available bytes |
| `ds:<pool>/<path>:quota` | Dataset quota bytes (omitted if quota=0) |
| `ds:<pool>/<path>:compratio` | Dataset compression ratio |

The structured format enables frontend filtering: `key.startsWith("pool:tank:")` for all tank pool series, `key.startsWith("ds:tank/")` for all tank datasets.

---

## Feature 2: Capacity Collector Goroutine

### New File: `system/capacity_collector.go`

```go
package system

import (
    "log"
    "path/filepath"
    "strconv"
    "strings"
    "time"
    "zfsnas/internal/capacityrrd"
)

var capacityDB *capacityrrd.DB

// GetCapacityDB returns the shared capacity RRD database (may be nil on startup).
func GetCapacityDB() *capacityrrd.DB {
    return capacityDB
}

// StartCapacityCollector opens (or creates) the capacity RRD and starts the 5-minute poller.
func StartCapacityCollector(configDir string) {
    dbPath := filepath.Join(configDir, "capacity.rrd.json")
    db, err := capacityrrd.Open(dbPath)
    if err != nil {
        log.Printf("capacity: failed to open RRD at %s: %v", dbPath, err)
        return
    }
    capacityDB = db

    go func() {
        tick := time.NewTicker(5 * time.Minute)
        defer tick.Stop()
        for now := range tick.C {
            sampleCapacity(db, now)
            if err := db.Flush(); err != nil {
                log.Printf("capacity: flush error: %v", err)
            }
        }
    }()
}

func sampleCapacity(db *capacityrrd.DB, now time.Time) {
    pools, err := GetAllPools()
    if err != nil {
        log.Printf("capacity: GetAllPools: %v", err)
        return
    }
    for _, p := range pools {
        pfx := "pool:" + p.Name + ":"
        db.Record(pfx+"used",       float64(p.UsableUsed),  now)
        db.Record(pfx+"avail",      float64(p.UsableAvail), now)
        db.Record(pfx+"usable",     float64(p.UsableSize),  now)
        if cr := parseComprRatio(p.Compression); cr > 0 {
            db.Record(pfx+"compratio", cr, now)
        }
    }

    datasets, err := ListAllDatasets()
    if err != nil {
        log.Printf("capacity: ListAllDatasets: %v", err)
        return
    }
    for _, d := range datasets {
        pfx := "ds:" + d.Name + ":"
        db.Record(pfx+"used",  float64(d.Used),  now)
        db.Record(pfx+"avail", float64(d.Avail), now)
        if d.Quota > 0 {
            db.Record(pfx+"quota", float64(d.Quota), now)
        }
        if cr := parseComprRatio(d.CompRatio); cr > 0 {
            db.Record(pfx+"compratio", cr, now)
        }
    }
}

// parseComprRatio converts "1.24x" → 1.24; returns 0 on error.
func parseComprRatio(s string) float64 {
    s = strings.TrimSuffix(strings.TrimSpace(s), "x")
    v, _ := strconv.ParseFloat(s, 64)
    return v
}
```

Edge cases:
- **New dataset/pool**: `db.Record` creates the series automatically on first call.
- **Removed dataset/pool**: Series remains in RRD with historical data; ages out of circular buffer naturally. Not auto-deleted in v6.

---

## Feature 3: New API Endpoints

### New Handler File: `handlers/capacity.go`

#### `GET /api/capacity/series`

Returns metadata about all currently active pools and datasets (derived from the live ZFS query, not the RRD). Used to populate the selector panel.

Response:
```json
{
  "pools": [
    { "key": "pool:tank", "name": "tank", "usable": 10737418240 }
  ],
  "datasets": [
    { "key": "ds:tank/media", "name": "tank/media", "pool": "tank" }
  ]
}
```

- `usable` comes from live `GetAllPools()` so the dashed ceiling line always reflects current reality.
- If a pool is not imported, it won't appear in this response (but its RRD data still exists).

Handler: `HandleCapacitySeries`

#### `GET /api/capacity/data`

Returns time-series CapSample data for the requested series keys.

Query parameters:
- `keys` — comma-separated series keys (e.g. `pool:tank:used,ds:tank/media:used`)
- `tier` — `0` (5-min, default), `1` (30-min), `2` (daily)
- `since` — optional Unix timestamp; only return samples with `ts >= since`

Response:
```json
{
  "tier": 0,
  "series": {
    "pool:tank:used":     [{"ts":1234567890,"min":1000,"avg":1050,"max":1100,"n":1}, ...],
    "ds:tank/media:used": [...]
  }
}
```

Handler: `HandleCapacityData`

#### `GET /api/capacity/oldest`

Returns the Unix timestamp of the oldest Tier0 sample across all series. Used by "Since Data" to set the X-axis minimum.

```json
{ "oldest_ts": 1710000000 }
```

Returns `{ "oldest_ts": 0 }` if no data exists yet.

Handler: `HandleCapacityOldest`

### Router Changes (`handlers/router.go`)

```go
r.Handle("/api/capacity/series",
    RequireAuth(http.HandlerFunc(HandleCapacitySeries))).Methods("GET")
r.Handle("/api/capacity/data",
    RequireAuth(http.HandlerFunc(HandleCapacityData))).Methods("GET")
r.Handle("/api/capacity/oldest",
    RequireAuth(http.HandlerFunc(HandleCapacityOldest))).Methods("GET")
```

---

## Feature 4: Startup Wiring (`main.go`)

Add after the existing `system.StartMetricsCollector(absConfig)` call:

```go
// Capacity RRD collector (5-minute samples, 3-tier retention up to 5 years)
system.StartCapacityCollector(absConfig)
```

---

## Feature 5: Frontend — "Capacity Trend" Page

### Sidebar Entry (`static/index.html`)

In the Overview section, after the Performance nav item:

```html
<button class="nav-item" onclick="showPage('capacity')" id="nav-capacity">
  <span class="nav-icon">💾</span><span class="nav-label"> Capacity Trend</span>
</button>
```

### Page HTML Structure

```html
<div class="page" id="page-capacity">
  <!-- Header: title + selector button + time range -->
  <div style="display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin-bottom:20px;position:relative;">
    <div style="font-size:18px;font-weight:700;color:var(--text);">Capacity Trend</div>

    <!-- Blue dropdown button -->
    <button id="cap-selector-btn" onclick="toggleCapSelector()"
            style="background:rgba(10,132,255,0.15);border:1px solid rgba(10,132,255,0.4);
                   color:var(--accent);border-radius:8px;padding:6px 14px;font-size:13px;
                   font-weight:600;cursor:pointer;display:flex;align-items:center;gap:6px;">
      <span id="cap-selector-label">Select pools &amp; datasets…</span>
      ▾
    </button>

    <!-- Selector dropdown panel -->
    <div id="cap-selector-panel" class="hidden cap-selector-panel">
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:10px;">
        <span style="font-size:12px;font-weight:700;text-transform:uppercase;
                     letter-spacing:.06em;color:var(--text-3);">Select Series</span>
        <span>
          <a onclick="capSelectAll()" style="cursor:pointer;color:var(--accent);font-size:12px;margin-right:10px;">All</a>
          <a onclick="capDeselectAll()" style="cursor:pointer;color:var(--text-3);font-size:12px;">None</a>
        </span>
      </div>
      <!-- Quick-select buttons: one per pool -->
      <div id="cap-quick-btns" style="display:flex;flex-wrap:wrap;gap:6px;margin-bottom:12px;"></div>
      <!-- Pool+dataset tree with checkboxes -->
      <div id="cap-tree" style="max-height:300px;overflow-y:auto;"></div>
      <div style="margin-top:12px;text-align:right;">
        <button onclick="applyCapSelection();toggleCapSelector()"
                style="background:var(--accent);color:#fff;border:none;border-radius:8px;
                       padding:6px 18px;font-size:13px;font-weight:600;cursor:pointer;">Apply</button>
      </div>
    </div>

    <!-- Time range selector (top right) -->
    <div style="margin-left:auto;display:flex;gap:6px;">
      <button class="cap-range-btn active" data-range="all"   onclick="setCapRange('all')">Since Data</button>
      <button class="cap-range-btn"        data-range="day"   onclick="setCapRange('day')">Day</button>
      <button class="cap-range-btn"        data-range="week"  onclick="setCapRange('week')">Week</button>
      <button class="cap-range-btn"        data-range="month" onclick="setCapRange('month')">Month</button>
      <button class="cap-range-btn"        data-range="year"  onclick="setCapRange('year')">Year</button>
    </div>
  </div>

  <!-- Chart -->
  <div class="card-sm" style="padding:20px;">
    <canvas id="cap-chart" style="width:100%;height:400px;"></canvas>
  </div>

  <!-- No-data state -->
  <div id="cap-no-data" class="hidden" style="text-align:center;padding:60px 20px;color:var(--text-3);">
    <div style="font-size:40px;margin-bottom:12px;">📊</div>
    <div style="font-weight:600;font-size:16px;color:var(--text-2);">No capacity data yet</div>
    <p style="margin-top:8px;">Data will appear after the first 5-minute collection interval.</p>
  </div>
</div>
```

### JavaScript Functions

All JS added inside the existing `<script>` block.

#### State variables

```js
let capMeta = { pools: [], datasets: [] };   // from /api/capacity/series
let capSelectedKeys = [];                     // keys selected in panel (ds:... or pool:...)
let capCurrentRange = 'all';
let capChartInstance = null;
let capOldestTs = 0;
```

#### `loadCapacityPage()`

Called from `showPage('capacity')`:

```js
async function loadCapacityPage() {
    const resp = await fetch('/api/capacity/series');
    if (!resp.ok) return;
    capMeta = await resp.json();
    buildCapSelectorPanel(capMeta);
    if (capSelectedKeys.length === 0 && capMeta.pools.length > 0) {
        capSelectPoolAndDatasets(capMeta.pools[0].name);
    }
    // Fetch oldest timestamp for "Since Data" default
    const or = await fetch('/api/capacity/oldest');
    if (or.ok) { const od = await or.json(); capOldestTs = od.oldest_ts || 0; }
    await renderCapChart();
}
```

#### `buildCapSelectorPanel(meta)`

Builds the quick-select buttons and checkbox tree:

```js
function buildCapSelectorPanel(meta) {
    // Quick-select buttons
    const qb = document.getElementById('cap-quick-btns');
    qb.innerHTML = '';
    for (const p of meta.pools) {
        const btn = document.createElement('button');
        btn.className = 'btn-outline';
        btn.style = 'font-size:12px;padding:3px 10px;';
        btn.textContent = p.name + ' + all datasets';
        btn.onclick = () => { capSelectPoolAndDatasets(p.name); buildCapTree(meta); };
        qb.appendChild(btn);
    }
    buildCapTree(meta);
}

function buildCapTree(meta) {
    const tree = document.getElementById('cap-tree');
    tree.innerHTML = '';
    for (const p of meta.pools) {
        // Pool row
        const poolKey = 'pool:' + p.name + ':used';
        tree.appendChild(capCheckRow(poolKey, p.name + '  (pool)', true));
        // Dataset rows for this pool
        const poolDatasets = meta.datasets.filter(d => d.pool === p.name);
        for (const d of poolDatasets) {
            const dsKey = 'ds:' + d.name + ':used';
            tree.appendChild(capCheckRow(dsKey, '  ' + d.name, false));
        }
    }
}

function capCheckRow(key, label, isBold) {
    const row = document.createElement('label');
    row.style = 'display:flex;align-items:center;gap:8px;padding:4px 0;cursor:pointer;' +
                (isBold ? 'font-weight:600;' : '');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.dataset.key = key;
    cb.checked = capSelectedKeys.includes(key);
    row.appendChild(cb);
    row.appendChild(document.createTextNode(label));
    return row;
}
```

#### `capSelectPoolAndDatasets(poolName)`

```js
function capSelectPoolAndDatasets(poolName) {
    capSelectedKeys = ['pool:' + poolName + ':used'];
    for (const d of capMeta.datasets) {
        if (d.pool === poolName) capSelectedKeys.push('ds:' + d.name + ':used');
    }
    updateCapSelectorLabel();
}
```

#### `applyCapSelection()`

Reads checkboxes from the tree and rebuilds `capSelectedKeys`, then re-renders:

```js
function applyCapSelection() {
    const cbs = document.querySelectorAll('#cap-tree input[type=checkbox]');
    capSelectedKeys = [];
    cbs.forEach(cb => { if (cb.checked) capSelectedKeys.push(cb.dataset.key); });
    updateCapSelectorLabel();
    renderCapChart();
}
```

#### `updateCapSelectorLabel()`

```js
function updateCapSelectorLabel() {
    const pools = capSelectedKeys.filter(k => k.startsWith('pool:')).length;
    const ds    = capSelectedKeys.filter(k => k.startsWith('ds:')).length;
    let label = '';
    if (pools === 0 && ds === 0) label = 'Select pools & datasets…';
    else if (pools > 0 && ds === 0) label = pools + ' pool' + (pools>1?'s':'');
    else if (pools === 0 && ds > 0) label = ds + ' dataset' + (ds>1?'s':'');
    else label = pools + ' pool' + (pools>1?'s':'') + ', ' + ds + ' dataset' + (ds>1?'s':'');
    document.getElementById('cap-selector-label').textContent = label;
}
```

#### `setCapRange(range)`

```js
function setCapRange(range) {
    capCurrentRange = range;
    document.querySelectorAll('.cap-range-btn').forEach(b => {
        b.classList.toggle('active', b.dataset.range === range);
    });
    renderCapChart();
}
```

#### Tier selection logic

```js
function capTierForRange(range) {
    if (range === 'day' || range === 'week') return 0;    // 5-min
    if (range === 'month') return 1;                       // 30-min
    if (range === 'year') return 2;                        // daily
    // 'all': choose tier based on oldest data age
    if (capOldestTs === 0) return 0;
    const ageSeconds = Math.floor(Date.now() / 1000) - capOldestTs;
    if (ageSeconds > 30 * 86400) return 2;
    if (ageSeconds > 7 * 86400)  return 1;
    return 0;
}

function capSinceTs(range) {
    const now = Math.floor(Date.now() / 1000);
    const map = { day: 86400, week: 604800, month: 2592000, year: 31536000 };
    return map[range] ? now - map[range] : 0;  // 0 = all data
}
```

#### `renderCapChart()`

```js
async function renderCapChart() {
    if (capSelectedKeys.length === 0) {
        document.getElementById('cap-chart').parentElement.classList.add('hidden');
        document.getElementById('cap-no-data').classList.remove('hidden');
        return;
    }

    const tier  = capTierForRange(capCurrentRange);
    const since = capSinceTs(capCurrentRange);

    // Also fetch pool usable keys for the ceiling line
    const poolUsableKeys = capMeta.pools
        .filter(p => capSelectedKeys.some(k => k.startsWith('pool:' + p.name + ':') ||
                                               k.startsWith('ds:' + p.name + '/')))
        .map(p => 'pool:' + p.name + ':usable');

    const allKeys = [...new Set([...capSelectedKeys, ...poolUsableKeys])];
    const params  = new URLSearchParams({ keys: allKeys.join(','), tier, since });
    const resp    = await fetch('/api/capacity/data?' + params);
    if (!resp.ok) return;
    const data = await resp.json();

    // Check for empty data
    const hasData = Object.values(data.series || {}).some(arr => arr && arr.length > 0);
    document.getElementById('cap-chart').parentElement.classList.toggle('hidden', !hasData);
    document.getElementById('cap-no-data').classList.toggle('hidden', hasData);
    if (!hasData) return;

    // Build datasets
    const chartDatasets = [];
    const CAP_COLORS = [
        '#0a84ff','#32d74b','#ff9f0a','#bf5af2',
        '#64d2ff','#ffd60a','#30d158','#ff6961','#b4b4b4'
    ];

    // Separate pool-used and ds-used series from ceiling series
    const usedKeys   = capSelectedKeys;  // pool:*:used + ds:*:used
    const usableKeys = poolUsableKeys;

    // Apply ">6 datasets" aggregation rule
    const dsKeys   = usedKeys.filter(k => k.startsWith('ds:'));
    const poolKeys = usedKeys.filter(k => k.startsWith('pool:'));

    let finalDsKeys = dsKeys;
    let otherKeys   = [];
    if (dsKeys.length > 6) {
        // Sort ds by median avg usage descending, keep top 5, rest → "Other"
        const medians = dsKeys.map(k => {
            const samples = data.series[k] || [];
            if (!samples.length) return { k, med: 0 };
            const avgs = samples.map(s => s.avg).sort((a,b)=>a-b);
            return { k, med: avgs[Math.floor(avgs.length/2)] };
        }).sort((a,b) => b.med - a.med);
        finalDsKeys = medians.slice(0,5).map(m => m.k);
        otherKeys   = medians.slice(5).map(m => m.k);
    }

    let colorIdx = 0;

    // Pool-used datasets
    for (const k of poolKeys) {
        const samples = data.series[k] || [];
        const color   = CAP_COLORS[colorIdx++ % CAP_COLORS.length];
        chartDatasets.push({
            label: k.split(':')[1] + ' (pool)',
            data:  samples.map(s => ({ x: s.ts * 1000, y: s.avg })),
            borderColor: color, backgroundColor: color + '33',
            fill: true, tension: 0.2, pointRadius: 0, stack: 'used',
        });
    }

    // Individual dataset curves
    for (const k of finalDsKeys) {
        const samples = data.series[k] || [];
        const name    = k.replace(/^ds:/, '').replace(/:used$/, '');
        const color   = CAP_COLORS[colorIdx++ % CAP_COLORS.length];
        chartDatasets.push({
            label: name,
            data:  samples.map(s => ({ x: s.ts * 1000, y: s.avg })),
            borderColor: color, backgroundColor: color + '33',
            fill: true, tension: 0.2, pointRadius: 0, stack: 'used',
        });
    }

    // "Other" aggregate
    if (otherKeys.length > 0) {
        const merged = mergeCapOther(otherKeys, data.series);
        chartDatasets.push({
            label: 'Other (' + otherKeys.length + ' datasets)',
            data:  merged,
            borderColor: '#888', backgroundColor: '#88888833',
            fill: true, tension: 0.2, pointRadius: 0, stack: 'used',
        });
    }

    // Dashed red usable ceiling line (not stacked)
    let ceilSamples = null;
    if (usableKeys.length === 1) {
        ceilSamples = data.series[usableKeys[0]] || [];
    } else if (usableKeys.length > 1) {
        // Sum usable across multiple pools at each timestamp
        ceilSamples = mergeCapSum(usableKeys, data.series);
    }
    if (ceilSamples && ceilSamples.length > 0) {
        chartDatasets.push({
            label: 'Usable Capacity' + (usableKeys.length > 1 ? ' (total)' : ''),
            data:  ceilSamples.map(s => ({ x: (s.ts || s.x/1000) * 1000, y: s.avg || s.y })),
            borderColor: '#ff453a', borderDash: [6, 3],
            backgroundColor: 'transparent', fill: false,
            tension: 0, pointRadius: 0, stack: undefined,
            borderWidth: 2,
        });
    }

    // Render
    if (capChartInstance) { capChartInstance.destroy(); capChartInstance = null; }
    const ctx = document.getElementById('cap-chart').getContext('2d');
    capChartInstance = new Chart(ctx, {
        type: 'line',
        data: { datasets: chartDatasets },
        options: {
            responsive: true,
            interaction: { mode: 'index', intersect: false },
            scales: {
                x: {
                    type: 'linear',
                    ticks: {
                        maxTicksLimit: 8,
                        color: getComputedStyle(document.documentElement)
                                   .getPropertyValue('--text-3').trim(),
                        callback: val => {
                            const d = new Date(val);
                            return d.toLocaleDateString([], {month:'short',day:'numeric'})
                                 + ' ' + d.toLocaleTimeString([], {hour:'2-digit',minute:'2-digit'});
                        }
                    }
                },
                y: {
                    stacked: true,
                    ticks: {
                        color: getComputedStyle(document.documentElement)
                                   .getPropertyValue('--text-3').trim(),
                        callback: v => formatBytes(v)  // reuse existing formatBytes()
                    },
                    grid: { color: 'rgba(128,128,128,0.1)' }
                }
            },
            plugins: {
                legend: {
                    position: 'bottom',
                    labels: {
                        color: getComputedStyle(document.documentElement)
                                   .getPropertyValue('--text-2').trim(),
                        boxWidth: 12, padding: 16
                    }
                },
                tooltip: {
                    callbacks: {
                        label: ctx => ctx.dataset.label + ': ' + formatBytes(ctx.parsed.y)
                    }
                }
            }
        }
    });
}

// Merge "other" datasets by summing avg at each aligned timestamp
function mergeCapOther(keys, series) {
    return mergeCapSum(keys, series);
}

// Sum multiple series by timestamp, return [{x: ms, y: sum}]
function mergeCapSum(keys, series) {
    const map = new Map();
    for (const k of keys) {
        for (const s of (series[k] || [])) {
            const ms = s.ts * 1000;
            map.set(ms, (map.get(ms) || 0) + (s.avg || 0));
        }
    }
    return Array.from(map.entries()).sort((a,b)=>a[0]-b[0]).map(([x,y])=>({x,y}));
}
```

### CSS Additions (`static/style.css`)

```css
/* Capacity Trend — time range selector buttons */
.cap-range-btn {
    padding: 4px 12px;
    font-size: 12px;
    font-weight: 600;
    border-radius: 6px;
    border: 1px solid var(--border);
    background: transparent;
    color: var(--text-2);
    cursor: pointer;
    transition: background .15s, color .15s;
}
.cap-range-btn:hover {
    background: var(--surface-2);
    color: var(--text);
}
.cap-range-btn.active {
    background: rgba(10,132,255,0.15);
    border-color: rgba(10,132,255,0.4);
    color: var(--accent);
}

/* Capacity selector dropdown panel */
.cap-selector-panel {
    position: absolute;
    top: 100%;
    left: 0;
    z-index: 50;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 16px;
    min-width: 320px;
    box-shadow: var(--shadow);
    margin-top: 4px;
}
```

---

## Edge Cases

### First Boot — No Data

`capacityrrd.Open()` creates an empty DB. `GET /api/capacity/data` returns `{ "series": {} }`. Frontend shows the `#cap-no-data` empty state instead of the chart.

### Pool/Dataset Appears for the First Time

`db.Record` creates the new series on the first call. `GET /api/capacity/series` includes it after the next collection tick. The selector panel rebuilds on each `loadCapacityPage()` navigation.

### Pool/Dataset Destroyed

Series remains in RRD but no longer appears in `GET /api/capacity/series`. Historical data is still queryable by explicit key. Old circular buffer entries age out naturally.

### Dataset With No Quota

The `ds:<name>:quota` series simply has no entries. The frontend omits quota lines when the series is empty.

### Multi-Pool Ceiling Line

Sum of `pool:<name>:usable` latest values across all selected pools. Labeled "Usable Capacity (total)" in the legend.

### Large Dataset Count (>50)

`GET /api/capacity/data` response for 50 full series ≈ 2-3 MB — acceptable on a local network. Frontend UI shows a soft warning banner when >20 series are selected. The >6 aggregation rule activates automatically.

### Capacity RRD File Size

For 20 series (1 pool + 19 datasets), all three tiers full:
- Tier0: 2016 × 20 × ~55 bytes ≈ 2.2 MB
- Tier1: 1440 × 20 × ~55 bytes ≈ 1.6 MB
- Tier2: 1825 × 20 × ~55 bytes ≈ 2.0 MB
- **Total ≈ 5.8 MB** — acceptable for embedded JSON storage.

---

## New Files Summary

| File | Purpose |
|------|---------|
| `internal/capacityrrd/capacityrrd.go` | Multi-resolution min/avg/max capacity RRD — new package, does not touch existing `internal/rrd` |
| `system/capacity_collector.go` | 5-minute ZFS capacity poller goroutine |
| `handlers/capacity.go` | Three API handlers: series metadata, time-series data, oldest timestamp |

## Modified Files Summary

| File | Change |
|------|--------|
| `handlers/router.go` | Register 3 new `/api/capacity/` routes |
| `main.go` | Call `system.StartCapacityCollector(absConfig)` after metrics collector |
| `static/index.html` | Sidebar nav item, new page div, all capacity JS |
| `static/style.css` | `.cap-range-btn`, `.cap-selector-panel` styles |

## New API Routes

```
GET /api/capacity/series   → pool/dataset metadata with live usable bytes
GET /api/capacity/data     → CapSample time-series (?keys=&tier=&since=)
GET /api/capacity/oldest   → oldest Tier0 sample timestamp
```

---

## Implementation Order

1. `internal/capacityrrd/capacityrrd.go` — `Open`, `Record`, `Query`, `Keys`, `Flush`, `DeleteKey` with 3-tier circular buffers and bucket-boundary aggregation
2. `system/capacity_collector.go` — `StartCapacityCollector`, `sampleCapacity`, `parseComprRatio`
3. `handlers/capacity.go` — `HandleCapacitySeries`, `HandleCapacityData`, `HandleCapacityOldest`
4. `handlers/router.go` — add 3 routes
5. `main.go` — add `StartCapacityCollector` call
6. `static/style.css` — `.cap-range-btn`, `.cap-selector-panel`
7. `static/index.html` — sidebar nav item
8. `static/index.html` — page HTML (header, selector panel, canvas, no-data state)
9. `static/index.html` — JS: state vars, `loadCapacityPage`, `buildCapSelectorPanel`, `buildCapTree`, `capSelectPoolAndDatasets`, `capSelectAll`, `capDeselectAll`, `applyCapSelection`, `updateCapSelectorLabel`, `toggleCapSelector`, `setCapRange`, `capTierForRange`, `capSinceTs`, `renderCapChart`, `mergeCapOther`, `mergeCapSum`
10. Build, test, deploy
