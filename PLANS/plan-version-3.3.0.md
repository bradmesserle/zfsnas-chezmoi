# Version 3.3.0 — Plan

## Goals

1. **Pool Cache Config** — L2ARC cache device management modal in the Pool tab
2. **Disk serial number column** — dedicated Serial column in the Physical Disks table
3. **Dataset detail popup enrichment** — show Sync, Dedup, Case Sensitivity, Reservation, Comment when clicking a dataset in the pool capacity bar
4. **Dataset comment** — editable comment field (stored as ZFS user property `zfsnas:comment`), shown as a column in the datasets table
5. **Capacity reservation** — expose `refreservation` in dataset create/edit
6. **Sync mode** — expose `sync` property (Inherit, Standard, Always, Disabled) in dataset create/edit
7. **Deduplication** — expose `dedup` property (Inherit, Off, On, Verify) in dataset create/edit
8. **Record size** — expose `recordsize` property via dropdown in dataset create/edit

---

## Feature 1 — Pool Cache Config

### Problem
The pool has no UI for managing L2ARC cache devices (`zpool add <pool> cache <dev>` / `zpool remove <pool> <dev>`). Users have no way to see which disks are assigned as cache, add new ones, or remove them.

### Backend changes — `system/zfs.go`

**`Pool` struct:** add `CacheDevs []string \`json:"cache_devs"\``

**`GetPool()`:** call `poolCacheDevs(p.Name)` and assign to `p.CacheDevs`.

**`poolCacheDevs(poolName string) []string`:** parses `zpool status -P` output. Tracks state machine:
- Sets `inCache = true` when a line with a single token `"cache"` is encountered in the config section.
- Resets to `inCache = false` when any other section header (no valid state token) is encountered.
- Collects device paths for lines where `inCache` is true, `state` is a valid ZFS state, and name is not a virtual vdev prefix (`mirror-`, `raidz`, etc.).
- Calls `resolveDevPath()` on each collected path.

**`AddPoolCache(poolName, device string) error`:** runs `sudo zpool add <pool> cache <device>`.

**`RemovePoolCache(poolName, device string) error`:** runs `sudo zpool remove <pool> <device>`.

### Backend changes — `handlers/pools.go`

**`HandleAddPoolCache`** — `POST /api/pool/cache`:
- Decodes `{device: string}`.
- Gets pool, calls `system.AddPoolCache(pool.Name, req.Device)`.
- Audits via `ActionGrowPool` with `Details: "add cache <device>"`.
- Returns updated pool JSON.

**`HandleRemovePoolCache`** — `DELETE /api/pool/cache`:
- Same structure, calls `system.RemovePoolCache`.
- Audits with `Details: "remove cache <device>"`.
- Returns updated pool JSON.

### Backend changes — `handlers/router.go`

```go
r.Handle("/api/pool/cache",
    RequireAuth(RequireAdmin(http.HandlerFunc(HandleAddPoolCache)))).Methods("POST")
r.Handle("/api/pool/cache",
    RequireAuth(RequireAdmin(http.HandlerFunc(HandleRemovePoolCache)))).Methods("DELETE")
```

### Frontend changes — `static/index.html`

**Pool header buttons** (`renderPoolExists`): add `⚡ Cache Config` button before "Import ZFS Pool", admin-only.

**Modal `#modal-cache-config`:**
- Header: "⚡ L2ARC Cache Config"
- Large green info panel explaining: L2ARC purpose, best disk type (NVMe / high-endurance SSD), capacity guidance (≥ working set), warning that removing a cache device evicts all its data immediately (no data loss, but temporary latency increase).
- Section "Current Cache Devices": list of `pool.cache_devs` each with a Remove button. If empty, shows "No cache devices assigned."
- Section "Add Cache Device": dropdown of available (non-in-use) disks fetched fresh from `/api/disks`, filtered to exclude current cache devices. "+ Add as Cache" button.
- `#cache-alert` error banner.

**JS functions:**
- `openCacheConfigModal()` — clears alert, calls `_renderCacheModal()`, shows modal.
- `closeCacheConfigModal()` — hides modal.
- `_renderCacheModal()` — renders current list + fetches `/api/disks` to populate the add dropdown.
- `submitAddCache()` — `POST /api/pool/cache`, updates `poolData`, re-renders.
- `submitRemoveCache(device)` — confirms, `DELETE /api/pool/cache`, updates `poolData`, re-renders.

---

## Feature 2 — Disk Serial Number Column

### Problem
`DiskInfo.Serial` is already populated from `smartctl` and has `json:"serial"`. It was previously only displayed as sub-text inside the Vendor/Model cell. Users need a dedicated column for quick identification.

### Backend changes
None — serial is already fetched and serialised.

### Frontend changes — `static/index.html`

**`diskTableHeader()`:** add `<th>Serial</th>` after `<th>Vendor / Model</th>`.

**`diskTableHeaderWithPool()`:** same addition.

**`diskRow(d, dimmed)`:** add `<td>` with `d.serial` in monospace; show `—` if empty. Remove the serial sub-text from the Vendor/Model cell (it was there as a fallback).

**`diskRowWithPool(d, poolName)`:** same.

---

## Feature 3 & 4 — Dataset Extended Properties + Comment

Features 3–8 share the same backend plumbing and are described together for the backend, then separately for the UI.

### Backend changes — `system/zfs.go`

**`Dataset` struct:** add fields:
```go
Refreservation    uint64 `json:"refreservation"`
RecordSizeRaw     string `json:"record_size_raw"` // e.g. "128K", "inherit"
Sync              string `json:"sync"`
Dedup             string `json:"dedup"`
CaseSensitivity   string `json:"case_sensitivity"`
Comment           string `json:"comment"`
RefreservationStr string `json:"refreservation_str"`
```

**`DatasetCreateOptions` struct:**
```go
type DatasetCreateOptions struct {
    Quota           uint64
    QuotaType       string
    Refreservation  uint64
    Compression     string
    Sync            string
    Dedup           string
    CaseSensitivity string
    RecordSize      string  // raw ZFS value: "128K", "1M", "inherit", ""
    Comment         string
}
```

**`ListDatasets`:** extend `-o` properties list to:
```
name,used,avail,refer,quota,refquota,compression,compressratio,recordsize,mountpoint,
sync,dedup,casesensitivity,refreservation,zfsnas:comment
```

**`parseDatasetLine`:** update field count check from `< 10` to `< 15`; parse fields 10–14 as `sync`, `dedup`, `caseSensitivity`, `refreservation`, `comment`. Normalize comment: if `"-"` → `""`.

**`formatBytesShort(b uint64) string`:** converts bytes to compact ZFS notation (`128K`, `1M`, etc.) used for `RecordSizeRaw`. Returns `"inherit"` for 0.

**`CreateDataset(name string, opts DatasetCreateOptions) error`:** replaces old `(name, quota, quotaType, compression)` signature:
- Appends `-o` args for each non-empty / non-inherit option.
- `casesensitivity` is set via `-o` at create time (cannot be changed later).
- After `zfs create`, calls `SetDatasetProps(name, map{"zfsnas:comment": opts.Comment})` if comment non-empty.

**`SetDatasetProps`:** updated so that a value of `""` calls `zfs inherit <prop> <name>` instead of `zfs set`, allowing user properties to be cleared.

### Backend changes — `handlers/datasets.go`

**`HandleCreateDataset`:** extend request struct with `Refreservation`, `Compression`, `Sync`, `Dedup`, `CaseSensitivity`, `RecordSize`, `Comment`. Build `DatasetCreateOptions` and pass to `system.CreateDataset`.

**`HandleUpdateDataset`:** extend request struct with `Refreservation *uint64`, `Sync`, `Dedup`, `RecordSize`, `Comment *string`. Add all new fields to `props` map:
- `refreservation`: `"none"` if 0, else bytes string.
- `recordsize`: pass raw value (`"128K"`, `"inherit"`, etc.).
- `sync` / `dedup`: pass as-is.
- `zfsnas:comment`: `""` triggers `zfs inherit` (clears property); non-empty sets it.

### Frontend changes — Dataset detail modal (`#modal-dataset-detail`)

Add stat rows after Record size and before Mountpoint:
- Reservation: `id="dsd-refreserv"`
- Sync mode: `id="dsd-sync"`
- Deduplication: `id="dsd-dedup"`
- Case sensitivity: `id="dsd-case"`
- Comment: `id="dsd-comment"` in a conditionally shown row (`id="dsd-comment-row"`, hidden when empty).

Update `openDatasetDetail(name)` to populate new fields from dataset object.

### Frontend changes — Datasets table

Add `<th>Comment</th>` header column. In row rendering, add `<td>` showing comment text truncated to ~160px with `text-overflow:ellipsis` and `title` attribute for full text on hover. Shows `—` when empty.

### Frontend changes — Create Dataset modal (`#modal-create-dataset`)

New fields added:
1. **Capacity Reservation** — amount input + MB/GB/TB unit selector. Info `ℹ` tooltip explaining reserved space semantics.
2. **Compression** — existing field, now in a 2-col grid.
3. **Record Size** — dropdown (see full list below). Info `ℹ` tooltip explaining block size / workload matching.
4. **Sync Mode** — dropdown: Inherit / Standard / Always / Disabled. Info `ℹ` tooltip.
5. **Deduplication** — dropdown: Inherit / Off / On / Verify. Info `ℹ` tooltip warning about RAM requirements.
6. **Case Sensitivity** — dropdown: Sensitive (Linux default) / Insensitive (Windows/SMB) / Mixed. Info `ℹ` tooltip noting this is create-only. No "inherit" option since it must be explicit at creation.
7. **Comment** — textarea (2 rows).

Items 3–6 rendered in a `2×2` grid for compactness.

`openCreateDataset()` resets all new fields to defaults.
`submitCreateDataset()` collects and includes all new fields in the `POST /api/datasets` body.

### Frontend changes — Edit Dataset modal (`#modal-edit-dataset`)

New fields added (same set as create, minus Case Sensitivity — immutable after creation):
1. **Capacity Reservation** — amount + unit, pre-populated from `d.refreservation`.
2. **Compression** — existing.
3. **Record Size** — dropdown, pre-selected from `d.record_size_raw`.
4. **Sync Mode** — dropdown, pre-selected from `d.sync`.
5. **Deduplication** — dropdown, pre-selected from `d.dedup`.
6. **Comment** — textarea, pre-populated from `d.comment`.

`openEditDataset(name, d)` signature changes from `(name, quotaStr, comp)` to `(name, datasetObj)`. Quota is parsed back from bytes to amount+unit. All new fields populated from `d`.
`submitEditDataset()` sends all new fields in the `PUT /api/datasets/{path}` body.

The Edit button in the datasets table row changes from:
```js
openEditDataset('${d.name}','${d.quota_str}','${d.compression}')
```
to:
```js
openEditDataset('${d.name}', ${JSON.stringify(d)})
```

---

## Record Size Dropdown Values

Used in both Create and Edit modals (`#cds-recordsize`, `#eds-recordsize`):

| Display | Value sent to API |
|---------|------------------|
| Inherit (pool default) | `inherit` |
| 512 B | `512` |
| 1 KiB | `1K` |
| 2 KiB | `2K` |
| 4 KiB | `4K` |
| 8 KiB | `8K` |
| 16 KiB | `16K` |
| 32 KiB | `32K` |
| 64 KiB | `64K` |
| 128 KiB | `128K` |
| 256 KiB | `256K` |
| 512 KiB | `512K` |
| 1 MiB | `1M` |
| 2 MiB | `2M` |
| 4 MiB | `4M` |
| 8 MiB | `8M` |
| 16 MiB | `16M` |

`RecordSizeRaw` from the backend (`formatBytesShort`) maps the stored bytes value to one of these option values so the dropdown pre-selects correctly on edit.

---

## Info Tooltip Text Reference

### Capacity Reservation
> Reserves guaranteed space on the pool exclusively for this dataset's own data. Other datasets cannot use this reserved space even when the dataset is not using it.
>
> Use sparingly — reserved space is permanently deducted from pool free space. Useful for databases or critical datasets that must always have room to grow.

### Record Size
> The block size ZFS uses for this dataset. Smaller values (4K–16K) suit databases and random-access workloads. Larger values (128K–1M) suit media files and sequential workloads. A mismatch causes read/write amplification. Only affects new data — existing blocks are not re-chunked.
>
> Inherit: uses the pool default (usually 128K).

### Sync Mode
> Controls when writes are committed to stable storage.
>
> **Standard:** writes go to the ZFS Intent Log first; flushed on fsync() calls — good balance of safety and speed.
> **Always:** every write waits for disk commit — safest, but slowest. Use for databases that require strict durability.
> **Disabled:** no sync semantics — fastest, but risks data loss on sudden power failure. Never use for critical data without a UPS.
> **Inherit:** uses the parent dataset or pool setting.

### Deduplication
> Deduplication detects identical blocks and stores them only once, saving space when data is highly repetitive (e.g. VMs sharing the same OS image).
>
> **Warning:** ZFS must keep the entire dedup table (DDT) in RAM (~320 bytes per unique block). With insufficient RAM, performance degrades severely. Only enable if you have ample RAM and genuinely duplicated data.
>
> **Verify:** checksums each dedup hit to guard against hash collisions. Safer but slower.

### Case Sensitivity
> Determines how filename lookups handle uppercase/lowercase.
>
> **Sensitive** (default on Linux): 'File.txt' and 'file.txt' are different files.
> **Insensitive:** 'File.txt' and 'file.txt' refer to the same file — required for Windows SMB clients that expect case-insensitive behaviour.
> **Mixed:** case-insensitive lookups but case-preserving storage.
>
> This setting cannot be changed after the dataset is created.

### L2ARC Cache (modal info panel)
> **L2ARC (Level 2 Adaptive Replacement Cache)** is a read cache stored on a fast device (SSD or NVMe). ZFS automatically promotes hot data to L2ARC to reduce latency for repeated reads.
>
> **Best disk type:** NVMe or high-endurance SSD. Capacity ideally ≥ your working data set (not RAM). Low-endurance SSDs will wear out quickly under heavy random read workloads.
>
> **Warning:** Removing a cache device immediately evicts all its cached data — this does not cause data loss but may temporarily increase read latency.

---

## Files Changed Summary

| File | Change |
|------|--------|
| `system/zfs.go` | `Pool.CacheDevs`; `poolCacheDevs()`; `AddPoolCache()`; `RemovePoolCache()`; `Dataset` new fields; `DatasetCreateOptions` struct; `ListDatasets` extended `-o`; `parseDatasetLine` extended; `CreateDataset` new signature; `SetDatasetProps` `""` → `zfs inherit`; `formatBytesShort()` |
| `handlers/datasets.go` | `HandleCreateDataset` extended request + `DatasetCreateOptions`; `HandleUpdateDataset` extended request + props map |
| `handlers/pools.go` | `HandleAddPoolCache`, `HandleRemovePoolCache` |
| `handlers/router.go` | `POST /api/pool/cache`, `DELETE /api/pool/cache` |
| `static/index.html` | Cache Config button + modal + JS; Serial column in disk tables; Dataset detail modal new rows; Datasets table Comment column; Create/Edit dataset modals new fields; `openEditDataset` signature change |

---

## Notes / Edge Cases

- **`casesensitivity` immutability:** ZFS does not allow changing this property after creation. It is exposed in the Create modal only. The Edit modal omits it. The Detail modal shows the current value as read-only.
- **`zfsnas:comment` vs standard ZFS comment:** ZFS has no built-in "comment" property for filesystems (only for pools). We use the user-defined property `zfsnas:comment`. It survives pool import/export and is visible via `zfs get zfsnas:comment`.
- **Clearing a comment:** Setting `zfsnas:comment` to `""` calls `zfs inherit zfsnas:comment <dataset>` which removes the property entirely (returns `"-"` on next `zfs list`). This is correct — stored `"-"` is normalised to `""` in parsing.
- **`recordsize` on existing data:** ZFS applies the new record size only to data written after the change. Pre-existing blocks remain at their original size. This is documented in the info tooltip.
- **`dedup` on existing data:** Similarly, enabling dedup does not retroactively dedup existing blocks. Only new writes are subject to deduplication.
- **`refreservation` vs `reservation`:** We expose only `refreservation` (counts only the dataset's own data, excludes snapshots/children) as it is the safer and more commonly useful setting. `reservation` (includes snapshots) is not exposed.
- **L2ARC removal latency:** When `RemovePoolCache` is called, ZFS removes the device immediately. The in-flight eviction is handled by ZFS in the background. The portal confirms the action with a warning dialog.
- **Pool not available:** Cache endpoints return 400 if no pool is configured.
- **Available disks for cache:** `_renderCacheModal()` fetches `/api/disks` and filters `d.in_use === false`. It also excludes devices already in `pool.cache_devs` using a Set for O(1) lookup.
- **`record_size_raw` matching dropdown:** `formatBytesShort(131072)` → `"128K"`, which matches the `<option value="128K">` exactly, so `select.value = d.record_size_raw` pre-selects correctly.
