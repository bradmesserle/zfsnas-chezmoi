# Version 4.0.0 — Release Notes

Focus: **Multi-Pool Support** — full first-class management of multiple ZFS pools
alongside a Pool Fixer Wizard, improved health monitoring, and a cluster of smaller
improvements throughout the stack.

---

## 1. Multi-Pool Backend

### `system/zfs.go`

**New functions**

| Function | Description |
|---|---|
| `GetAllPools() ([]*Pool, error)` | Runs `zpool list -Hp` and returns a fully-enriched slice of all imported pools (never nil). |
| `GetPoolByName(name string) (*Pool, error)` | Returns one enriched pool by name; nil if not found. Used by every mutation handler instead of the old blind `GetPool()`. |
| `GetPoolStatusByName(name string) (string, error)` | Runs `zpool status [name]`; empty name returns all pools. |
| `zpoolStatusDevices(poolName, section string, withFullPaths bool)` | Internal helper — extracts ordered device names from `zpool status` output for a given section (`data` or `cache`). |
| `poolMembers(poolName string) (raw, resolved, roles []string)` | Replaces the old `poolMembers` — runs status twice (-P and without) to return raw stored paths, ZFS-resolved paths, and per-disk vdev roles. Falls back to `resolveDevPath()` for by-partuuid entries. |
| `poolMemberRoles(poolName string, count int)` | Determines per-disk role (`stripe`/`mirror`/`raidz1`/`raidz2`) by analysing `zpool status` indent structure. |
| `poolCacheDevs(poolName string) (raw, resolved []string)` | Returns cache device paths for a specific pool. |
| `poolMemberStatuses(poolName string) []string` | Returns per-disk state strings (`ONLINE`, `FAULTED`, etc.) by parsing `zpool status`. |
| `poolMemberPresent(members []string) []bool` | Checks which member paths actually exist under `/dev`. |
| `ListAllDatasets() ([]Dataset, error)` | Returns datasets from every imported pool combined (replaces the old single-pool version used by the datasets handler). |

**Updated `Pool` struct fields**

```go
MemberDevices  []string `json:"member_devices"`   // resolved canonical /dev/sdX paths
MemberRoles    []string `json:"member_roles"`      // per-member vdev role
MemberStatuses []string `json:"member_statuses"`   // per-member device state
MemberPresent  []bool   `json:"member_present"`    // true when /dev path exists
```

### `handlers/pools.go`

All mutation handlers (`HandleSetPoolProperties`, `HandleGrowPool`, `HandleDestroyPool`,
`HandleUpgradePool`, `HandleAddPoolCache`, `HandleRemovePoolCache`, `HandleImportPool`)
now accept an optional `pool` string field in their request body and resolve the target
via `GetPoolByName()` instead of the old `GetPool()` fallback.

**New handler: `HandleGetPools`** — `GET /api/pools`
Returns the full slice from `GetAllPools()`.

**New handler: `HandlePoolCreateStatus`** — `GET /api/pool/create-status?id=`
Polls the status of an async pool creation job (see below).

**Async pool creation**
`HandleCreatePool` now returns `202 Accepted` with a `{ "job_id": "..." }` immediately
and runs `system.CreatePool()` in a background goroutine. The UI polls
`/api/pool/create-status?id=` until status is `"done"` or `"error"`.

### `handlers/datasets.go`

`HandleListDatasets` now calls `system.ListAllDatasets()` (all pools) instead of
`system.ListDatasets(pool.Name)` (single pool).

### `handlers/snapshots.go`

`HandleListSnapshots` now:
- Accepts optional `?pool=` query param — returns snapshots for that pool only.
- Without param — iterates all pools via `GetAllPools()` and returns a combined list.

### `handlers/router.go`

New routes:

```
GET  /api/pools                 HandleGetPools
GET  /api/pool/create-status    HandlePoolCreateStatus
POST /api/pool/clear            HandleClearPool
POST /api/pool/fixer/online     HandlePoolFixerOnline
POST /api/pool/disk/offline     HandleDiskOffline
POST /api/pool/disk/online      HandleDiskOnline
GET  /api/sysinfo/hardware      HandleGetHardwareInfo
```

---

## 2. Pool Fixer Wizard

A guided recovery wizard that appears whenever a pool is in a degraded or suspended
state.

### Backend

**`system/zfs.go`**

| Function | Description |
|---|---|
| `ClearPool(poolName string) error` | Runs `zpool clear` to clear error counters and resume a SUSPENDED pool. |
| `OnlinePoolDisks(poolName string, devices []string) error` | Runs `zpool online <pool> <devs...>` to re-mark one or more disks as online. |
| `SetDiskOffline(poolName, device string) error` | Runs `zpool offline` on a single disk. |
| `SetDiskOnline(poolName, device string) error` | Runs `zpool online` on a single disk. |

**`handlers/pools.go`**

| Handler | Route | Description |
|---|---|---|
| `HandleClearPool` | `POST /api/pool/clear` | Calls `ClearPool`, then `LogPoolHealthEvents` to capture the immediate state change in the audit log. |
| `HandlePoolFixerOnline` | `POST /api/pool/fixer/online` | Two-step recovery: `ClearPool` first (needed when pool is SUSPENDED), then `OnlinePoolDisks`. Writes audit entry and emits health events. |
| `HandleDiskOffline` | `POST /api/pool/disk/offline` | Takes a single disk offline. |
| `HandleDiskOnline` | `POST /api/pool/disk/online` | Brings a single disk online. |

### Frontend (`static/index.html`)

- **"✦ Pool Fixer Wizard" button** — shown in `pool-header-actions` when the pool health
  is not ONLINE or when any member disk is not ONLINE. Styled with a gradient
  (purple → blue → teal) to distinguish it from normal admin actions.
- **Modal `#modal-pool-fixer`** — two scenes:
  - *Scene A — Clear errors*: shown when pool is SUSPENDED or FAULTED without recoverable
    disks. Describes the situation and offers a "Fix now" button that calls
    `POST /api/pool/clear`.
  - *Scene B — Recover disks*: shown when member disks are offline but present in `/dev`.
    Lists recoverable disks and calls `POST /api/pool/fixer/online` with the device list.
- After a successful fix the modal closes, `allPoolsData` is updated, and
  `renderPoolExists()` re-renders the pool tab.

---

## 3. Multi-Pool Frontend Globals & Selectors

### New globals

```js
let allPoolsData           = [];   // all Pool objects from GET /api/pools
let selectedPoolName       = null; // active pool in the Pool tab
let selectedTopBarPoolName = null; // active pool in the top capacity bar
```

### `refreshPoolBar()`

Now fetches `/api/pools` → `allPoolsData`. Resolves `selectedTopBarPoolName`
from user preferences (`currentUser.preferences.selected_top_bar_pool`) or
defaults to the first pool. Sets `poolData` to the matching pool.

### Top-bar pool selector

`renderPoolBar()` reads `<span id="topbar-pool-title">` (id added to HTML):
- 1 pool → static text "Pool Capacity".
- 2+ pools → renders a styled green `<select id="topbar-pool-selector">`.
  `onchange` calls `switchTopBarPool(name)`.

### Pool tab selector

`_renderPoolTabTitle()` updates `<h2 id="pool-page-title">`:
- 1 pool → static "ZFS Pool".
- 2+ pools → renders a styled green `<select id="pool-tab-selector">`.
  `onchange` calls `switchPoolTab(name)`.

### New JS functions

| Function | Description |
|---|---|
| `switchPoolTab(name)` | Updates `selectedPoolName`, saves prefs, re-fetches pool list, calls `renderPoolExists()` + `_renderPoolTabTitle()`. |
| `switchTopBarPool(name)` | Updates `selectedTopBarPoolName`, sets `poolData`, re-renders top bar, saves prefs. |
| `savePoolPrefs()` | Fire-and-forget `PUT /api/prefs` with `selected_pool` + `selected_top_bar_pool`; updates `currentUser.preferences` in memory. |
| `_updatePoolInCache(pool)` | Replaces a pool in `allPoolsData` by name; also refreshes `poolData` and top bar if the pool is the currently selected top-bar pool. |
| `openNewPool()` | Clears pool status interval, calls `renderPoolCreate(el, { backPoolName: selectedPoolName })`. |

### Pool mutation calls — `pool` field added

All fetch calls that modify pool state now include `pool: selectedPoolName` in
the request body (grow, settings, cache add/remove, upgrade).

### After-success updates

Pool settings and cache mutations call `_updatePoolInCache(data)` +
`renderPoolExists(el, data)` instead of directly overwriting `poolData`, keeping
all pool slots in sync.

### Async pool creation polling

After `POST /api/pool` returns `job_id`, the UI polls
`GET /api/pool/create-status?id=` at 1-second intervals, showing a progress bar,
until `status === "done"` (re-renders pool tab) or `status === "error"` (shows error).

### `_fetchPoolStatus()`

Now fetches `/api/pool/status?name=` + `encodeURIComponent(selectedPoolName || '')`.

---

## 4. User Preferences — Pool Selection

### `internal/config/config.go`

```go
type UserPreferences struct {
    ActivityBarCollapsed bool   `json:"activity_bar_collapsed,omitempty"`
    SelectedPool         string `json:"selected_pool,omitempty"`         // last pool in Pool tab
    SelectedTopBarPool   string `json:"selected_top_bar_pool,omitempty"` // last pool in top bar
}
```

No change to `HandleUpdatePrefs` — it already decodes the full struct and saves it.

---

## 5. Health Monitoring Improvements

### `handlers/alerts.go`

**New function: `LogPoolHealthEvents(pool *Pool)`**

Shared pool health event tracker. Compares current pool health and per-member disk
statuses against the last known state and writes audit entries for every transition:

- Pool health worsens (new bad state or different bad state) → `ActionPoolProblem`
- Pool health recovers → `ActionPoolRecovered`
- Disk becomes non-ONLINE → `ActionDiskProblem`
- Disk recovers to ONLINE → `ActionDiskRecovered`

Uses `sync.Mutex`-guarded package-level maps (`healthEvPoolStates`,
`healthEvMemberStates`, `smtpLastPoolHealths`) so the background poller and
pool operation handlers share the same state without duplicating events.

**`StartHealthPoller` / `runHealthCheck` updated**

- Calls `GetAllPools()` (not single `GetPool()`) — all pools are monitored.
- Calls `LogPoolHealthEvents(pool)` for each pool before checking SMTP thresholds.
- SMTP dedup is now per-pool (keyed by pool name in `smtpLastPoolHealths`).

### `internal/audit/audit.go`

New action constants:

```go
ActionPoolProblem    = "pool_problem"
ActionPoolRecovered  = "pool_recovered"
ActionDiskProblem    = "disk_problem"
ActionDiskRecovered  = "disk_recovered"
```

### `system/disks.go` — `zfsPoolDiskNames()` rewritten

Old version fetched `GetPool()` and iterated `Members`/`CacheDevs`.
New version parses `zpool status -P` directly, covering all pools.
Also resolves by-partuuid paths via `resolveDevPath()` and skips unresolved UUIDs.

### `system/sysinfo.go` — `poolMemberBaseNames()` rewritten

Same approach as `zfsPoolDiskNames()`: parses `zpool status -P` across all pools
so the disk I/O poller captures data for every pool's member disks, not just the first.

Disk I/O poll interval reduced from 5 s to 3 s.

---

## 6. Hardware Info Endpoint

### `system/sysinfo.go`

```go
type HardwareInfo struct {
    CPUCores      int    `json:"cpu_cores"`
    TotalRAMBytes uint64 `json:"total_ram_bytes"`
}
func GetHardwareInfo() HardwareInfo
```

Reads CPU core count from `/proc/cpuinfo` and total RAM from `/proc/meminfo`.

### `handlers/dashboard.go`

`HandleGetHardwareInfo` → `GET /api/sysinfo/hardware`
Returns static hardware properties (CPU cores, total RAM) as JSON.

---

## 7. Timezone Fallback

### `system/timezone.go`

`ListTimezones()` now tries `timedatectl list-timezones` first and, if unavailable
(e.g. on minimal Debian installs without `systemd-timesyncd`), falls back to walking
`/usr/share/zoneinfo/` to collect timezone names. Skips `posix/`, `right/`, and
non-timezone files (`.list`, `.tab`, `.zi`, and top-level entries without a `/`).

---

## 8. Miscellaneous

### `main.go`

`WriteTimeout` increased from 60 s to 300 s to accommodate long-running operations
(pool creation, binary self-update, scrub commands).

### Nav dot `updatePoolNavDot()`

Now iterates `allPoolsData` to show a red dot if **any** pool is degraded/faulted,
not just the first pool.

---

## API Surface Summary

| Method | Route | Handler | Auth |
|---|---|---|---|
| GET | `/api/pools` | `HandleGetPools` | auth |
| GET | `/api/pool?name=` | `HandleGetPool` | auth |
| POST | `/api/pool` | `HandleCreatePool` (async) | admin |
| GET | `/api/pool/create-status?id=` | `HandlePoolCreateStatus` | auth |
| GET | `/api/pool/status?name=` | `HandlePoolStatus` | auth |
| POST | `/api/pool/clear` | `HandleClearPool` | admin |
| POST | `/api/pool/fixer/online` | `HandlePoolFixerOnline` | admin |
| POST | `/api/pool/disk/offline` | `HandleDiskOffline` | admin |
| POST | `/api/pool/disk/online` | `HandleDiskOnline` | admin |
| PUT | `/api/pool/settings` | `HandleSetPoolProperties` (+ `pool` field) | admin |
| POST | `/api/pool/grow` | `HandleGrowPool` (+ `pool` field) | admin |
| POST | `/api/pool/upgrade` | `HandleUpgradePool` (+ `pool` field) | admin |
| POST | `/api/pool/cache` | `HandleAddPoolCache` (+ `pool` field) | admin |
| DELETE | `/api/pool/cache` | `HandleRemovePoolCache` (+ `pool` field) | admin |
| GET | `/api/sysinfo/hardware` | `HandleGetHardwareInfo` | auth |

---

## What Did NOT Change

- SMB / NFS shares — unchanged
- Snapshot scheduler — still runs per-dataset; no multi-pool-specific changes
- SMART poller, metrics RRD, binary updater, alerts SMTP config — unchanged
- Dataset operations (create, edit, destroy, mount) — work unchanged; dataset paths
  already carry their pool prefix (`pool/name`)
