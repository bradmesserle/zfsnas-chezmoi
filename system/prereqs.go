package system

import (
	"os/exec"
	"strings"
)

// Package describes a required system package and its install status.
type Package struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Installed   bool   `json:"installed"`
}

// RequiredPackages lists every package the application needs.
var RequiredPackages = []Package{
	{Name: "zfsutils-linux", Description: "ZFS pool and dataset management"},
	{Name: "samba", Description: "Windows file sharing (SMB/CIFS)"},
	{Name: "nfs-kernel-server", Description: "Linux NFS server (NFS exports)"},
	{Name: "smartmontools", Description: "SSD/HDD health monitoring (smartctl)"},
	{Name: "nvme-cli", Description: "NVMe drive health monitoring"},
	{Name: "util-linux", Description: "Disk utilities (lsblk)"},
}

// CheckPackages returns RequiredPackages with Installed populated.
func CheckPackages() []Package {
	result := make([]Package, len(RequiredPackages))
	copy(result, RequiredPackages)
	for i := range result {
		result[i].Installed = isInstalled(result[i].Name)
	}
	return result
}

// MissingPackages returns the names of packages that are not installed.
func MissingPackages(pkgs []Package) []string {
	var missing []string
	for _, p := range pkgs {
		if !p.Installed {
			missing = append(missing, p.Name)
		}
	}
	return missing
}

// isInstalled checks whether a Debian/Ubuntu package is fully installed.
func isInstalled(pkg string) bool {
	out, err := exec.Command("dpkg", "-s", pkg).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Status: install ok installed")
}

// IsServiceInstalled returns true if the zfsnas systemd unit exists and is enabled.
func IsServiceInstalled() bool {
	out, err := exec.Command("systemctl", "is-enabled", "zfsnas").Output()
	if err != nil {
		return false
	}
	status := strings.TrimSpace(string(out))
	return status == "enabled" || status == "static"
}
