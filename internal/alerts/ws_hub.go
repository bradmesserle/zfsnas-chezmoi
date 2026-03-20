package alerts

import (
	"encoding/json"
	"sync"

	"github.com/gorilla/websocket"
)

// AlertsHub broadcasts alert payloads to all connected browser sessions.
type AlertsHub struct {
	mu      sync.Mutex
	clients map[*alertsClient]struct{}
}

type alertsClient struct {
	conn *websocket.Conn
	send chan []byte
}

// NewAlertsHub creates a ready-to-use AlertsHub.
func NewAlertsHub() *AlertsHub {
	return &AlertsHub{clients: make(map[*alertsClient]struct{})}
}

// Register upgrades the connection, registers the client, and starts read/write pumps.
func (h *AlertsHub) Register(conn *websocket.Conn) {
	c := &alertsClient{conn: conn, send: make(chan []byte, 32)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	// Write pump — forwards queued messages to the browser.
	go func() {
		defer func() {
			h.mu.Lock()
			delete(h.clients, c)
			h.mu.Unlock()
			conn.Close()
		}()
		for msg := range c.send {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()

	// Read pump — discards incoming data and detects disconnection.
	go func() {
		defer close(c.send)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

// BroadcastJSON marshals v and sends it to all connected clients.
func (h *AlertsHub) BroadcastJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- append([]byte(nil), data...):
		default: // slow client: drop
		}
	}
}
