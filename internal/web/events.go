package web

import (
	"encoding/json"
	"fmt"
	"sync"
)

// EventType represents different types of SSE events.
type EventType string

const (
	EventQueueAdded     EventType = "queue_added"
	EventQueueUpdated   EventType = "queue_updated"
	EventStatusChanged  EventType = "status_changed"
	EventProgressUpdate EventType = "progress_update"
	EventScanComplete   EventType = "scan_complete"
	EventTaskLog        EventType = "task_log"
	EventQueueItemHTML  EventType = "queue_item_html"
)

// Event represents a server-sent event.
type Event struct {
	Type EventType   `json:"type"`
	Data interface{} `json:"data"`
}

// EventBroadcaster manages SSE connections and broadcasts events.
type EventBroadcaster struct {
	mu      sync.RWMutex
	clients map[chan Event]bool
}

// NewEventBroadcaster creates a new event broadcaster.
func NewEventBroadcaster() *EventBroadcaster {
	return &EventBroadcaster{
		clients: make(map[chan Event]bool),
	}
}

// Register adds a new client channel.
func (eb *EventBroadcaster) Register(client chan Event) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.clients[client] = true
}

// Unregister removes a client channel.
func (eb *EventBroadcaster) Unregister(client chan Event) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	delete(eb.clients, client)
	close(client)
}

// Broadcast sends an event to all connected clients.
func (eb *EventBroadcaster) Broadcast(event Event) {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	for client := range eb.clients {
		select {
		case client <- event:
			// Event sent successfully
		default:
			// Client channel is full, skip
		}
	}
}

// BroadcastQueueAdded broadcasts a queue added event.
func (eb *EventBroadcaster) BroadcastQueueAdded(fileID uint, fileName string) {
	eb.Broadcast(Event{
		Type: EventQueueAdded,
		Data: map[string]interface{}{
			"file_id":   fileID,
			"file_name": fileName,
		},
	})
}

// BroadcastQueueUpdated broadcasts a queue updated event.
func (eb *EventBroadcaster) BroadcastQueueUpdated(queueID uint, fileID uint, status string) {
	eb.Broadcast(Event{
		Type: EventQueueUpdated,
		Data: map[string]interface{}{
			"queue_id": queueID,
			"file_id":  fileID,
			"status":   status,
		},
	})
}

// BroadcastStatusChanged broadcasts a status change event.
func (eb *EventBroadcaster) BroadcastStatusChanged(fileID uint, oldStatus, newStatus string) {
	eb.Broadcast(Event{
		Type: EventStatusChanged,
		Data: map[string]interface{}{
			"file_id":    fileID,
			"old_status": oldStatus,
			"new_status": newStatus,
		},
	})
}

// BroadcastScanComplete broadcasts a scan complete event.
func (eb *EventBroadcaster) BroadcastScanComplete(fileCount int) {
	eb.Broadcast(Event{
		Type: EventScanComplete,
		Data: map[string]interface{}{
			"file_count": fileCount,
		},
	})
}

// BroadcastTaskLog broadcasts a task log event.
func (eb *EventBroadcaster) BroadcastTaskLog(queueItemID, fileID uint, logLevel, message string, timestamp string) {
	eb.Broadcast(Event{
		Type: EventTaskLog,
		Data: map[string]interface{}{
			"queue_item_id": queueItemID,
			"file_id":       fileID,
			"log_level":     logLevel,
			"message":       message,
			"created_at":    timestamp,
		},
	})
}

// BroadcastProgressUpdate broadcasts a transcoding progress update event.
func (eb *EventBroadcaster) BroadcastProgressUpdate(
	queueItemID, fileID uint,
	fps float64,
	speed string,
	outTimeS float64,
	sizeBytes int64,
	device string,
) {
	eb.Broadcast(Event{
		Type: EventProgressUpdate,
		Data: map[string]interface{}{
			"queue_item_id": queueItemID,
			"file_id":       fileID,
			"fps":           fps,
			"speed":         speed,
			"out_time_s":    outTimeS,
			"size_bytes":    sizeBytes,
			"device":        device,
		},
	})
}

// BroadcastQueueItemHTML broadcasts an HTML fragment for a queue item.
func (eb *EventBroadcaster) BroadcastQueueItemHTML(queueItemID uint, html string) {
	eb.Broadcast(Event{
		Type: EventQueueItemHTML,
		Data: map[string]interface{}{
			"queue_item_id": queueItemID,
			"html":          html,
		},
	})
}

// FormatSSE formats an event as SSE format.
func FormatSSE(event Event) string {
	data, err := json.Marshal(event)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event.Type, string(data))
}
