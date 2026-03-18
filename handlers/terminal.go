package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

var termUpgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        origin := r.Header.Get("Origin")
        if origin == "" {
            return true // non-browser clients (curl, etc.)
        }
        return strings.HasSuffix(origin, "://"+r.Host)
    },
}

// termMsg is a control message sent from the browser to signal a terminal resize.
type termMsg struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// HandleTerminal opens a WebSocket-backed PTY shell for the authenticated user.
func HandleTerminal(w http.ResponseWriter, r *http.Request) {
	conn, err := termUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("terminal: pty start: %v", err)
		conn.WriteMessage(websocket.TextMessage, []byte("Failed to start shell: "+err.Error()+"\r\n"))
		return
	}
	defer func() {
		cmd.Process.Kill()
		ptmx.Close()
	}()

	var once sync.Once
	done := make(chan struct{})

	// PTY → WebSocket (terminal output)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
			if err != nil {
				once.Do(func() { close(done) })
				return
			}
		}
	}()

	// WebSocket → PTY (keyboard input + resize)
	go func() {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				once.Do(func() { close(done) })
				return
			}
			if mt == websocket.TextMessage {
				// Try to parse as a resize control message.
				var msg termMsg
				if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
					pty.Setsize(ptmx, &pty.Winsize{Cols: msg.Cols, Rows: msg.Rows})
					continue
				}
			}
			// Binary or unrecognised text → raw input to shell.
			io.Copy(ptmx, newBytesReader(data))
		}
	}()

	<-done
}

// newBytesReader wraps a byte slice in an io.Reader.
type bytesReader struct{ b []byte; pos int }

func newBytesReader(b []byte) io.Reader { return &bytesReader{b: b} }
func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}
