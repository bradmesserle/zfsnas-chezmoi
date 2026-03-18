package alerts

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/smtp"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"zfsnas/internal/secret"
)

// SMTPConfig holds SMTP connection parameters.
type SMTPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	From     string `json:"from"`
	AuthMode string `json:"auth_mode"` // none | plain | starttls | tls
	Username string `json:"username"`
	Password string `json:"password"`
}

// EventConfig holds per-event alert subscription flags and thresholds.
type EventConfig struct {
	PoolDegraded         bool `json:"pool_degraded"`
	SmartError           bool `json:"smart_error"`
	WearoutExceeded      bool `json:"wearout_exceeded"`
	WearoutThresholdPct  int  `json:"wearout_threshold_pct"`
	FailedLoginAlert     bool `json:"failed_login_alert"`
	FailedLoginThreshold int  `json:"failed_login_threshold"`
	SecurityUpdates      bool `json:"security_updates"`
	ScrubErrors          bool `json:"scrub_errors"`
	SnapshotFailure      bool `json:"snapshot_failure"`
	UserCreatedDeleted   bool `json:"user_created_deleted"`
	ShareCreatedDeleted  bool `json:"share_created_deleted"`
}

// AlertConfig is the root alert configuration persisted to alerts.json.
type AlertConfig struct {
	SMTP   SMTPConfig  `json:"smtp"`
	To     []string    `json:"to"`
	Events EventConfig `json:"events"`
}

var (
	configDir    string
	mu           sync.RWMutex
	failedLogins int64
	smtpKey      []byte // AES-256 key for SMTP password encryption
)

// Init sets the config directory and loads (or creates) the SMTP encryption key.
func Init(dir string) {
	configDir = dir
	keyPath := filepath.Join(dir, "smtp.key")
	key, err := secret.LoadOrCreateKey(keyPath)
	if err != nil {
		log.Printf("[alerts] warning: could not load/create SMTP key: %v — password will be stored unencrypted", err)
		return
	}
	smtpKey = key
}

func defaultConfig() *AlertConfig {
	return &AlertConfig{
		SMTP: SMTPConfig{Port: 587, AuthMode: "starttls"},
		Events: EventConfig{
			PoolDegraded:         true,
			SmartError:           true,
			WearoutExceeded:      true,
			WearoutThresholdPct:  80,
			FailedLoginAlert:     true,
			FailedLoginThreshold: 5,
			ScrubErrors:          true,
			SnapshotFailure:      true,
		},
	}
}

// Load reads alert config from disk, returning defaults if the file does not exist.
// If the SMTP password is encrypted, it is decrypted in the returned struct so
// callers (e.g. the SMTP sender) always receive the plaintext value.
func Load() (*AlertConfig, error) {
	mu.RLock()
	defer mu.RUnlock()
	cfg := defaultConfig()
	path := filepath.Join(configDir, "alerts.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	// Decrypt SMTP password if encrypted, silently keep plaintext for legacy configs.
	if smtpKey != nil && secret.IsEncrypted(cfg.SMTP.Password) {
		if plain, err := secret.Decrypt(smtpKey, cfg.SMTP.Password); err == nil {
			cfg.SMTP.Password = plain
		}
	}
	return cfg, nil
}

// Save persists alert config to disk, encrypting the SMTP password if a key is available.
// If the password is already an encrypted blob (e.g. copied from an existing config),
// it is written as-is without double-encrypting.
func Save(cfg *AlertConfig) error {
	mu.Lock()
	defer mu.Unlock()

	// Work on a shallow copy so we don't modify the caller's struct.
	toWrite := *cfg
	if smtpKey != nil && toWrite.SMTP.Password != "" && !secret.IsEncrypted(toWrite.SMTP.Password) {
		enc, err := secret.Encrypt(smtpKey, toWrite.SMTP.Password)
		if err != nil {
			return fmt.Errorf("encrypt SMTP password: %w", err)
		}
		toWrite.SMTP.Password = enc
	}

	path := filepath.Join(configDir, "alerts.json")
	data, err := json.MarshalIndent(toWrite, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0640)
}

// RecordFailedLogin increments the failed-login counter.
func RecordFailedLogin() {
	atomic.AddInt64(&failedLogins, 1)
}

// ResetFailedLogins resets the counter to zero.
func ResetFailedLogins() {
	atomic.StoreInt64(&failedLogins, 0)
}

// FailedLoginCount returns the current failed-login count.
func FailedLoginCount() int64 {
	return atomic.LoadInt64(&failedLogins)
}

// Send sends an HTML alert email with the given event name and detail text.
// It is a no-op when SMTP is not configured or no recipients are set.
func Send(subject, event, details string) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	if cfg.SMTP.Host == "" || len(cfg.To) == 0 {
		return nil
	}
	hostname, _ := os.Hostname()
	body, err := renderEmail(emailData{
		Event:    event,
		Details:  details,
		Hostname: hostname,
		Time:     time.Now().Format("2006-01-02 15:04:05 MST"),
	})
	if err != nil {
		return err
	}
	return sendSMTP(cfg, subject, body)
}

func sendSMTP(cfg *AlertConfig, subject, htmlBody string) error {
	addr := fmt.Sprintf("%s:%d", cfg.SMTP.Host, cfg.SMTP.Port)
	msg := buildMIME(cfg.SMTP.From, cfg.To, subject, htmlBody)

	switch cfg.SMTP.AuthMode {
	case "none":
		return smtp.SendMail(addr, nil, cfg.SMTP.From, cfg.To, msg)
	case "plain", "starttls":
		auth := smtp.PlainAuth("", cfg.SMTP.Username, cfg.SMTP.Password, cfg.SMTP.Host)
		return smtp.SendMail(addr, auth, cfg.SMTP.From, cfg.To, msg)
	case "tls":
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: cfg.SMTP.Host})
		if err != nil {
			return err
		}
		c, err := smtp.NewClient(conn, cfg.SMTP.Host)
		if err != nil {
			return err
		}
		defer c.Quit() //nolint
		if cfg.SMTP.Username != "" {
			auth := smtp.PlainAuth("", cfg.SMTP.Username, cfg.SMTP.Password, cfg.SMTP.Host)
			if err := c.Auth(auth); err != nil {
				return err
			}
		}
		if err := c.Mail(cfg.SMTP.From); err != nil {
			return err
		}
		for _, r := range cfg.To {
			if err := c.Rcpt(r); err != nil {
				return err
			}
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		return w.Close()
	}
	return fmt.Errorf("unknown auth_mode: %s", cfg.SMTP.AuthMode)
}

func buildMIME(from string, to []string, subject, htmlBody string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", from)
	for _, t := range to {
		fmt.Fprintf(&buf, "To: %s\r\n", t)
	}
	fmt.Fprintf(&buf, "Subject: [ZFS NAS] %s\r\n", subject)
	fmt.Fprintf(&buf, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&buf, "Content-Type: text/html; charset=UTF-8\r\n")
	fmt.Fprintf(&buf, "\r\n")
	fmt.Fprintf(&buf, "%s", htmlBody)
	return buf.Bytes()
}

type emailData struct {
	Event    string
	Details  string
	Hostname string
	Time     string
}

var emailTmpl = template.Must(template.New("email").Parse(`<!DOCTYPE html>
<html><head><meta charset="UTF-8"></head>
<body style="margin:0;padding:0;background:#0d0d0f;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;">
  <div style="max-width:560px;margin:32px auto;border-radius:12px;overflow:hidden;border:1px solid #2a2a35;">
    <div style="background:linear-gradient(135deg,#bf5af2,#6e40c9);padding:20px 28px;">
      <div style="color:#fff;font-size:20px;font-weight:700;">ZFS NAS Alert</div>
      <div style="color:rgba(255,255,255,.75);font-size:13px;margin-top:4px;">{{.Event}}</div>
    </div>
    <div style="background:#161619;padding:28px;">
      <table style="width:100%;border-collapse:collapse;font-size:14px;color:#e5e5ea;">
        <tr>
          <td style="padding:10px 0;border-bottom:1px solid #2a2a35;color:#8e8e93;width:38%;">Event</td>
          <td style="padding:10px 0;border-bottom:1px solid #2a2a35;font-weight:600;">{{.Event}}</td>
        </tr>
        <tr>
          <td style="padding:10px 0;border-bottom:1px solid #2a2a35;color:#8e8e93;">Details</td>
          <td style="padding:10px 0;border-bottom:1px solid #2a2a35;">{{.Details}}</td>
        </tr>
        <tr>
          <td style="padding:10px 0;border-bottom:1px solid #2a2a35;color:#8e8e93;">Host</td>
          <td style="padding:10px 0;border-bottom:1px solid #2a2a35;font-family:monospace;">{{.Hostname}}</td>
        </tr>
        <tr>
          <td style="padding:10px 0;color:#8e8e93;">Time</td>
          <td style="padding:10px 0;font-family:monospace;">{{.Time}}</td>
        </tr>
      </table>
    </div>
    <div style="background:#0d0d0f;padding:14px 28px;font-size:11px;color:#48484a;border-top:1px solid #2a2a35;">
      Sent by ZFS NAS Management Portal &middot; {{.Hostname}}
    </div>
  </div>
</body></html>`))

func renderEmail(d emailData) (string, error) {
	var buf bytes.Buffer
	if err := emailTmpl.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}
