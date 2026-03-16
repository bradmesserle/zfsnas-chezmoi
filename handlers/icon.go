package handlers

import (
	"net/http"
)

// HandleAppleTouchIcon serves favicon.jpg for iOS/iPadOS home screen icons.
func HandleAppleTouchIcon(readFile func(string) ([]byte, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := readFile("favicon.jpg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data) //nolint:errcheck
	}
}
