package system

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"zfsnas/internal/config"
)

// RunReplication executes a ZFS send/receive replication job. It:
//  1. Creates a new snapshot (or reuses existingSnap if provided)
//  2. Sends it to the remote host using zfs send | ssh zfs receive
//  3. Returns the new snapshot name so the caller can update task.LastSnap
//
// existingSnap, if non-empty, is the full dataset@snap name of an already-created
// snapshot to replicate — no new snapshot will be created.
// send is called with each line of combined output from the pipeline.
// RunReplication executes a ZFS send/receive replication job.
// existingSnap, if non-empty, is the full "dataset@snapname" of an already-created snapshot.
// Returns the snap-name suffix (the part after "@") so callers can store it as LastSnap.
func RunReplication(task *config.ReplicationTask, send func(string), existingSnap string) (snapSuffix string, err error) {
	var fullSnapName string // dataset@snapname — used in zfs send
	if existingSnap != "" {
		// Reuse the snapshot that was already created (e.g. by the scheduler).
		fullSnapName = existingSnap
		at := strings.LastIndex(fullSnapName, "@")
		if at >= 0 {
			snapSuffix = fullSnapName[at+1:]
		} else {
			snapSuffix = fullSnapName
		}
		send(fmt.Sprintf("Replicating existing snapshot: %s", fullSnapName))
	} else {
		snapSuffix = fmt.Sprintf("zfsnas-rep-%s-%s", task.ID[:8], time.Now().UTC().Format("20060102T150405Z"))
		fullSnapName = task.SourceDataset + "@" + snapSuffix

		// Create the snapshot.
		send(fmt.Sprintf("Creating snapshot: %s", fullSnapName))
		snapArgs := []string{"snapshot"}
		if task.Recursive {
			snapArgs = append(snapArgs, "-r")
		}
		snapArgs = append(snapArgs, fullSnapName)
		if out, snapErr := exec.Command("sudo", append([]string{"zfs"}, snapArgs...)...).CombinedOutput(); snapErr != nil {
			return "", fmt.Errorf("zfs snapshot failed: %w: %s", snapErr, strings.TrimSpace(string(out)))
		}
		send("Snapshot created.")
	}

	// Build the zfs send command.
	sendArgs := []string{"send"}
	if task.Recursive {
		sendArgs = append(sendArgs, "-R")
	}
	if task.Compressed {
		sendArgs = append(sendArgs, "-c")
	}
	if task.LastSnap != "" {
		sendArgs = append(sendArgs, "-I", task.SourceDataset+"@"+task.LastSnap)
	}
	sendArgs = append(sendArgs, fullSnapName)

	// Build the ssh zfs receive command.
	remoteUser := task.RemoteUser
	if remoteUser == "" {
		remoteUser = "root"
	}
	receiveCmd := fmt.Sprintf("zfs receive -F %s", task.RemoteDataset)
	sshTarget := fmt.Sprintf("%s@%s", remoteUser, task.RemoteHost)
	shellCmd := fmt.Sprintf("sudo zfs %s | ssh -o BatchMode=yes -o StrictHostKeyChecking=no %s '%s'",
		strings.Join(sendArgs, " "), sshTarget, receiveCmd)

	send(fmt.Sprintf("Running: %s", shellCmd))
	send("─────────────────────────────────────────")

	cmd := exec.Command("sh", "-c", shellCmd)

	pr, pw, pipeErr := os.Pipe()
	if pipeErr != nil {
		return "", fmt.Errorf("pipe: %w", pipeErr)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if startErr := cmd.Start(); startErr != nil {
		pw.Close()
		pr.Close()
		return "", fmt.Errorf("start: %w", startErr)
	}
	pw.Close()

	buf := make([]byte, 4096)
	for {
		n, readErr := pr.Read(buf)
		if n > 0 {
			for _, l := range strings.Split(string(buf[:n]), "\n") {
				if strings.TrimSpace(l) != "" {
					send(l)
				}
			}
		}
		if readErr != nil {
			break
		}
	}

	if waitErr := cmd.Wait(); waitErr != nil {
		return "", fmt.Errorf("replication failed: %w", waitErr)
	}

	return snapSuffix, nil
}
