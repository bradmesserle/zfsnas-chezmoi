# Version 3.4.0 — Plan

## Goals

1. **Multi-pool support** — allow creating and managing multiple ZFS pools side-by-side
2. **Pool tab selector** — when >1 pool exists, the "ZFS Pool" heading becomes a green dropdown to switch between pools
3. **Top-bar pool selector** — when >1 pool, "Pool Capacity" becomes a green dropdown to switch the capacity bar to any pool
4. **New Pool button** — visible whenever at least one pool exists; renders the creation form inline with a ← Back button
5. **Pool preference persistence** — last selected pool (tab + top bar) saved per-user across sessions

---

## Feature 1 — Backend: GetAllPools, GetPoolByName, GetPoolStatusByName

### Problem
`GetPool()` only returns the first pool line from `zpool list`. All mutation handlers call
`GetPool()` to identify the target pool — this breaks as soon as a second pool exists.

### Changes — `system/zfs.go`

**`GetAllPools() ([]*Pool, error)`**
Runs `zpool list -Hp -o name,size,alloc,free,health`, parses every line via the existing
`parsePool()`, enriches each pool (usable size, members, cache devs, vdev type, operation,
root props). Returns empty slice (never nil) when no pools exist.

**`GetPoolByName(name string) (*Pool, error)`**
Runs `zpool list -Hp -o name,size,alloc,free,health <name>`, enriches exactly like `GetPool()`.
Returns nil if the pool does not exist. Used by every mutation handler instead of the
blind `GetPool()`.

**`GetPoolStatusByName(name string) (string, error)`**
Runs `sudo zpool status [name]`. Extends `GetPoolStatus()` with an optional pool name.
When `name` is empty the output covers all pools (current behaviour preserved).

---

## Feature 2 — Backend: HandleGetPools + updated handlers

### Changes — `handlers/pools.go`

**`HandleGetPools`** (new) — `GET /api/pools`
Calls `system.GetAllPools()`, returns the slice (empty array if none).

**`HandleGetPool`** — `GET /api/pool?name=<name>` (backward-compatible)
Reads optional `?name=` query param. If present calls `GetPoolByName(name)`,
otherwise falls back to `GetPool()`.

**`HandlePoolStatus`** — `GET /api/pool/status?name=<name>`
Reads optional `?name=` query param, calls `GetPoolStatusByName(name)`.

**`HandleSetPoolProperties`** — adds `Pool string` to request struct.
Resolves pool via `GetPoolByName(req.Pool)` (fallback `GetPool()` if blank).
Returns updated pool via `GetPoolByName(pool.Name)`.

**`HandleGrowPool`** — adds `Pool string` to request struct. Same resolution pattern.

**`HandleDestroyPool`** — replaces `GetPool()` + name-match check with a single
`GetPoolByName(req.Name)` call.

**`HandleUpgradePool`** — adds `Pool string` request body (was empty body).

**`HandleAddPoolCache`** / **`HandleRemovePoolCache`** — add `Pool string` to request struct.

**`HandleCreatePool`** — after creation returns `GetPoolByName(req.Name)` instead of `GetPool()`.

**`HandleImportPool`** — after import returns `GetPoolByName(req.Name)` instead of `GetPool()`.

### Changes — `handlers/router.go`

```
GET /api/pools   →  HandleGetPools  (RequireAuth)
```

---

## Feature 3 — User preferences for pool selection

### Changes — `internal/config/config.go`

```go
type UserPreferences struct {
    ActivityBarCollapsed bool   `json:"activity_bar_collapsed,omitempty"`
    SelectedPool         string `json:"selected_pool,omitempty"`         // last pool in Pool tab
    SelectedTopBarPool   string `json:"selected_top_bar_pool,omitempty"` // last pool in top bar
}
```

No change to `HandleUpdatePrefs` — it already decodes the full struct and saves it.

---

## Feature 4 — Frontend: multi-pool globals + refreshPoolBar

### New globals (after `let datasetData = []`)

```js
let allPoolsData           = [];   // all Pool objects from GET /api/pools
let selectedPoolName       = null; // active pool in the Pool tab
let selectedTopBarPoolName = null; // active pool in the top capacity bar
```

### `refreshPoolBar()`
- Fetches `/api/pools` (replaces `/api/pool`) → `allPoolsData`
- If `selectedTopBarPoolName` is null or no longer in the list: restore from
  `currentUser.preferences.selected_top_bar_pool` or default to first pool
- Sets `poolData` = pool matching `selectedTopBarPoolName`
- Calls `renderPoolBar()`

### `renderPoolBar()` — topbar title
- Reads `<span id="topbar-pool-title">` (add id to HTML)
- Single pool (≤1): static text "Pool Capacity"
- Multiple pools: renders a green `<select id="topbar-pool-selector">` with all pool names,
  `onchange="switchTopBarPool(this.value)"`

---

## Feature 5 — Frontend: pool tab selector + "New Pool"

### `loadPool()`
- After `refreshPoolBar()`, resolves `selectedPoolName` from pref or first pool
- Calls `_renderPoolTabTitle()` to update `<h2 id="pool-page-title">`
- Finds pool in `allPoolsData` by `selectedPoolName`, passes it to `renderPoolExists()`

### `_renderPoolTabTitle()`
- Single pool or no pool: `h2.textContent = 'ZFS Pool'`
- Multiple pools: renders `<select id="pool-tab-selector">` styled in green (`var(--accent-3)`),
  `onchange="switchPoolTab(this.value)"`

### `switchPoolTab(name)`
- Sets `selectedPoolName = name`, calls `savePoolPrefs()`
- Fetches fresh pool list, finds pool by name, calls `renderPoolExists()`
- Calls `_renderPoolTabTitle()` to keep selector in sync

### `switchTopBarPool(name)`
- Sets `selectedTopBarPoolName = name`, updates `poolData`, calls `renderPoolBar()`, `savePoolPrefs()`

### `savePoolPrefs()`
- Fire-and-forget `PUT /api/prefs` with `selected_pool` + `selected_top_bar_pool`
- Updates `currentUser.preferences` in memory

### `_updatePoolInCache(pool)`
- Updates `allPoolsData[i]` in-place by pool name
- If `pool.name === selectedTopBarPoolName` → also update `poolData` and re-render bar

### `renderPoolExists()` — "New Pool" button
- Add `<button class="btn btn-primary btn-sm" onclick="openNewPool()">+ New Pool</button>`
  to the admin `pool-header-actions` (always shown when renderPoolExists is called because
  at least one pool already exists)

### `openNewPool()`
- Clears pool status interval
- Calls `renderPoolCreate(el, { backPoolName: selectedPoolName })`

### `renderPoolCreate(el, options)`
- Accepts optional `options = {}` second param
- When `options.backPoolName` is set, prepends
  `<button onclick="loadPool()">← poolName</button>` to `pool-header-actions`

### `_fetchPoolStatus()`
- Fetches `/api/pool/status?name=` + `encodeURIComponent(selectedPoolName || '')`

### Pool mutation fetch calls — add `pool` field
| Function | Added field |
|---|---|
| grow disks | `pool: selectedPoolName` |
| save pool settings | `pool: selectedPoolName` |
| add cache device | `pool: selectedPoolName` |
| remove cache device | `pool: selectedPoolName` |
| destroy pool | already sends `name` (which is the pool name) — backend accepts it |
| upgrade pool | `pool: selectedPoolName` (was empty body) |

After-success updates for settings / cache mutations: call `_updatePoolInCache(data)` +
`renderPoolExists(el, data)` instead of directly assigning `poolData = data`.

---

## Edge Cases

| Scenario | Behaviour |
|---|---|
| 0 pools | No dropdown anywhere. Pool tab shows create form. Top bar: "No pool configured". |
| 1 pool | Static text everywhere. "New Pool" button shown. |
| 2+ pools | Both selectors shown as green `<select>`. |
| Destroy last pool of the tab selection | `loadPool()` resets `selectedPoolName` to first remaining pool or create form. |
| Saved pref points to deleted pool | Stale name → `allPoolsData.find()` returns undefined → fallback to first pool. Pref updated on next switch. |
| Pool created via "New Pool" | After `_doCreatePool()` success: `refreshPoolBar()` + `loadPool()` re-initialise state; `selectedPoolName` resolves to the new pool (it becomes first in list… actually it might not be first). Let me handle: after create success, set `selectedPoolName = req.name`, then call `loadPool()`. |

---

## What Does NOT Change

- `GET /api/datasets` — returns all datasets across all pools (identified by `pool/name` paths); dataset tab unchanged in v3.4.0
- SMB / NFS shares — unchanged
- Scrub auto-scheduler — scrubs the first pool only (multi-pool scrub deferred)
- SMART, metrics RRD, alerts, binary update — unchanged
