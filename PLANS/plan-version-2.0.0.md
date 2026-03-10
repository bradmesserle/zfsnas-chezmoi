# ZFS NAS Management Portal — Version 2.0.0 Plan

Version 2.0.0 builds on the solid 1.0.0 foundation by adding proactive alerting,
NFS share support, automated snapshot scheduling, ZFS scrub management, and a live
system resource dashboard. No breaking changes to the existing API or data formats.

---

## What Shipped in 1.0.0

- HTTPS server with server-side sessions, roles (admin / read-only / smb-only)
- First-run setup wizard, prerequisites installer, systemd service installer
- ZFS pool create / import / detect / expand / destroy
- ZFS datasets (full nested hierarchy), snapshots (create / restore / clone / delete)
- Physical disk listing with SMART wearout (ATA + NVMe), ZFS-assigned vs available split
- SMB shares (create / edit / delete), per-user Samba password provisioning
- Pool capacity liquid-cylinder bar chart (top of every page)
- Ubuntu OS update check + streaming apply
- Web terminal (xterm.js + PTY + WebSocket, admin-only, bottom drawer)
- Audit log (activity sidebar + full log page)
- Settings: port, storage unit (GB / GiB)
- zpool status auto-refresh panel on the Pool page

---

## Version 2.0.0 Feature Set

---

### Phase 1 — Email Alerts

**Goal:** proactive notification when something goes wrong or needs attention.

**SMTP configuration (Settings page — new "Alerts" tab):**
- Host, port, from address
- Auth mode: none | PLAIN (user + pass) | STARTTLS | TLS
- "Send test email" button that sends a real message and reports success/failure

**Subscribable event types (per-event toggle switches):**
| Event | Default |
|---|---|
| Pool health degraded (DEGRADED / FAULTED) | On |
| SMART error detected on any disk | On |
| Disk wearout exceeds threshold (configurable %, default 80%) | On |
| Failed login attempts exceed threshold (configurable count) | On |
| Ubuntu security updates available | Off |
| Scrub completed with errors | On |
| Snapshot auto-policy failure | On |
| User created / deleted | Off |
| Share created / deleted | Off |

**Email format:**
- HTML, dark header matching portal theme, gradient accent bar
- Subject: `[ZFS NAS] <Event> — <short description>`
- Body: compact 2-column table with relevant details
- Footer: timestamp + hostname + link to portal

**Backend:**
- `internal/alerts/` package: SMTP send, config persistence (`config/alerts.json`)
- Background health-check goroutine (every 5 min): polls pool health, SMART wearout
- Failed login counter tracked in session store, fires alert on threshold breach
- `handlers/alerts.go`: GET + PUT `/api/alerts`, POST `/api/alerts/test`

---

### Phase 2 — NFS Shares

**Goal:** Linux/macOS file sharing alongside existing SMB.

**UI — new "NFS Shares" nav item (under Sharing, below SMB Shares):**
- Status banner: NFS installed / not installed (link to Prerequisites)
- Share table: Path | Clients | Options | Actions (Edit / Delete)
- Create / Edit modal:
  - Path: ZFS dataset dropdown (same as SMB)
  - Client CIDR (e.g. `10.0.0.0/8`, `*` for all)
  - Options: `ro` / `rw`, `sync` / `async`, `no_subtree_check`, `no_root_squash`
  - Comment / label

**Backend (`system/nfs.go`):**
- Manage `/etc/exports` with begin/end markers (same pattern as smb.conf)
- `ExportFS(shares)` — writes managed block, runs `sudo exportfs -ra`
- `NFSStatus()` — `systemctl is-active nfs-server`
- `IsNFSInstalled()` — checks for `exportfs` binary
- `handlers/nfs.go`: standard CRUD handlers + status endpoint
- Add `nfs-kernel-server` to prerequisites package list

---

### Phase 3 — Scheduled Snapshots

**Goal:** automated, policy-driven snapshot creation and pruning.

**UI — new "Schedules" sub-section under Snapshots page:**
- Policy table per dataset: Frequency | Retention | Label | Last Run | Next Run | Status
- Create policy modal:
  - Target dataset (dropdown)
  - Frequency: Hourly | Daily | Weekly | Monthly (with time-of-day picker for daily+)
  - Retention count (e.g. keep last 24 hourly, 7 daily, 4 weekly)
  - Label prefix (e.g. `auto`)
- Manual "Run Now" button per policy
- Policy stored in `config/snapshot-schedules.json`

**Backend:**
- `internal/scheduler/` package: cron-like ticker goroutine, loads policies on start
- On trigger: calls `system.CreateSnapshot()`, then prunes oldest snapshots beyond retention
- Logs each run to audit log
- `handlers/schedules.go`: CRUD for `/api/snapshot-schedules`

---

### Phase 4 — ZFS Scrub Management

**Goal:** trigger, monitor, and schedule scrubs from the portal.

**UI — new "Scrub" card on the ZFS Pool page (below zpool status):**
- Current scrub status: idle / in progress (% complete, estimated time remaining) / finished (errors found or clean)
- "Start Scrub" button (admin only) — disabled if scrub already running
- "Stop Scrub" button — appears only while running
- Last scrub result: date, duration, errors found
- Auto-refresh every 10 s while scrub is in progress (reuses existing pool-status interval)
- Optional: scheduled scrub (weekly on Sunday at 02:00 — toggle in scrub card)

**Backend:**
- `system.StartScrub(poolName)` → `sudo zpool scrub <pool>`
- `system.StopScrub(poolName)` → `sudo zpool scrub -s <pool>`
- `system.ScrubStatus(poolName)` → parses `zpool status` for `scan:` line → returns struct with state, progress %, errors, start time, duration
- `handlers/scrub.go`: POST `/api/pool/scrub/start`, POST `/api/pool/scrub/stop`, GET `/api/pool/scrub/status`

---

### Phase 5 — System Resource Dashboard

**Goal:** at-a-glance live view of server health (CPU, RAM, network, disk I/O).

**UI — new "Dashboard" nav item (top of sidebar, first item):**
- 4 stat cards: CPU usage %, RAM used/total, RX/TX network (Mbps), Disk I/O (MB/s)
- CPU chart: 60-second rolling sparkline (updates every 2 s)
- RAM chart: used vs available donut
- Network chart: per-interface RX + TX sparklines
- Disk I/O chart: per-pool read + write sparklines (from `zpool iostat`)
- All charts drawn with a lightweight canvas-based library (no heavy dependencies — use Chart.js 4 from CDN, same pattern as xterm.js)

**Backend:**
- `system/sysinfo.go`:
  - `GetCPUPercent()` — reads `/proc/stat`, computes delta between two samples
  - `GetMemInfo()` — reads `/proc/meminfo`
  - `GetNetStats()` — reads `/proc/net/dev`
  - `GetZpoolIOStat(poolName)` — `sudo zpool iostat -Hp <pool> 1 2` (two samples, take second)
- `GET /api/sysinfo` — returns all four metrics in one JSON response
- Frontend polls every 2 s; stores 60-point ring buffer per metric for sparklines

---

## API Additions (v2.0.0)

```
Alerts
  GET    /api/alerts                  get SMTP config + event subscriptions
  PUT    /api/alerts                  update config + subscriptions
  POST   /api/alerts/test             send test email

NFS
  GET    /api/nfs/status
  GET    /api/nfs/shares
  POST   /api/nfs/shares
  PUT    /api/nfs/shares/{id}
  DELETE /api/nfs/shares/{id}

Snapshot Schedules
  GET    /api/snapshot-schedules
  POST   /api/snapshot-schedules
  PUT    /api/snapshot-schedules/{id}
  DELETE /api/snapshot-schedules/{id}
  POST   /api/snapshot-schedules/{id}/run-now

Scrub
  GET    /api/pool/scrub/status
  POST   /api/pool/scrub/start
  POST   /api/pool/scrub/stop

System Info
  GET    /api/sysinfo
```

---

## New Dependencies

| Package | Purpose |
|---|---|
| None (Go stdlib only) | CPU/RAM/network stats from `/proc` |
| Chart.js 4 (CDN) | Sparkline + donut charts on dashboard |

No new Go modules required — all system stats come from `/proc` and existing CLI tools.

---

## Implementation Order

| Phase | Scope | Effort |
|---|---|---|
| 1 | Email alerts (SMTP config, health poller, HTML templates) | Medium |
| 2 | NFS shares (`/etc/exports` management, UI) | Small |
| 3 | Scheduled snapshots (scheduler goroutine, policy CRUD, pruning) | Medium |
| 4 | ZFS scrub management (trigger, monitor, schedule) | Small |
| 5 | System resource dashboard (sysinfo API, sparkline charts) | Medium |
