# Version 6.1.0 Plan — ZVols & iSCSI Sharing

## Overview

Three major deliverable areas:

1. **ZVol Management** — Create, list, edit, snapshot, and delete ZFS volumes (zvols) directly from the Datasets page. ZVols appear inline in the dataset tables with a distinct disk icon, and all management actions (edit, snapshot, delete) are accessible per-row.

2. **iSCSI Sharing** — New iSCSI section in the Sharing sidebar. Greyed out until prerequisites are installed; when installed, shows service controls (start/stop/restart/configure), a global settings popup, a host registry, and per-share creation backed by ZVols.

3. **Prerequisites Update** — targetcli-fb (and related packages) added to the Prerequisites page with "greyed out / installable" styling and an "Enable iSCSI Feature" button — distinct from the existing yellow "not installed" alert.

No new Go module dependencies — stdlib + gorilla/mux + gorilla/websocket only.

---

## Feature 1: ZVol Management

### 1.1 Backend — `system/zfs.go`

#### New Struct

```go
// ZVol represents a ZFS volume (block device).
type ZVol struct {
    Name        string `json:"name"`         // full path: pool/vol or pool/parent/vol
    Pool        string `json:"pool"`
    Size        uint64 `json:"size"`          // volsize in bytes
    Used        uint64 `json:"used"`
    Refer       uint64 `json:"refer"`
    Compression string `json:"compression"`
    CompRatio   string `json:"comp_ratio"`
    Sync        string `json:"sync"`
    Dedup       string `json:"dedup"`
    VolBlockSize string `json:"volblocksize"`
    Encrypted   bool   `json:"encrypted"`
    Comment     string `json:"comment"`       // org.freebsd:swap or zfsnas:comment property
    DevPath     string `json:"dev_path"`      // /dev/zvol/<name>
}
```

#### `ListAllZVols() ([]ZVol, error)`

Runs `zfs list -t volume -H -p -o name,volsize,used,refer,compression,compressratio,sync,dedup,volblocksize,encryption,org.zfsnas:comment` across all imported pools.

- Parse each line into a `ZVol`.
- Derive `Pool` from the first path component.
- Derive `DevPath` as `/dev/zvol/<name>`.
- `Encrypted` = true when the `encryption` column is not `off`.
- `Comment` from the `org.zfsnas:comment` property (returns `-` if unset; store empty string in that case).

#### `CreateZVol(req ZVolCreateRequest) error`

```go
type ZVolCreateRequest struct {
    Parent      string `json:"parent"`       // pool or pool/dataset; ZVol is created under this
    Name        string `json:"name"`         // leaf name only
    Size        string `json:"size"`         // human string: "10G", "500M", etc.
    Comment     string `json:"comment"`
    Sync        string `json:"sync"`         // "" = inherit, "standard", "always", "disabled"
    Compression string `json:"compression"`  // "" = inherit, "lz4", "zstd", etc.
    Dedup       string `json:"dedup"`        // "" = inherit, "on", "off", "verify"
    BlockSize   string `json:"block_size"`   // "" = inherit, "4K", "8K", "16K", "32K", "64K", "128K"
    Encryption  string `json:"encryption"`   // "" = inherit, "enabled"
    KeyName     string `json:"key_name"`     // required when Encryption="enabled"
}
```

Builds the `zfs create -V <size> [-o ...] <parent>/<name>` command:

- `-o volblocksize=<block_size>` only when BlockSize is not empty/inherit.
- `-o sync=<sync>` only when Sync is not empty/inherit.
- `-o compression=<compression>` only when Compression is not empty/inherit.
- `-o dedup=<dedup>` only when Dedup is not empty/inherit.
- When Encryption="enabled": `-o encryption=aes-256-gcm -o keyformat=raw -o keylocation=file:///.../<KeyName>` (same key-file path as used for encrypted datasets in v5.0.0).
- After creation, if Comment is not empty: `zfs set org.zfsnas:comment=<comment> <fullName>`.
- Returns the error from `exec.Command`.

#### `EditZVol(req ZVolEditRequest) error`

```go
type ZVolEditRequest struct {
    Name        string `json:"name"`         // full zvol path
    Comment     string `json:"comment"`
    Sync        string `json:"sync"`
    Compression string `json:"compression"`
    Dedup       string `json:"dedup"`
}
```

Applies changes via `zfs set` for each non-empty/changed property. No size change in v6.1 (resize is out of scope).

#### `DeleteZVol(name string) error`

Runs `zfs destroy -r <name>`. Returns error verbatim.

#### `GetZVolCompressionsSupported() []string`

Runs `zfs get -H compression <any pool>` to confirm availability, then returns the static list:
`["lz4", "zstd", "zstd-1", "zstd-3", "zstd-6", "zstd-9", "gzip", "gzip-1", "gzip-9", "lzjb", "none"]`

(Same list already used for dataset creation — reuse the existing helper or inline.)

---

### 1.2 New Handler File: `handlers/zvol.go`

#### `HandleListZVols` — `GET /api/zvols`

```go
func HandleListZVols(w http.ResponseWriter, r *http.Request) {
    zvols, err := system.ListAllZVols()
    if err != nil {
        jsonErr(w, 500, err.Error())
        return
    }
    jsonOK(w, zvols)
}
```

#### `HandleCreateZVol` — `POST /api/zvol/create`

Decode `ZVolCreateRequest`, validate (non-empty name, valid size string, key present when encryption enabled), call `system.CreateZVol`. Write audit entry `audit.ActionCreateZVol`.

#### `HandleEditZVol` — `POST /api/zvol/edit`

Decode `ZVolEditRequest`, validate, call `system.EditZVol`. Write audit entry `audit.ActionEditZVol`.

#### `HandleDeleteZVol` — `POST /api/zvol/delete`

Body: `{ "name": "pool/vol" }`. Call `system.DeleteZVol`. Write audit entry `audit.ActionDeleteZVol`.

ZVol snapshots reuse the existing `HandleCreateSnapshot` / `HandleListSnapshots` / `HandleDeleteSnapshot` handlers — they already work on any ZFS dataset or volume path.

---

### 1.3 Router Changes (`handlers/router.go`)

```go
r.Handle("/api/zvols",
    RequireAuth(http.HandlerFunc(HandleListZVols))).Methods("GET")
r.Handle("/api/zvol/create",
    RequireAdmin(http.HandlerFunc(HandleCreateZVol))).Methods("POST")
r.Handle("/api/zvol/edit",
    RequireAdmin(http.HandlerFunc(HandleEditZVol))).Methods("POST")
r.Handle("/api/zvol/delete",
    RequireAdmin(http.HandlerFunc(HandleDeleteZVol))).Methods("POST")
```

---

### 1.4 Audit Constants (`internal/audit/audit.go`)

```go
const (
    ActionCreateZVol  = "create_zvol"
    ActionEditZVol    = "edit_zvol"
    ActionDeleteZVol  = "delete_zvol"
)
```

---

### 1.5 Frontend — "Create ZVol" Button & Modal

#### Button placement

In the Datasets page header (alongside the existing "Create Dataset" button):

```html
<button class="btn-primary" onclick="openCreateZVolModal()" style="background:rgba(100,210,255,0.15);border:1px solid rgba(100,210,255,0.4);color:#64d2ff;">
  + Create ZVol
</button>
```

#### Create ZVol Modal

A full-width modal (`modal-lg`) with these sections:

**Section 1 — Identity**

| Field | Control | Notes |
|-------|---------|-------|
| Parent | `<select>` tree | Lists the selected pool at top, then all its child datasets (indented). Default = currently selected pool root |
| Name | `<input>` text | Leaf name only; validated as alphanumeric+dash+underscore |
| Size | `<input>` text | e.g. `10G`, `500M`; show info tip: "Suffix: K, M, G, T, P" |
| Comment | `<input>` text | Optional |

**Section 2 — Parameters** (always visible)

| Parameter | Control | Details |
|-----------|---------|---------|
| Sync | `<select>` | `Inherit` (default), `Standard`, `Always`, `Disabled` |
| Compression | `<select>` | `Inherit` (default), then each supported type with a short note (see table below) |
| Deduplication | `<select>` | `Inherit` (default), `On`, `Off`, `Verify` |
| Block Size | `<select>` | `Inherit` (default), `4 KiB`, `8 KiB`, `16 KiB`, `32 KiB`, `64 KiB`, `128 KiB` |

**Compression option labels:**

| Value | Label shown in dropdown |
|-------|------------------------|
| `lz4` | `LZ4 — fast, lightweight, recommended default` |
| `zstd` | `ZSTD — balanced speed & ratio` |
| `zstd-1` | `ZSTD-1 — fast with good ratio` |
| `zstd-3` | `ZSTD-3 — moderate ratio, moderate speed` |
| `zstd-6` | `ZSTD-6 — high ratio, slower` |
| `zstd-9` | `ZSTD-9 — maximum ratio, slow` |
| `gzip` | `Gzip — classic, broad compatibility` |
| `gzip-1` | `Gzip-1 — fastest gzip` |
| `gzip-9` | `Gzip-9 — best gzip compression` |
| `lzjb` | `LZJB — legacy, low overhead` |
| `none` | `None — no compression` |

**Block Size option labels:**

| Value | Label shown |
|-------|------------|
| `4K` | `4 KiB — databases, random I/O, VM disks` |
| `8K` | `8 KiB — general purpose` |
| `16K` | `16 KiB — mixed workloads` |
| `32K` | `32 KiB — moderate sequential` |
| `64K` | `64 KiB — sequential, backups` |
| `128K` | `128 KiB — large sequential, NFS/iSCSI` |

**Section 3 — Advanced Security** (collapsible `<details>` element, collapsed by default)

| Parameter | Control | Details |
|-----------|---------|---------|
| Encryption | `<select>` | `Inherit` (default), `Enabled` |
| Encryption Key | `<select>` (shown only when Enabled) | Lists all key names from the key store |
| Manage Keys | `<button>` (shown only when Enabled) | Opens the existing key-management popup |

**Footer buttons:** `Cancel` | `Create ZVol`

#### `openCreateZVolModal()`

```js
async function openCreateZVolModal() {
    // Populate parent <select> with pool root + child datasets for selectedPoolName
    // Fetch /api/datasets?pool=<selectedPoolName> and build option list
    // Fetch /api/keys (existing endpoint) to populate key dropdown
    // Reset all fields to defaults
    // Show modal
}
```

#### `submitCreateZVol()`

```js
async function submitCreateZVol() {
    const body = {
        parent:      document.getElementById('zvol-parent').value,
        name:        document.getElementById('zvol-name').value.trim(),
        size:        document.getElementById('zvol-size').value.trim(),
        comment:     document.getElementById('zvol-comment').value.trim(),
        sync:        document.getElementById('zvol-sync').value,
        compression: document.getElementById('zvol-compression').value,
        dedup:       document.getElementById('zvol-dedup').value,
        block_size:  document.getElementById('zvol-blocksize').value,
        encryption:  document.getElementById('zvol-encryption').value,
        key_name:    document.getElementById('zvol-key').value,
    };
    // POST /api/zvol/create
    // On success: close modal, reload dataset/zvol list, show activity bar message
}
```

---

### 1.6 Frontend — ZVols in Dataset Tables

#### `loadZVols()` / integration with `loadDatasets()`

After loading datasets, call `GET /api/zvols` and merge results into the same table. ZVols appear after the datasets in each pool section (or interspersed based on parent path, whichever matches the existing ordering logic).

#### Row rendering differences

- **Icon**: Use the same disk SVG icon (`💽` or the existing physical disk icon class) instead of the dataset folder icon.
- **Type badge**: A small grey `ZVOL` badge inline with the name.
- **Size column**: Show `volsize` formatted with `formatBytes()` instead of `used/avail`.
- **No children**: ZVols are never expanded (no child datasets).

```html
<!-- ZVol row example -->
<tr class="zvol-row" data-zvol="pool/volname">
  <td>
    <span class="disk-icon">🖴</span>
    <span>volname</span>
    <span class="badge-grey" style="font-size:10px;margin-left:6px;">ZVOL</span>
    <!-- lock icon if encrypted -->
  </td>
  <td><!-- size --></td>
  <td><!-- used --></td>
  <td><!-- compression --></td>
  <td><!-- comment --></td>
  <td>
    <button onclick="openEditZVolModal('pool/volname')">Edit</button>
    <div class="burger-menu">
      <button onclick="openSnapshotModal('pool/volname')">Snapshot</button>
      <button class="danger" onclick="confirmDeleteZVol('pool/volname')">Delete ZVol</button>
    </div>
  </td>
</tr>
```

#### Edit ZVol Modal

Simpler modal (no parent, no size, no block size — only comment, sync, compression, dedup). Pre-populated from the current values of the ZVol. Same pattern as the existing Edit Dataset modal.

#### Delete ZVol Confirmation Modal

A distinct danger modal with red accent:

```html
<div class="modal-header" style="border-bottom:2px solid #ff453a;">
  <span style="color:#ff453a;font-size:20px;">⚠ Delete ZVol</span>
</div>
<div class="modal-body">
  <p style="color:var(--text);font-size:15px;line-height:1.6;">
    You are about to permanently destroy
    <strong style="color:#ff453a;">pool/volname</strong>
    and <strong>all its snapshots</strong>.
  </p>
  <p style="color:#ff9f0a;font-size:13px;margin-top:12px;">
    ⚠ This action is <strong>irreversible</strong>. All data stored in this ZVol will be lost.
  </p>
  <p style="margin-top:16px;font-size:13px;color:var(--text-2);">
    Type the ZVol name to confirm:
  </p>
  <input id="zvol-delete-confirm-input" type="text" placeholder="pool/volname"
         style="width:100%;margin-top:6px;" />
</div>
<div class="modal-footer">
  <button onclick="closeModal()">Cancel</button>
  <button id="zvol-delete-confirm-btn" class="btn-danger" disabled
          onclick="executeDeleteZVol()">Delete ZVol</button>
</div>
```

The "Delete ZVol" button stays disabled until the typed name matches exactly.

---

## Feature 2: iSCSI Sharing

### 2.1 Config Additions (`internal/config/config.go`)

```go
// ISCSIHost is a known initiator that can be granted access to iSCSI shares.
type ISCSIHost struct {
    ID      string `json:"id"`      // UUID
    Name    string `json:"name"`    // friendly label
    IQN     string `json:"iqn"`     // iqn.yyyy-mm.com.example:hostname
    Comment string `json:"comment"`
}

// ISCSIShare is a single exported iSCSI target backed by a ZVol.
type ISCSIShare struct {
    ID        string   `json:"id"`          // UUID
    ZVol      string   `json:"zvol"`        // full ZVol path: pool/name
    IQN       string   `json:"iqn"`         // generated target IQN
    HostIDs   []string `json:"host_ids"`    // allowed hosts; empty = any
    Comment   string   `json:"comment"`
    CreatedAt int64    `json:"created_at"`
}

// ISCSIConfig holds all persistent iSCSI settings.
type ISCSIConfig struct {
    Enabled  bool         `json:"enabled"`   // true once prerequisites installed + user enabled
    BaseName string       `json:"base_name"` // e.g. "iqn.2003-06.ca.chezmoi.zfsnas"
    Port     int          `json:"port"`      // default 3260
    Hosts    []ISCSIHost  `json:"hosts"`
    Shares   []ISCSIShare `json:"shares"`
}
```

Add `ISCSI ISCSIConfig` to the `AppConfig` struct. Populate default `BaseName = "iqn.2003-06.ca.chezmoi.zfsnas"` and `Port = 3260` in `DefaultAppConfig()`.

---

### 2.2 Backend — `system/iscsi.go`

New file `system/iscsi.go`:

#### Prerequisites detection

```go
// ISCSIPrereqsInstalled returns true when targetcli-fb is present on the system.
func ISCSIPrereqsInstalled() bool {
    _, err := exec.LookPath("targetcli")
    return err == nil
}
```

Also check for `tgt` or `iscsitarget` as alternatives — but the primary package is `targetcli-fb`.

#### Service control

```go
type ISCSIServiceStatus struct {
    Active  bool   `json:"active"`
    Status  string `json:"status"`  // "active", "inactive", "failed", "unknown"
}

func GetISCSIServiceStatus() ISCSIServiceStatus
func StartISCSIService() error
func StopISCSIService() error
func RestartISCSIService() error
```

All implemented via `exec.Command("systemctl", action, "targetclid")` (targetcli-fb daemon) or `"rtslib-fb-targetctl"` depending on what the installed package provides.

Use `systemctl is-active targetclid` to detect service name — fall back to `tgt` if targetclid is absent.

#### Target management via `targetcli`

```go
// ApplyISCSIConfig generates a targetcli saveconfig-compatible JSON and applies it.
// Called after any create/delete share operation and after Configure is saved.
func ApplyISCSIConfig(cfg *config.ISCSIConfig, serverIPs []string) error
```

Implementation outline:
1. For each `ISCSIShare` in `cfg.Shares`:
   - Create backstore: `targetcli /backstores/block create name=<shareID> dev=/dev/zvol/<zvol>`
   - Create target: `targetcli /iscsi create <iqn>`
   - Create portal on every server IP: `targetcli /iscsi/<iqn>/tpg1/portals create <ip> <port>`
   - Delete default portal `0.0.0.0:3260` if we added specific IP portals, or keep `0.0.0.0` for "all interfaces" behaviour.
   - Create LUN: `targetcli /iscsi/<iqn>/tpg1/luns create /backstores/block/<shareID>`
   - For each allowed host in `HostIDs`: `targetcli /iscsi/<iqn>/tpg1/acls create <hostIQN>`
   - If `HostIDs` is empty, set `generate_node_acls 1` on the TPG (any host allowed).
2. Run `targetcli saveconfig` at the end.

For simplicity in v6.1, `ApplyISCSIConfig` does a full teardown-and-rebuild:
- Run `targetcli clearconfig confirm=True` first, then rebuild from scratch based on config.

This is safe because the config is the source of truth; the targetcli state is derived.

#### IQN generation

```go
// GenerateTargetIQN returns a unique target IQN for a share.
// Format: <baseName>:<shareID-short>
func GenerateTargetIQN(baseName, shareID string) string {
    return baseName + ":" + shareID[:8]
}
```

---

### 2.3 New Handler File: `handlers/iscsi.go`

#### `HandleISCSIStatus` — `GET /api/iscsi/status`

```json
{
  "prereqs_installed": true,
  "service_active": true,
  "service_status": "active"
}
```

#### `HandleISCSIServiceAction` — `POST /api/iscsi/service`

Body: `{ "action": "start" | "stop" | "restart" }`. Calls the appropriate `system.StartISCSIService()` etc. Returns `{ "ok": true }` or error.

#### `HandleGetISCSIConfig` — `GET /api/iscsi/config`

Returns `AppConfig.ISCSI` (sans full host/share arrays — those have their own endpoints). Returns base_name, port, enabled.

#### `HandleSaveISCSIConfig` — `POST /api/iscsi/config`

Body: `{ "base_name": "...", "port": 3260 }`. Validates port range (1–65535), saves to config, calls `system.ApplyISCSIConfig`. Writes audit entry `audit.ActionEditISCSIConfig`.

#### `HandleListISCSIHosts` — `GET /api/iscsi/hosts`

Returns `AppConfig.ISCSI.Hosts`.

#### `HandleSaveISCSIHost` — `POST /api/iscsi/host`

Upsert a host (create if no `id`, update if `id` present). Validates IQN format. Returns saved host with `id` populated (UUID generated on create). Does NOT call `ApplyISCSIConfig` — hosts are reference data only.

#### `HandleDeleteISCSIHost` — `POST /api/iscsi/host/delete`

Body: `{ "id": "..." }`. Removes host from config. Checks the host is not in use by any share — returns 409 if it is.

#### `HandleListISCSIShares` — `GET /api/iscsi/shares`

Returns `AppConfig.ISCSI.Shares`.

#### `HandleCreateISCSIShare` — `POST /api/iscsi/share/create`

```go
type ISCSIShareCreateRequest struct {
    ZVol    string   `json:"zvol"`
    HostIDs []string `json:"host_ids"`
    Comment string   `json:"comment"`
}
```

Validates that `zvol` exists (call `system.ListAllZVols()`), generates IQN via `GenerateTargetIQN`, appends to `AppConfig.ISCSI.Shares`, saves config, calls `system.ApplyISCSIConfig`. Writes audit entry `audit.ActionCreateISCSIShare`.

#### `HandleDeleteISCSIShare` — `POST /api/iscsi/share/delete`

Body: `{ "id": "..." }`. Removes share, saves config, calls `system.ApplyISCSIConfig`. Writes audit entry `audit.ActionDeleteISCSIShare`.

---

### 2.4 Router Changes (`handlers/router.go`)

```go
r.Handle("/api/iscsi/status",
    RequireAuth(http.HandlerFunc(HandleISCSIStatus))).Methods("GET")
r.Handle("/api/iscsi/service",
    RequireAdmin(http.HandlerFunc(HandleISCSIServiceAction))).Methods("POST")
r.Handle("/api/iscsi/config",
    RequireAuth(http.HandlerFunc(HandleGetISCSIConfig))).Methods("GET")
r.Handle("/api/iscsi/config",
    RequireAdmin(http.HandlerFunc(HandleSaveISCSIConfig))).Methods("POST")
r.Handle("/api/iscsi/hosts",
    RequireAuth(http.HandlerFunc(HandleListISCSIHosts))).Methods("GET")
r.Handle("/api/iscsi/host",
    RequireAdmin(http.HandlerFunc(HandleSaveISCSIHost))).Methods("POST")
r.Handle("/api/iscsi/host/delete",
    RequireAdmin(http.HandlerFunc(HandleDeleteISCSIHost))).Methods("POST")
r.Handle("/api/iscsi/shares",
    RequireAuth(http.HandlerFunc(HandleListISCSIShares))).Methods("GET")
r.Handle("/api/iscsi/share/create",
    RequireAdmin(http.HandlerFunc(HandleCreateISCSIShare))).Methods("POST")
r.Handle("/api/iscsi/share/delete",
    RequireAdmin(http.HandlerFunc(HandleDeleteISCSIShare))).Methods("POST")
```

---

### 2.5 Audit Constants

```go
const (
    ActionEditISCSIConfig   = "edit_iscsi_config"
    ActionCreateISCSIShare  = "create_iscsi_share"
    ActionDeleteISCSIShare  = "delete_iscsi_share"
)
```

---

### 2.6 Frontend — iSCSI Sidebar & Page

#### Sidebar entry (`static/index.html`)

In the Sharing section of the left nav, after the existing NFS nav item:

```html
<button class="nav-item" id="nav-iscsi" onclick="showPage('iscsi')"
        style="opacity:0.4;cursor:default;" disabled>
  <span class="nav-icon">🖧</span><span class="nav-label"> iSCSI</span>
</button>
```

On `initApp()` / `loadISCSIStatus()`, fetch `GET /api/iscsi/status`:
- If `prereqs_installed === false`: keep button dimmed and disabled, show a small `(not installed)` sub-label.
- If `prereqs_installed === true`: enable the button (remove opacity + disabled).

#### iSCSI Page HTML Structure

```html
<div class="page" id="page-iscsi">

  <!-- Prerequisites banner (shown only when prereqs missing) -->
  <div id="iscsi-prereqs-banner" class="hidden card-sm" style="padding:16px;margin-bottom:16px;border:1px solid var(--border);opacity:0.7;">
    <span style="font-size:14px;color:var(--text-2);">
      iSCSI requires <strong>targetcli-fb</strong> to be installed.
      Go to <a onclick="showPage('prerequisites')" style="color:var(--accent);cursor:pointer;">Prerequisites</a> to enable this feature.
    </span>
  </div>

  <!-- Service control bar (shown only when prereqs installed) -->
  <div id="iscsi-service-bar" class="hidden" style="display:flex;align-items:center;gap:12px;margin-bottom:20px;flex-wrap:wrap;">
    <div style="font-size:18px;font-weight:700;color:var(--text);">iSCSI Sharing</div>
    <span id="iscsi-service-badge" class="badge-grey">unknown</span>
    <div style="display:flex;gap:8px;margin-left:auto;">
      <button class="btn-outline" onclick="iscsiServiceAction('start')">Start</button>
      <button class="btn-outline" onclick="iscsiServiceAction('stop')">Stop</button>
      <button class="btn-outline" onclick="iscsiServiceAction('restart')">Restart</button>
      <button class="btn-primary" onclick="openISCSIConfigModal()">⚙ Configure iSCSI</button>
    </div>
  </div>

  <!-- Shares table header -->
  <div id="iscsi-content" class="hidden">
    <div style="display:flex;align-items:center;margin-bottom:12px;">
      <span style="font-size:15px;font-weight:700;color:var(--text-2);">iSCSI Shares</span>
      <button class="btn-primary" onclick="openNewISCSIShareModal()" style="margin-left:auto;">+ New iSCSI Share</button>
    </div>
    <table class="data-table" id="iscsi-shares-table">
      <thead>
        <tr>
          <th>IQN</th>
          <th>ZVol</th>
          <th>Allowed Hosts</th>
          <th>Comment</th>
          <th></th>
        </tr>
      </thead>
      <tbody id="iscsi-shares-body"></tbody>
    </table>
  </div>

</div>
```

#### `loadISCSIPage()`

```js
async function loadISCSIPage() {
    const r = await fetch('/api/iscsi/status');
    if (!r.ok) return;
    const s = await r.json();

    const prereqsBanner = document.getElementById('iscsi-prereqs-banner');
    const serviceBar    = document.getElementById('iscsi-service-bar');
    const content       = document.getElementById('iscsi-content');

    if (!s.prereqs_installed) {
        prereqsBanner.classList.remove('hidden');
        serviceBar.classList.add('hidden');
        content.classList.add('hidden');
        return;
    }

    prereqsBanner.classList.add('hidden');
    serviceBar.classList.remove('hidden');
    content.classList.remove('hidden');

    // Update service badge
    const badge = document.getElementById('iscsi-service-badge');
    badge.textContent = s.service_status;
    badge.className = s.service_active ? 'badge-green' : 'badge-grey';

    await loadISCSIShares();
}
```

---

### 2.7 Frontend — Configure iSCSI Modal

A large modal (`modal-lg`) with two sections:

**Section 1 — General Settings**

| Field | Control | Notes |
|-------|---------|-------|
| Base Name | `<input>` text | Default `iqn.2003-06.ca.chezmoi.zfsnas`; info tip: "Used as prefix for all target IQNs generated by ZFSNAS" |
| Listen Port | `<input>` number | Default `3260`; info tip: "ZFSNAS will configure iSCSI portals on all network interfaces of this server listening on this port." |

**Footer:** `Cancel` | `Save & Apply`

`submitISCSIConfig()` POSTs to `/api/iscsi/config` and on success calls `loadISCSIPage()`.

---

### 2.8 Frontend — New iSCSI Share Modal

A large modal (`modal-lg`) split into sections:

#### Section 1 — ZVol Selection

```html
<label>ZVol <span class="info-tip" title="The ZFS volume that backs this iSCSI target.">ℹ</span></label>
<select id="iscsi-zvol-select">
  <!-- Populated from GET /api/zvols -->
</select>
```

#### Section 2 — Allowed Hosts

```html
<div style="display:flex;align-items:center;margin-bottom:8px;">
  <span style="font-weight:600;font-size:14px;">Allowed Hosts</span>
  <button id="iscsi-hosts-edit-btn" onclick="toggleISCSIHostsEditMode()"
          style="margin-left:auto;font-size:12px;" class="btn-outline">+/−</button>
  <button id="iscsi-hosts-done-btn" onclick="toggleISCSIHostsEditMode()"
          style="margin-left:auto;font-size:12px;display:none;" class="btn-primary">Done</button>
</div>
<p style="font-size:12px;color:var(--text-3);margin-bottom:10px;">
  If no hosts are selected, any initiator can connect.
</p>
<table id="iscsi-hosts-table" style="width:100%;">
  <thead>
    <tr>
      <th style="width:32px;"><input type="checkbox" id="iscsi-host-check-all"></th>
      <th>Name</th>
      <th>IQN</th>
      <th>Comment</th>
      <th>In Use</th>
      <th id="iscsi-hosts-actions-col" style="display:none;"></th>
    </tr>
  </thead>
  <tbody id="iscsi-hosts-body">
    <!-- Rows populated from GET /api/iscsi/hosts -->
    <!-- New empty row added in edit mode -->
  </tbody>
</table>
```

Edit mode behaviour:
- Each existing row gains **Edit** (pencil inline) and **Delete** (🗑) buttons in the last column.
- A new empty row is appended at the bottom with `<input>` fields for Name, IQN, Comment; pressing Enter in the Comment field saves the row (calls `POST /api/iscsi/host`) and adds another empty row.
- The `+/−` button text changes to `Done`; pressing Done exits edit mode, hides the new-row inputs and action column, calls `loadISCSIHostsTable()`.

"In Use" column: `✓` if the host ID appears in any existing share's `host_ids`.

#### Section 3 — Comment

```html
<label>Comment (optional)</label>
<input id="iscsi-share-comment" type="text" placeholder="Optional description for this share" />
```

#### Footer

```html
<button onclick="closeModal()">Cancel</button>
<button class="btn-primary" onclick="submitNewISCSIShare()">Create iSCSI Share</button>
```

#### `submitNewISCSIShare()`

```js
async function submitNewISCSIShare() {
    const checkedHosts = Array.from(
        document.querySelectorAll('#iscsi-hosts-body input[type=checkbox]:checked')
    ).map(cb => cb.dataset.hostId);

    const body = {
        zvol:     document.getElementById('iscsi-zvol-select').value,
        host_ids: checkedHosts,
        comment:  document.getElementById('iscsi-share-comment').value.trim(),
    };
    // POST /api/iscsi/share/create
    // On success: close modal, reload iscsi page
}
```

---

## Feature 3: Prerequisites Page Update

### 3.1 iSCSI Package Entry

The Prerequisites page already lists packages like `samba`, `nfs-kernel-server`, etc. Add a new entry for the iSCSI feature:

```html
<tr id="prereq-iscsi-row">
  <td>
    <span style="color:var(--text-3);">targetcli-fb</span>
    <span class="info-tip" title="Required for iSCSI block storage sharing via ZVols.">ℹ</span>
  </td>
  <td>
    <span id="prereq-iscsi-status" style="color:var(--text-3);">Not installed</span>
  </td>
  <td>
    <button id="prereq-iscsi-btn" class="btn-outline"
            onclick="enableISCSIFeature()"
            style="font-size:12px;padding:4px 12px;">
      Enable iSCSI Feature
    </button>
  </td>
</tr>
```

**Styling distinction from other "not installed" packages:**
- Other packages use yellow/amber when missing (signalling something may be broken).
- iSCSI is an **optional feature** — it is not yellow; the package name and status text are grey (`var(--text-3)`).
- No yellow badge or warning icon.
- When installed: shows a green `Installed` badge and the button disappears (or changes to a disabled state).

### 3.2 `enableISCSIFeature()`

```js
async function enableISCSIFeature() {
    // POST /api/prerequisites/install with { package: "targetcli-fb" }
    // (reuse the existing package-install flow if one exists, otherwise use the OS Updates flow)
    // Show progress in activity bar
    // On success: reload prerequisites page + reload iSCSI status
}
```

If no generic "install package" endpoint exists yet, add:

#### `HandleInstallPackage` — `POST /api/prerequisites/install`

Body: `{ "package": "targetcli-fb" }`. Validates the package name against an allowlist. Runs `apt-get install -y <package>` with live output streamed or polled. Returns `{ "ok": true }` on completion.

Allowlist: `["targetcli-fb"]` for v6.1.0 (expandable in future versions).

---

## New Files Summary

| File | Purpose |
|------|---------|
| `system/iscsi.go` | ISCSIPrereqsInstalled, service control, targetcli Apply |
| `handlers/zvol.go` | List, create, edit, delete ZVol handlers |
| `handlers/iscsi.go` | All iSCSI API handlers |

## Modified Files Summary

| File | Change |
|------|--------|
| `system/zfs.go` | Add `ZVol` struct, `ListAllZVols`, `CreateZVol`, `EditZVol`, `DeleteZVol` |
| `internal/config/config.go` | Add `ISCSIHost`, `ISCSIShare`, `ISCSIConfig`, `AppConfig.ISCSI` field |
| `internal/audit/audit.go` | Add `ActionCreateZVol`, `ActionEditZVol`, `ActionDeleteZVol`, `ActionEditISCSIConfig`, `ActionCreateISCSIShare`, `ActionDeleteISCSIShare` |
| `handlers/router.go` | Register 4 ZVol + 10 iSCSI routes |
| `static/index.html` | ZVol button, modal, table rows; iSCSI sidebar nav, page, modals, all JS |
| `static/style.css` | Delete ZVol danger modal styles; iSCSI page/table styles |

## New API Routes

```
GET  /api/zvols                    → list all ZVols across pools
POST /api/zvol/create              → create ZVol (admin)
POST /api/zvol/edit                → edit ZVol properties (admin)
POST /api/zvol/delete              → destroy ZVol (admin)

GET  /api/iscsi/status             → prereqs installed + service status
POST /api/iscsi/service            → start/stop/restart service (admin)
GET  /api/iscsi/config             → get iSCSI settings
POST /api/iscsi/config             → save + apply iSCSI settings (admin)
GET  /api/iscsi/hosts              → list hosts
POST /api/iscsi/host               → upsert host (admin)
POST /api/iscsi/host/delete        → delete host (admin)
GET  /api/iscsi/shares             → list shares
POST /api/iscsi/share/create       → create share + apply targetcli (admin)
POST /api/iscsi/share/delete       → delete share + apply targetcli (admin)

POST /api/prerequisites/install    → install an optional package (admin, allowlisted)
```

---

## Implementation Order

1. `internal/config/config.go` — `ISCSIHost`, `ISCSIShare`, `ISCSIConfig`; add `ISCSI ISCSIConfig` to `AppConfig`
2. `internal/audit/audit.go` — new action constants
3. `system/zfs.go` — `ZVol` struct, `ListAllZVols`, `CreateZVol`, `EditZVol`, `DeleteZVol`
4. `system/iscsi.go` — `ISCSIPrereqsInstalled`, service control functions, `ApplyISCSIConfig`
5. `handlers/zvol.go` — four ZVol handlers
6. `handlers/iscsi.go` — ten iSCSI handlers + optional `HandleInstallPackage`
7. `handlers/router.go` — register all new routes
8. `static/style.css` — danger modal styles, iSCSI page styles, greyed-out prereq styles
9. `static/index.html` — iSCSI sidebar nav item
10. `static/index.html` — iSCSI page HTML (service bar, shares table, prereqs banner)
11. `static/index.html` — Configure iSCSI modal HTML
12. `static/index.html` — New iSCSI Share modal HTML (hosts table with edit mode)
13. `static/index.html` — Datasets page: "Create ZVol" button
14. `static/index.html` — Create ZVol modal HTML
15. `static/index.html` — Edit ZVol modal HTML
16. `static/index.html` — Delete ZVol danger confirmation modal HTML
17. `static/index.html` — Prerequisites page: iSCSI package row
18. `static/index.html` — All JS: `loadISCSIPage`, `iscsiServiceAction`, `openISCSIConfigModal`, `submitISCSIConfig`, `openNewISCSIShareModal`, `loadISCSIHostsTable`, `toggleISCSIHostsEditMode`, `submitNewISCSIShare`, `openCreateZVolModal`, `submitCreateZVol`, `openEditZVolModal`, `submitEditZVol`, `confirmDeleteZVol`, `executeDeleteZVol`, `loadZVols`, `enableISCSIFeature`, updates to `loadDatasets`/`renderDatasetTable` to merge ZVol rows
19. Build, test, deploy
