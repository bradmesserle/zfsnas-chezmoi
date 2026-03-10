# ZFS NAS Management Portal — Project Plan

## Tech Stack

| Layer | Choice | Reason |
|---|---|---|
| Language | Go 1.22+ | Single binary, fast, great stdlib |
| HTTP Router | `net/http` + `gorilla/mux` | Lightweight routing |
| Auth | Server-side sessions (cookie, in-memory map) | Real logout, admin session kill |
| Config/Storage | JSON files + append-only audit.log | No DB needed |
| Frontend | Embedded HTML/CSS/JS via `embed` (Alpine.js) | Single binary, reactive UI, no build step |
| Terminal | `xterm.js` + WebSocket + `pty` | Live terminal in browser |
| TLS | Auto-generated self-signed cert | HTTPS from day one |

---

## Directory Structure

```
zfsnas/
├── main.go
├── go.mod
├── go.sum
├── PLAN.md
├── config/                        (created at runtime, gitignored)
│   ├── config.json
│   ├── users.json
│   ├── shares.json
│   ├── snapshots.json
│   ├── alerts.json
│   ├── audit.log
│   └── certs/
│       ├── server.crt
│       └── server.key
├── handlers/
│   ├── auth.go
│   ├── disks.go
│   ├── pools.go
│   ├── datasets.go
│   ├── snapshots.go
│   ├── shares.go
│   ├── users.go
│   ├── terminal.go
│   ├── updates.go
│   ├── alerts.go
│   ├── audit.go
│   ├── prereqs.go
│   └── settings.go
├── system/
│   ├── zfs.go                     (zpool/zfs command wrappers)
│   ├── disks.go                   (lsblk, smartctl, nvme-cli)
│   ├── samba.go                   (smb.conf management, smbpasswd)
│   └── apt.go                     (apt-get wrappers)
├── config/pkg/                    (Go package for config types & loader)
│   ├── config.go
│   ├── users.go
│   ├── shares.go
│   └── audit.go
├── static/
│   ├── index.html
│   ├── app.js
│   ├── style.css
│   └── xterm/                     (xterm.js + addon files)
└── certs/                         (empty, populated at runtime)
```

---

## Config Folder

Location: `<app_binary_dir>/config/`
Works whether the app lives in `/home/user/` or `/opt/zfsnas/`.

```
config/
├── config.json      # port, first-run flag, SMART last-refresh timestamp
├── users.json       # all users: admin, read-only, smb-only
├── shares.json      # SMB share definitions
├── snapshots.json   # snapshot labels/metadata
├── alerts.json      # SMTP config + event subscriptions
├── audit.log        # append-only, one JSON line per entry
└── certs/
    ├── server.crt
    └── server.key
```

---

## Roles

| Role | Portal Login | Create/Edit | Terminal | SMB Share Picker |
|---|---|---|---|---|
| `admin` | Yes | Yes | Yes | Yes |
| `read-only` | Yes | No | No | No |
| `smb-only` | No | No | No | Yes (assignable only) |

---

## First-Run Sequence

1. Binary starts → read `config/users.json`
2. **Prerequisites check** → detect missing packages → prompt "Install now?" → stream `apt-get install` output in browser via WebSocket
3. **Systemd service** → prompt "Install as systemd service so this starts on reboot?" → if yes, write unit file pointing to current binary path + current user → `systemctl enable + start`
4. **ZFS pool detection** → scan existing pools → if found, offer "Import detected pool: `tank`?" → if none, user proceeds to create one
5. **First admin account** → if no users exist → `/setup` page (unauthenticated)
6. Login → full portal

---

## Port & TLS

- Default: `8443`, configurable in Settings UI (must be > 1024)
- Self-signed cert auto-generated on first start into `config/certs/`
- HTTPS only

---

## Sessions

- Server-side session map (in-memory, keyed by secure random token)
- Token sent as `HttpOnly` + `Secure` cookie
- Logout invalidates token immediately
- Admin can list and kill active sessions in User Management

---

## ZFS — Pool

**Create options:**
- Layout: Stripe | RAIDZ1 | RAIDZ2
- Block size (ashift): 9 (512b) | 12 (4K — recommended) | 13 (8K — NVMe) with tooltip
- Compression: off | lz4 (recommended) | zstd

**Detect existing:**
- "Scan for existing pools" button → `sudo zpool import` dry-run → shows found pools → admin clicks Import

---

## ZFS — Datasets (nested)

- Full nested support: `pool/data`, `pool/data/media`, `pool/data/media/movies`
- Create dataset under pool root or under any existing dataset
- Options: quota size, quota type (quota vs refquota with tooltip), compression on/off

---

## Pool Capacity Bar Chart

- Fixed at top of every page
- Full bar width = 100% of pool raw size
- Each dataset = colored segment, nested datasets shown as indented sub-segments inside parent
- Apple AI gradient palette per dataset (purple → blue → green → teal, cycling)
- Hover: tooltip with used / quota / compression ratio
- Click: slides open detail panel showing:
  - Used / Available / Quota / Refquota
  - Compression ratio (`compressratio`)
  - Record size, compression algorithm
  - Users with SMB access + their permission level

---

## ZFS — Snapshots

- Per-dataset "Snapshot" button → user provides a label → `sudo zfs snapshot pool/dataset@label-timestamp`
- **Tree visualization**: dataset hierarchy on left, each node expands to show snapshots chronologically with size delta
- Actions per snapshot: Restore (rollback), Delete, Clone to new dataset
- Columns: name, creation date, referenced size, used size

---

## SMB Shares

- User provides a **share name** (required, alphanumeric + dash/underscore)
- Toggle enable/disable (writes/removes share block in `smb.conf`, reloads without restart via `smbcontrol`)
- Per-user assignment: pick from admin + smb-only users (not read-only portal users)
- Each assigned user: **Read-Write** or **Read-Only**
- Default if no users selected: guest ok / everyone
- Global `smb.conf` enforces: `max smbd processes = 100`

---

## Disk Listing

- Source: `lsblk -J -o NAME,SIZE,ROTA,VENDOR,MODEL,TRAN,TYPE` filtered to disks not mounted to system paths (/, /boot, swap)
- Per disk: Vendor, Model, Size, Type (HDD/SSD/NVMe)
- SSD wearout: `sudo smartctl -j -a /dev/sdX` → `Wear_Leveling_Count` or `Percent_Lifetime_Remaining`
- NVMe wearout: `sudo nvme smart-log /dev/nvmeXn1` → `percentage_used`
- SMART data cached in `config/` with **daily refresh** (background goroutine, refreshes on start if last refresh > 24h)
- Wearout UI: color-coded progress bar (green → yellow → red)

---

## Email Alerts

**SMTP Settings (admin, in Settings page):**
- Host, Port, From address
- Auth mode: none | plain (user+pass) | STARTTLS | TLS
- "Send test email" button

**Subscribable event types:**
- Disk wearout exceeds threshold (configurable %, default 80%)
- SMART error detected
- Pool health degraded (DEGRADED/FAULTED)
- Share enabled / disabled
- User created / deleted
- Failed login attempts (configurable threshold)
- Ubuntu security updates available
- Snapshot created / deleted / restored

**Email format:**
- HTML, dark header with app logo + gradient accent bar
- Subject: `[ZFS NAS] <Event Type> — <short description>`
- Body: compact 2-column table, fits a single iPhone mail screen
- Footer: timestamp + hostname

---

## Audit Log

- File: `config/audit.log` — append-only, one JSON line per entry
- Fields: `timestamp`, `user`, `role`, `action`, `target`, `result`, `details`
- UI: "Activity Log" page + live sidebar widget
  - Filterable by user, action type, date range
  - Active/in-progress jobs (apt install, snapshot restore) shown live with spinner
  - Completed actions: green checkmark or red X

---

## Ubuntu Updates

- `sudo apt-get update` + `apt list --upgradable` → display list
- "Apply Updates" streams `sudo apt-get upgrade -y` via WebSocket
- Each apply run logged in audit log

---

## Web Terminal

- **Admin-only**
- `xterm.js` frontend + WebSocket backend
- Backend: `os/exec` spawning `bash` attached to a `pty` (no shell injection — args always separate)
- Kill button in terminal header
- Admin can list open terminal sessions and kill them

---

## Systemd Unit (auto-generated)

```ini
[Unit]
Description=ZFS NAS Management Portal
After=network.target

[Service]
Type=simple
User=<current_user>
WorkingDirectory=<app_dir>
ExecStart=<app_dir>/zfsnas
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

Written to `/etc/systemd/system/zfsnas.service`.

---

## Prerequisites (checked on every start)

| Package | Purpose |
|---|---|
| `zfsutils-linux` | zpool / zfs commands |
| `samba` | SMB sharing |
| `smartmontools` | smartctl for SSD wearout |
| `nvme-cli` | NVMe wearout |
| `util-linux` | lsblk |

---

## API Routes

```
Auth & Setup
  POST   /api/auth/login
  POST   /api/auth/logout
  GET    /api/auth/sessions           admin: list active sessions
  DELETE /api/auth/sessions/:id       admin: kill session
  POST   /setup                       unauthenticated, first-run only

Prerequisites & Service
  GET    /api/prereqs
  POST   /api/prereqs/install         (WS stream)
  POST   /api/prereqs/install-service

Disks
  GET    /api/disks                   returns cached SMART data
  POST   /api/disks/refresh           force SMART refresh

Pool
  GET    /api/pool
  POST   /api/pool                    create
  GET    /api/pool/detect             scan importable pools
  POST   /api/pool/import

Datasets
  GET    /api/datasets
  POST   /api/datasets
  PUT    /api/datasets/*path
  DELETE /api/datasets/*path

Snapshots
  GET    /api/snapshots
  POST   /api/snapshots               create with label
  POST   /api/snapshots/:id/restore
  POST   /api/snapshots/:id/clone
  DELETE /api/snapshots/:id

Shares
  GET    /api/shares
  POST   /api/shares
  PUT    /api/shares/:name
  DELETE /api/shares/:name
  POST   /api/shares/:name/enable
  POST   /api/shares/:name/disable

Users
  GET    /api/users
  POST   /api/users
  PUT    /api/users/:id
  DELETE /api/users/:id

Updates
  GET    /api/updates
  POST   /api/updates/apply           (WS stream)

Settings
  GET    /api/settings
  PUT    /api/settings
  POST   /api/settings/smtp-test

Alerts
  GET    /api/alerts
  PUT    /api/alerts

Audit
  GET    /api/audit

WebSockets
  WS     /ws/terminal
  WS     /ws/prereqs-install
  WS     /ws/updates
```

---

## UI Design — Dark Theme, Apple AI Palette

| Token | Value |
|---|---|
| Background | `#0d0d0f` |
| Surface (cards/panels) | `#1c1c1e` |
| Accent gradient | `#bf5af2` → `#0a84ff` → `#32d74b` |
| Text primary | `#f5f5f7` |
| Text secondary | `#8e8e93` |
| Danger | `#ff453a` |
| Warning | `#ffd60a` |
| Font | `system-ui`, SF Pro fallback |

- Rounded corners, subtle backdrop blur on modals
- Logo: top-left, gradient text "ZFS NAS" with subtle glow
- Alpine.js for reactivity (no build step, embedded)

---

## Implementation Phases

| Phase | Scope |
|---|---|
| 1 | Go module init, TLS, server-side sessions, middleware, first-run `/setup` |
| 2 | Prereqs check/install (WS), systemd install prompt, audit log foundation |
| 3 | Disk listing + SMART wearout caching, daily refresh goroutine |
| 4 | Pool detect/import/create, Dataset CRUD (nested), Snapshot tree |
| 5 | SMB share management, per-user rw/ro, smb.conf generation |
| 6 | Pool capacity bar chart (nested segments), full dark-theme SPA |
| 7 | User management (all 3 roles), active sessions UI, audit log UI |
| 8 | Web terminal (xterm.js + pty + WS) |
| 9 | Ubuntu updates page (WS streaming output) |
| 10 | Email alerts (SMTP config, event subscriptions, HTML templates) |
