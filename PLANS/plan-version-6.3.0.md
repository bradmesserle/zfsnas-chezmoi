# Plan — Version 6.3.0: Multi-Target Notifications

## Goal

Restructure the Alerts settings into six independent notification targets:
**Email**, **ntfy**, **Gotify**, **Pushover**, **Syslog**, and **WebSocket**.

Each target has its own:
- "Enable Notifications for [Target]" toggle (when unchecked the config fields are hidden)
- Target-specific connection parameters
- Full independent set of event subscription checkboxes (same 9 events as current email)
- "Send Test" button

**Syslog** and **WebSocket** are special cases:
- Syslog sends RFC 3164 messages to a remote syslog server over UDP or TCP; no HTML — plain structured text.
- WebSocket is in-app only — no external server required; broadcasts a JSON alert payload to all browser sessions currently open in the portal, which renders a real-time dismissible toast notification. Config is just `Enabled` + event subscriptions.

All targets can be enabled simultaneously. When an alert fires, it dispatches to every enabled target.

---

## Config Restructure (`internal/alerts/alerts.go`)

### New types

```go
// EventConfig is unchanged — reused per target
type EventConfig struct { ... }  // same 9 events + 2 thresholds

type EmailTarget struct {
    Enabled bool       `json:"enabled"`
    SMTP    SMTPConfig `json:"smtp"`
    To      []string   `json:"to"`
    Events  EventConfig `json:"events"`
}

type NtfyTarget struct {
    Enabled  bool        `json:"enabled"`
    URL      string      `json:"url"`      // full topic URL, e.g. https://ntfy.sh/mytopic
    Token    string      `json:"token"`    // optional Bearer token
    Priority string      `json:"priority"` // default | low | high | urgent
    Events   EventConfig `json:"events"`
}

type GotifyTarget struct {
    Enabled  bool        `json:"enabled"`
    URL      string      `json:"url"`      // server root, e.g. https://gotify.example.com
    Token    string      `json:"token"`    // app token
    Priority int         `json:"priority"` // 1–10 (Gotify default 5)
    Events   EventConfig `json:"events"`
}

type PushoverTarget struct {
    Enabled  bool        `json:"enabled"`
    UserKey  string      `json:"user_key"`
    APIToken string      `json:"api_token"`
    Device   string      `json:"device"`   // optional, blank = all devices
    Priority int         `json:"priority"` // -2 to 2 (Pushover default 0)
    Events   EventConfig `json:"events"`
}

type SyslogTarget struct {
    Enabled  bool        `json:"enabled"`
    Host     string      `json:"host"`      // remote syslog host
    Port     int         `json:"port"`      // default 514
    Protocol string      `json:"protocol"`  // udp | tcp
    Facility string      `json:"facility"`  // user | daemon | local0 … local7
    Tag      string      `json:"tag"`       // app name in syslog header, default "zfsnas"
    Events   EventConfig `json:"events"`
}

// WebSocketTarget broadcasts alert payloads to all browser sessions connected
// to /ws/alerts. No external server — the portal itself is the hub.
type WebSocketTarget struct {
    Enabled bool        `json:"enabled"`
    Events  EventConfig `json:"events"`
}

type AlertConfig struct {
    Email     EmailTarget     `json:"email"`
    Ntfy      NtfyTarget      `json:"ntfy"`
    Gotify    GotifyTarget    `json:"gotify"`
    Pushover  PushoverTarget  `json:"pushover"`
    Syslog    SyslogTarget    `json:"syslog"`
    WebSocket WebSocketTarget `json:"websocket"`
}
```

### Backwards-compat migration in `Load()`

`alerts.json` currently stores `smtp`, `to`, and `events` at the root level.
On first load with the new code, detect these old keys and migrate them into
`email` (with `enabled: true` if `smtp.host` is non-empty), then save the new
format. After migration the old root keys are dropped.

Strategy: unmarshal into a `legacyAlertConfig` struct first; if `email` key is
absent but `smtp` key is present, populate `cfg.Email` from legacy fields.

### `Send()` dispatch

`Send(subject, event, details string)` iterates all enabled targets and fires
goroutines for each. Errors are logged per-target, never block other targets.

```go
func Send(key EventKey, subject, event, details string) error {
    cfg, _ := Load()
    hostname, _ := os.Hostname()
    if cfg.Email.Enabled     && matchesEvent(key, cfg.Email.Events)     { go sendEmail(cfg, subject, event, details, hostname) }
    if cfg.Ntfy.Enabled      && matchesEvent(key, cfg.Ntfy.Events)      { go sendNtfy(cfg.Ntfy, subject, event, details, hostname) }
    if cfg.Gotify.Enabled    && matchesEvent(key, cfg.Gotify.Events)    { go sendGotify(cfg.Gotify, subject, event, details, hostname) }
    if cfg.Pushover.Enabled  && matchesEvent(key, cfg.Pushover.Events)  { go sendPushover(cfg.Pushover, subject, event, details, hostname) }
    if cfg.Syslog.Enabled    && matchesEvent(key, cfg.Syslog.Events)    { go sendSyslog(cfg.Syslog, subject, event, details, hostname) }
    if cfg.WebSocket.Enabled && matchesEvent(key, cfg.WebSocket.Events) { broadcastWebSocket(subject, event, details, hostname) }
    return nil
}
```

Callers in `handlers/alerts.go` that check `cfg.SMTP.Host == ""` must be
updated to check `cfg.Email.Enabled` instead.

### Per-target senders

Email, ntfy, Gotify, Pushover use stdlib `net/http`.
Syslog uses stdlib `log/syslog`.
WebSocket uses the existing `gorilla/websocket` (already a dep).

**`sendNtfy()`**
```
POST <URL>
Headers:
  Authorization: Bearer <token>   (if token set)
  X-Title: [ZFS NAS] <subject>
  X-Priority: <priority>
  X-Tags: warning
Body: plain-text "Event: <event>\nDetails: <details>\nHost: <hostname>\nTime: <now>"
```

**`sendGotify()`**
```
POST <URL>/message?token=<token>
Content-Type: application/json
Body: {"title":"[ZFS NAS] <subject>","message":"<details>","priority":<N>}
```

**`sendPushover()`**
```
POST https://api.pushover.net/1/messages.json
Content-Type: application/x-www-form-urlencoded
Body: token=<api_token>&user=<user_key>&device=<device>&
      title=[ZFS NAS] <subject>&message=<details>&priority=<N>
```

**`sendSyslog()`**

Uses `log/syslog` from stdlib. Opens a new connection per send (stateless, safe for low-frequency alerts).

```go
func sendSyslog(cfg SyslogTarget, subject, event, details, hostname string) {
    network := cfg.Protocol               // "udp" or "tcp"
    addr    := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
    tag     := cfg.Tag; if tag == "" { tag = "zfsnas" }
    w, err  := syslog.Dial(network, addr, syslogFacility(cfg.Facility)|syslog.LOG_WARNING, tag)
    // ...
    msg := fmt.Sprintf("[ZFS NAS] %s | %s | %s | host=%s", subject, event, details, hostname)
    w.Warning(msg)
    w.Close()
}
```

Facility mapping: `user→LOG_USER`, `daemon→LOG_DAEMON`, `local0–local7→LOG_LOCAL0–LOG_LOCAL7`.
Default facility: `daemon`.

**`broadcastWebSocket()`**

The WebSocket target does not dial outward — it pushes to connected browsers.

Architecture:
- `internal/alerts/` exports a `SetWSHub(h WSHub)` function and a `WSHub` interface:
  ```go
  type WSHub interface { BroadcastJSON(v any) }
  ```
- `main.go` creates an `AlertsHub` (new, lightweight — just a broadcast hub) and calls `alerts.SetWSHub(hub)`.
- `broadcastWebSocket()` calls `hub.BroadcastJSON(payload)` where `payload` is:
  ```json
  { "subject": "...", "event": "...", "details": "...", "hostname": "...", "time": "..." }
  ```
- A new WebSocket endpoint `/ws/alerts` is registered in `router.go`.
  Handler: upgrade the connection, register the client with `AlertsHub`, relay broadcasts.
- Browser: on page load, open `/ws/alerts`. On message, render a dismissible toast.

`AlertsHub` reuses the same hub pattern as the binary-update WS hub (register/unregister/broadcast channels, single goroutine). It lives in a new file `internal/alerts/ws_hub.go`.

Toast UI: a small fixed overlay (bottom-right, dark card matching the portal theme). Each alert appends a card with title, details, and a close button. Auto-dismisses after 10 seconds.

---

## Event dispatch in health poller (`handlers/alerts.go`)

The poller currently checks `cfg.Events.PoolDegraded` directly. After the
refactor it must check each enabled target's event config:

The poller now calls `alerts.Send(key, subject, event, details)` directly.
`Send()` handles per-target event filtering internally via `matchesEvent`.
The `shouldSend` helper in `handlers/alerts.go` is no longer needed.

### EventKey constants
```go
const (
    EventPoolDegraded    EventKey = "pool_degraded"
    EventSmartError      EventKey = "smart_error"
    EventWearoutExceeded EventKey = "wearout_exceeded"
    EventFailedLogin     EventKey = "failed_login_alert"
    EventSecurityUpdates EventKey = "security_updates"
    EventScrubErrors     EventKey = "scrub_errors"
    EventSnapshotFailure EventKey = "snapshot_failure"
    EventUserCreated     EventKey = "user_created_deleted"
    EventShareCreated    EventKey = "share_created_deleted"
    EventTest            EventKey = "test"  // always passes filter
)
```

`matchesEvent(key EventKey, ev EventConfig) bool` maps key → field.

---

## HTTP API changes (`handlers/alerts.go`, `handlers/router.go`)

| Method | Path | Change |
|--------|------|--------|
| `GET`  | `/api/alerts` | Returns new `AlertConfig` shape; masks `email.smtp.password` |
| `PUT`  | `/api/alerts` | Accepts new shape; password-mask logic unchanged for email |
| `POST` | `/api/alerts/test` | Tests all enabled targets |
| `POST` | `/api/alerts/test/email` | Tests email only (ignores `enabled` flag) |
| `POST` | `/api/alerts/test/ntfy` | Tests ntfy only |
| `POST` | `/api/alerts/test/gotify` | Tests Gotify only |
| `POST` | `/api/alerts/test/pushover` | Tests Pushover only |
| `POST` | `/api/alerts/test/syslog` | Tests syslog only |
| `POST` | `/api/alerts/test/websocket` | Tests WebSocket broadcast only |
| `GET`  | `/ws/alerts` | WebSocket endpoint for in-browser real-time alerts |

Per-target test endpoints use the saved config for that target (reads from disk)
so the user can test without hitting "Apply" first — consistent with current
email test behaviour.

---

## UI changes (`static/index.html`)

### Settings card restructure

Rename card title from "Email Alerts" to **"Notifications"**.

Replace the single collapsible SMTP+events block with **six collapsible
target sections**, each following the same pattern:

```
[checkbox] Enable Email Notifications
  (when checked, expands:)
  ├── SMTP Configuration (sub-collapsible, same as today)
  ├── Recipients textarea
  ├── Event Subscriptions (sub-collapsible, same 9 checkboxes)
  └── [Send Test Email] button

[checkbox] Enable ntfy Notifications
  (when checked, expands:)
  ├── Topic URL  (input)
  ├── Token      (password input, optional)
  ├── Priority   (select: default / low / high / urgent)
  ├── Event Subscriptions (sub-collapsible)
  └── [Send Test ntfy] button

[checkbox] Enable Gotify Notifications
  (when checked, expands:)
  ├── Server URL  (input)
  ├── App Token   (password input)
  ├── Priority    (number 1–10)
  ├── Event Subscriptions (sub-collapsible)
  └── [Send Test Gotify] button

[checkbox] Enable Pushover Notifications
  (when checked, expands:)
  ├── User Key    (input)
  ├── API Token   (password input)
  ├── Device      (input, optional)
  ├── Priority    (select: lowest / low / normal / high / emergency)
  ├── Event Subscriptions (sub-collapsible)
  └── [Send Test Pushover] button

[checkbox] Enable Syslog Notifications
  (when checked, expands:)
  ├── Host        (input)
  ├── Port        (number, default 514)
  ├── Protocol    (select: UDP / TCP)
  ├── Facility    (select: user / daemon / local0 … local7)
  ├── Tag         (input, default "zfsnas")
  ├── Event Subscriptions (sub-collapsible)
  └── [Send Test Syslog] button

[checkbox] Enable In-App (WebSocket) Notifications
  (when checked, expands:)
  ├── (info text: "Sends real-time toast notifications to all open browser sessions")
  ├── Event Subscriptions (sub-collapsible)
  └── [Send Test Notification] button
```

Single **"Apply"** button at the bottom of the card saves all six targets
together (one `PUT /api/alerts`).

### Element ID scheme

Prefix per target: `al-em-*` (email), `al-nt-*` (ntfy), `al-gt-*` (gotify),
`al-po-*` (pushover), `al-sl-*` (syslog), `al-ws-*` (websocket).

Event checkboxes per target use the pattern `al-<prefix>-ev-pool`, etc.

### JS functions

- `loadAlerts()` — populates all six sections from API response
- `saveAlerts()` — builds full body from all six sections, PUT
- `toggleTarget(prefix)` — show/hide config block when enable checkbox toggled
- `testAlert(target)` — POSTs to `/api/alerts/test/<target>`; called by each "Send Test" button
- `toggleEventSubs(prefix)` — per-target collapsible toggle
- `toggleSMTPConfig()` — unchanged (email only)
- `initAlertsWebSocket()` — called on page load; opens `/ws/alerts`; on message renders a toast
- `showAlertToast(data)` — creates and appends a dismissible toast card to `#alert-toasts` overlay (bottom-right, auto-dismiss 10 s)

---

## Files modified

| File | Change |
|------|--------|
| `internal/alerts/alerts.go` | New config structs, migration, per-target senders, `EventKey`, `matchesEvent`, updated `Send()`, `SetWSHub()` |
| `internal/alerts/ws_hub.go` | New — `AlertsHub` (register/unregister/broadcast goroutine, implements `WSHub`) |
| `handlers/alerts.go` | Updated handlers, new per-target test routes, WS alerts handler, updated health poller event checks |
| `handlers/router.go` | Register 6 new test routes + `/ws/alerts` |
| `main.go` | Create `AlertsHub`, call `alerts.SetWSHub()` |
| `static/index.html` | Restructured Notifications card HTML + JS; toast overlay; `initAlertsWebSocket()` |

No new Go module dependencies (`log/syslog` is stdlib; gorilla/websocket already present).

---

## Migration / backwards compat

- Old `alerts.json` (root-level `smtp`/`to`/`events`) auto-migrated to `email` target on first load.
- `email.enabled` set to `true` if `smtp.host` was non-empty in old config.
- Migration is silent; new file written immediately after load.
- No breaking change for existing SMTP users.

---

## Testing checklist

- [ ] Old `alerts.json` migrates correctly; email still fires
- [ ] Each target fires independently when enabled
- [ ] Each "Send Test" button works for its own target
- [ ] Disabled target never fires even if events are checked
- [ ] `PUT /api/alerts` round-trips all four targets without clobbering
- [ ] SMTP password mask logic still works for email target
- [ ] Enabling zero targets is valid (all alerts silently no-op)
- [ ] Syslog UDP and TCP both deliver messages to a test syslog receiver
- [ ] WebSocket toast appears in all open browser tabs when an alert fires
- [ ] WebSocket auto-reconnects if the server restarts
- [ ] `POST /api/alerts/test/websocket` triggers a toast in the browser
