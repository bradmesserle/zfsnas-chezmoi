package system

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// GetTimezone returns the currently configured system timezone (e.g. "America/New_York").
func GetTimezone() string {
	// Prefer /etc/timezone (Debian/Ubuntu standard).
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		tz := strings.TrimSpace(string(data))
		if tz != "" {
			return tz
		}
	}
	// Fall back to timedatectl.
	out, err := exec.Command("timedatectl", "show", "--property=Timezone", "--value").Output()
	if err == nil {
		tz := strings.TrimSpace(string(out))
		if tz != "" {
			return tz
		}
	}
	return "UTC"
}

// SetTimezone sets the system timezone using timedatectl.
func SetTimezone(tz string) error {
	out, err := exec.Command("sudo", "timedatectl", "set-timezone", tz).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ListTimezones returns all timezone names available on the system.
// It tries timedatectl first, then falls back to walking /usr/share/zoneinfo/.
func ListTimezones() ([]string, error) {
	if out, err := exec.Command("timedatectl", "list-timezones").Output(); err == nil {
		var tzs []string
		for _, line := range strings.Split(string(out), "\n") {
			if t := strings.TrimSpace(line); t != "" {
				tzs = append(tzs, t)
			}
		}
		if len(tzs) > 0 {
			return tzs, nil
		}
	}
	return listTimezonesFromZoneinfo("/usr/share/zoneinfo")
}

// listTimezonesFromZoneinfo walks the zoneinfo directory and returns timezone names.
func listTimezonesFromZoneinfo(root string) ([]string, error) {
	var tzs []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		// Skip non-timezone files (posix/, right/ sub-dirs, leap-seconds.list, etc.)
		rel := strings.TrimPrefix(path, root+"/")
		if strings.HasPrefix(rel, "posix/") || strings.HasPrefix(rel, "right/") ||
			strings.HasPrefix(rel, "+VERSION") || strings.HasSuffix(rel, ".list") ||
			strings.HasSuffix(rel, ".tab") || strings.HasSuffix(rel, ".zi") ||
			!strings.Contains(rel, "/") {
			return nil
		}
		tzs = append(tzs, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("zoneinfo not found: %w", err)
	}
	sort.Strings(tzs)
	return tzs, nil
}
