package handlers

import (
	"net/http"
	"zfsnas/system"
)

// HandleGetDiskIO returns the latest disk I/O snapshot for the pool's member disks.
func HandleGetDiskIO(w http.ResponseWriter, r *http.Request) {
	snap := system.GetDiskIOSnapshot()
	if snap == nil {
		jsonOK(w, map[string]interface{}{"devices": map[string]interface{}{}})
		return
	}
	jsonOK(w, snap)
}
