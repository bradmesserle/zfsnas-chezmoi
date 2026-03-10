package system

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	smbConfPath    = "/etc/samba/smb.conf"
	smbBeginMarker = "# ===== ZFS NAS MANAGED SHARES BEGIN ====="
	smbEndMarker   = "# ===== ZFS NAS MANAGED SHARES END ====="
)

// SMBShare represents a Samba file share.
type SMBShare struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Comment    string   `json:"comment"`
	Browseable bool     `json:"browseable"`
	ReadOnly   bool     `json:"read_only"`
	ValidUsers []string `json:"valid_users"`
	GuestOK    bool     `json:"guest_ok"`
}

func smbSharesPath(configDir string) string {
	return filepath.Join(configDir, "shares.json")
}

// ListSMBShares returns all configured SMB shares from the JSON store.
func ListSMBShares(configDir string) ([]SMBShare, error) {
	data, err := os.ReadFile(smbSharesPath(configDir))
	if os.IsNotExist(err) {
		return []SMBShare{}, nil
	}
	if err != nil {
		return nil, err
	}
	var shares []SMBShare
	if err := json.Unmarshal(data, &shares); err != nil {
		return nil, err
	}
	if shares == nil {
		return []SMBShare{}, nil
	}
	return shares, nil
}

// SaveSMBShares persists shares to JSON and applies them to smb.conf.
func SaveSMBShares(configDir string, shares []SMBShare) error {
	data, err := json.MarshalIndent(shares, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(smbSharesPath(configDir), data, 0640); err != nil {
		return err
	}
	return applySMBConf(shares)
}

// applySMBConf writes the managed section into /etc/samba/smb.conf.
func applySMBConf(shares []SMBShare) error {
	// Build the managed block.
	var sb strings.Builder
	sb.WriteString(smbBeginMarker + "\n")
	for _, s := range shares {
		sb.WriteString(fmt.Sprintf("\n[%s]\n", s.Name))
		if s.Comment != "" {
			sb.WriteString("   comment = " + s.Comment + "\n")
		}
		sb.WriteString("   path = " + s.Path + "\n")
		sb.WriteString("   browseable = " + boolSMB(s.Browseable) + "\n")
		sb.WriteString("   read only = " + boolSMB(s.ReadOnly) + "\n")
		sb.WriteString("   guest ok = " + boolSMB(s.GuestOK) + "\n")
		if len(s.ValidUsers) > 0 {
			sb.WriteString("   valid users = " + strings.Join(s.ValidUsers, ", ") + "\n")
		}
		sb.WriteString("   create mask = 0664\n")
		sb.WriteString("   directory mask = 0775\n")
		sb.WriteString("   force group = sambashare\n")
	}
	sb.WriteString("\n" + smbEndMarker + "\n")
	managed := sb.String()

	// Read existing smb.conf (readable without sudo on most systems).
	existing, err := os.ReadFile(smbConfPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read smb.conf: %w", err)
	}
	conf := string(existing)

	// Replace or append the managed section.
	begin := strings.Index(conf, smbBeginMarker)
	end := strings.Index(conf, smbEndMarker)
	var newConf string
	if begin >= 0 && end > begin {
		newConf = conf[:begin] + managed + conf[end+len(smbEndMarker):]
		// Trim any double newlines left by removal.
		newConf = strings.ReplaceAll(newConf, "\n\n\n", "\n\n")
	} else {
		newConf = strings.TrimRight(conf, "\n") + "\n\n" + managed
	}

	return writeFileSudo(smbConfPath, newConf)
}

// ReloadSamba reloads the Samba configuration without dropping connections.
func ReloadSamba() error {
	out, err := exec.Command("sudo", "systemctl", "reload", "smbd").CombinedOutput()
	if err != nil {
		// Fall back to restart if reload fails (smbd not running yet).
		out2, err2 := exec.Command("sudo", "systemctl", "restart", "smbd").CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("%s / %s", strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	return nil
}

// IsSambaInstalled checks if the smbd binary is available.
func IsSambaInstalled() bool {
	_, err := exec.LookPath("smbd")
	return err == nil
}

// SambaStatus returns "active", "inactive", or "not-installed".
func SambaStatus() string {
	if !IsSambaInstalled() {
		return "not-installed"
	}
	out, err := exec.Command("systemctl", "is-active", "smbd").Output()
	if err != nil {
		return "inactive"
	}
	return strings.TrimSpace(string(out))
}

// ControlSamba runs systemctl start/stop/restart on smbd (and nmbd if present).
func ControlSamba(action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("invalid action: %s", action)
	}
	out, err := exec.Command("sudo", "systemctl", action, "smbd").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s smbd: %s", action, strings.TrimSpace(string(out)))
	}
	// nmbd (NetBIOS name service) is optional; ignore errors.
	_ = exec.Command("sudo", "systemctl", action, "nmbd").Run()
	return nil
}

// EnsureSambaUser creates a Linux system account (if absent) and sets its
// Samba password, making the user ready for SMB authentication.
func EnsureSambaUser(username, password string) error {
	// Create a no-login Linux system account if it doesn't exist yet.
	// id exits 0 if user exists, non-zero otherwise.
	if err := exec.Command("id", username).Run(); err != nil {
		out, err2 := exec.Command("sudo", "useradd",
			"-M",                    // no home directory
			"-s", "/usr/sbin/nologin", // no shell login
			username).CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("useradd: %s", strings.TrimSpace(string(out)))
		}
	}

	// Add to sambashare group (created by samba package; ignore error if absent).
	_ = exec.Command("sudo", "usermod", "-aG", "sambashare", username).Run()

	// Set / update the Samba password (-s = silent, -a = add or update).
	cmd := exec.Command("sudo", "smbpasswd", "-s", "-a", username)
	cmd.Stdin = strings.NewReader(password + "\n" + password + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("smbpasswd: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ChmodSharePath sets permissions on a share path to 0777 so SMB clients can
// read and write regardless of the original dataset ownership.
func ChmodSharePath(path string) error {
	out, err := exec.Command("sudo", "chmod", "777", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chmod %s: %s", path, strings.TrimSpace(string(out)))
	}
	return nil
}

// boolSMB converts a bool to Samba "yes"/"no".
func boolSMB(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// writeFileSudo writes content to a path using sudo tee.
func writeFileSudo(path, content string) error {
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = strings.NewReader(content)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write %s: %s", path, strings.TrimSpace(stderr.String()))
	}
	return nil
}
