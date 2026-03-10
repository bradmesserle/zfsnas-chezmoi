package system

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// diskstatSample holds one raw reading from /proc/diskstats for a device.
type diskstatSample struct {
	sectorsRead    uint64
	sectorsWritten uint64
	msIO           uint64 // ms spent doing I/O (field 12)
	ts             time.Time
}

// DiskIOSample is the computed per-device I/O metrics for one interval.
type DiskIOSample struct {
	ReadKBps  float64 `json:"read_kbps"`
	WriteKBps float64 `json:"write_kbps"`
	BusyPct   float64 `json:"busy_pct"`
}

// DiskIOSnapshot is the full snapshot returned by the API.
type DiskIOSnapshot struct {
	Devices map[string]DiskIOSample `json:"devices"` // key = kernel device name (e.g. "sda")
	At      time.Time               `json:"at"`
}

var (
	diskIOMu      sync.RWMutex
	diskIOLatest  *DiskIOSnapshot
	diskIOPrev    map[string]diskstatSample
)

// StartDiskIOPoller samples /proc/diskstats every 2 s, keeping the latest
// computed snapshot for the pool's member disks.
func StartDiskIOPoller() {
	diskIOPrev = make(map[string]diskstatSample)
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for range tick.C {
			poolDevs := poolMemberBaseNames()
			if len(poolDevs) == 0 {
				continue
			}
			raw, err := readDiskstats(poolDevs)
			if err != nil {
				continue
			}
			now := time.Now()
			snap := &DiskIOSnapshot{
				Devices: make(map[string]DiskIOSample, len(poolDevs)),
				At:      now,
			}
			diskIOMu.Lock()
			for dev, cur := range raw {
				prev, hasPrev := diskIOPrev[dev]
				if hasPrev {
					dtSec := cur.ts.Sub(prev.ts).Seconds()
					if dtSec > 0 {
						readKBps := float64(cur.sectorsRead-prev.sectorsRead) * 512 / 1024 / dtSec
						writeKBps := float64(cur.sectorsWritten-prev.sectorsWritten) * 512 / 1024 / dtSec
						dtMS := dtSec * 1000
						busyPct := float64(cur.msIO-prev.msIO) / dtMS * 100
						if busyPct > 100 {
							busyPct = 100
						}
						snap.Devices[dev] = DiskIOSample{
							ReadKBps:  readKBps,
							WriteKBps: writeKBps,
							BusyPct:   busyPct,
						}
					}
				}
				diskIOPrev[dev] = cur
			}
			diskIOLatest = snap
			diskIOMu.Unlock()
		}
	}()
}

// GetDiskIOSnapshot returns the most-recently computed I/O snapshot.
func GetDiskIOSnapshot() *DiskIOSnapshot {
	diskIOMu.RLock()
	defer diskIOMu.RUnlock()
	return diskIOLatest
}

// poolMemberBaseNames returns the kernel device names (e.g. "sda") for the
// current ZFS pool's member devices (e.g. "/dev/sda" → "sda").
func poolMemberBaseNames() []string {
	pool, err := GetPool()
	if err != nil || pool == nil {
		return nil
	}
	names := make([]string, 0, len(pool.Members))
	for _, m := range pool.Members {
		// Resolve symlinks (e.g. /dev/disk/by-partuuid/xxx → /dev/sda1)
		// so we get the real kernel device name for /proc/diskstats lookups.
		real, err := filepath.EvalSymlinks(m)
		if err != nil {
			real = m
		}
		names = append(names, diskBaseName(filepath.Base(real)))
	}
	return names
}

// readDiskstats reads /proc/diskstats and returns samples for the requested devices.
func readDiskstats(devs []string) (map[string]diskstatSample, error) {
	want := make(map[string]bool, len(devs))
	for _, d := range devs {
		want[d] = true
	}

	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	now := time.Now()
	result := make(map[string]diskstatSample, len(devs))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}
		name := fields[2]
		if !want[name] {
			continue
		}
		sectorsRead, _ := strconv.ParseUint(fields[5], 10, 64)
		sectorsWritten, _ := strconv.ParseUint(fields[9], 10, 64)
		msIO, _ := strconv.ParseUint(fields[12], 10, 64)
		result[name] = diskstatSample{
			sectorsRead:    sectorsRead,
			sectorsWritten: sectorsWritten,
			msIO:           msIO,
			ts:             now,
		}
	}
	return result, scanner.Err()
}
