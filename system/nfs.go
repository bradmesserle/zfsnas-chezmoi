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
	exportsPath    = "/etc/exports"
	nfsBeginMarker = "# ===== ZFS NAS MANAGED EXPORTS BEGIN ====="
	nfsEndMarker   = "# ===== ZFS NAS MANAGED EXPORTS END ====="
)

// NFSShare represents a single /etc/exports entry.
type NFSShare struct {
	ID             string `json:"id"`
	Path           string `json:"path"`
	Client         string `json:"client"` // CIDR or "*"
	ReadOnly       bool   `json:"read_only"`
	Sync           bool   `json:"sync"`
	NoSubtreeCheck bool   `json:"no_subtree_check"`
	NoRootSquash   bool   `json:"no_root_squash"`
	Comment        string `json:"comment"`
}

func nfsSharesPath(configDir string) string {
	return filepath.Join(configDir, "nfs-shares.json")
}

// ListNFSShares returns all configured NFS shares from the JSON store.
func ListNFSShares(configDir string) ([]NFSShare, error) {
	data, err := os.ReadFile(nfsSharesPath(configDir))
	if os.IsNotExist(err) {
		return []NFSShare{}, nil
	}
	if err != nil {
		return nil, err
	}
	var shares []NFSShare
	if err := json.Unmarshal(data, &shares); err != nil {
		return nil, err
	}
	if shares == nil {
		return []NFSShare{}, nil
	}
	return shares, nil
}

// SaveNFSShares persists shares to JSON and writes /etc/exports.
func SaveNFSShares(configDir string, shares []NFSShare) error {
	data, err := json.MarshalIndent(shares, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(nfsSharesPath(configDir), data, 0640); err != nil {
		return err
	}
	return applyExports(shares)
}

func applyExports(shares []NFSShare) error {
	var sb strings.Builder
	sb.WriteString(nfsBeginMarker + "\n")
	for _, s := range shares {
		if s.Comment != "" {
			sb.WriteString("# " + s.Comment + "\n")
		}
		client := s.Client
		if client == "" {
			client = "*"
		}
		sb.WriteString(fmt.Sprintf("%s %s(%s)\n", s.Path, client, nfsOpts(s)))
	}
	sb.WriteString(nfsEndMarker + "\n")
	managed := sb.String()

	existing, err := os.ReadFile(exportsPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read /etc/exports: %w", err)
	}
	conf := string(existing)

	begin := strings.Index(conf, nfsBeginMarker)
	end := strings.Index(conf, nfsEndMarker)
	var newConf string
	if begin >= 0 && end > begin {
		newConf = conf[:begin] + managed + conf[end+len(nfsEndMarker):]
		newConf = strings.ReplaceAll(newConf, "\n\n\n", "\n\n")
	} else {
		newConf = strings.TrimRight(conf, "\n") + "\n\n" + managed
	}

	if err := writeFileSudo(exportsPath, newConf); err != nil {
		return err
	}
	return ExportFS()
}

func nfsOpts(s NFSShare) string {
	opts := []string{}
	if s.ReadOnly {
		opts = append(opts, "ro")
	} else {
		opts = append(opts, "rw")
	}
	if s.Sync {
		opts = append(opts, "sync")
	} else {
		opts = append(opts, "async")
	}
	if s.NoSubtreeCheck {
		opts = append(opts, "no_subtree_check")
	}
	if s.NoRootSquash {
		opts = append(opts, "no_root_squash")
	}
	return strings.Join(opts, ",")
}

// ExportFS applies the current /etc/exports to the running kernel.
func ExportFS() error {
	out, err := exec.Command("sudo", "exportfs", "-ra").CombinedOutput()
	if err != nil {
		return fmt.Errorf("exportfs -ra: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// IsNFSInstalled checks whether the exportfs binary is present.
func IsNFSInstalled() bool {
	_, err := exec.LookPath("exportfs")
	return err == nil
}

// NFSStatus returns "active", "inactive", or "not-installed".
func NFSStatus() string {
	if !IsNFSInstalled() {
		return "not-installed"
	}
	out, err := exec.Command("systemctl", "is-active", "nfs-server").Output()
	if err != nil {
		return "inactive"
	}
	return strings.TrimSpace(string(out))
}

// ControlNFS runs systemctl start/stop/restart on nfs-server.
func ControlNFS(action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("invalid action: %s", action)
	}
	out, err := exec.Command("sudo", "systemctl", action, "nfs-server").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s nfs-server: %s", action, strings.TrimSpace(string(out)))
	}
	return nil
}
