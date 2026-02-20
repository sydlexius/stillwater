package webhook

import (
	"encoding/json"
	"fmt"

	"github.com/sydlexius/stillwater/internal/event"
)

// formatPayload returns the request body and content-type for a webhook delivery.
func formatPayload(w *Webhook, e event.Event) ([]byte, string) {
	switch w.Type {
	case TypeDiscord:
		return formatDiscord(e)
	case TypeSlack:
		return formatSlack(e)
	case TypeGotify:
		return formatGotify(e)
	default:
		return formatGeneric(e)
	}
}

func formatGeneric(e event.Event) ([]byte, string) {
	payload := map[string]any{
		"event":     string(e.Type),
		"timestamp": e.Timestamp,
		"data":      e.Data,
	}
	body, _ := json.Marshal(payload)
	return body, "application/json"
}

func formatDiscord(e event.Event) ([]byte, string) {
	title := fmt.Sprintf("Stillwater: %s", e.Type)
	desc := formatDescription(e)

	payload := map[string]any{
		"embeds": []map[string]any{
			{
				"title":       title,
				"description": desc,
				"color":       3447003, // blue
				"timestamp":   e.Timestamp.Format("2006-01-02T15:04:05Z"),
			},
		},
	}
	body, _ := json.Marshal(payload)
	return body, "application/json"
}

func formatSlack(e event.Event) ([]byte, string) {
	text := fmt.Sprintf("*Stillwater: %s*\n%s", e.Type, formatDescription(e))
	payload := map[string]any{
		"text": text,
	}
	body, _ := json.Marshal(payload)
	return body, "application/json"
}

func formatGotify(e event.Event) ([]byte, string) {
	payload := map[string]any{
		"title":   fmt.Sprintf("Stillwater: %s", e.Type),
		"message": formatDescription(e),
	}
	body, _ := json.Marshal(payload)
	return body, "application/json"
}

func formatDescription(e event.Event) string {
	if e.Data == nil {
		return string(e.Type)
	}
	if msg, ok := e.Data["message"].(string); ok {
		return msg
	}
	b, _ := json.Marshal(e.Data)
	return string(b)
}
