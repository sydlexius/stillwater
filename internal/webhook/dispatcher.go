package webhook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sydlexius/stillwater/internal/event"
)

const (
	maxRetries     = 3
	requestTimeout = 10 * time.Second
)

// Dispatcher sends events to matching webhooks.
type Dispatcher struct {
	service    *Service
	httpClient *http.Client
	logger     *slog.Logger
}

// NewDispatcher creates a webhook dispatcher.
func NewDispatcher(service *Service, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		service:    service,
		httpClient: &http.Client{Timeout: requestTimeout},
		logger:     logger.With(slog.String("component", "webhook-dispatcher")),
	}
}

// NewDispatcherWithHTTPClient creates a dispatcher with a custom HTTP client (for testing).
func NewDispatcherWithHTTPClient(service *Service, httpClient *http.Client, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		service:    service,
		httpClient: httpClient,
		logger:     logger.With(slog.String("component", "webhook-dispatcher")),
	}
}

// HandleEvent is an event.Handler that dispatches the event to all matching webhooks.
func (d *Dispatcher) HandleEvent(e event.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	webhooks, err := d.service.ListByEvent(ctx, string(e.Type))
	if err != nil {
		d.logger.Error("listing webhooks for event", "type", string(e.Type), "error", err)
		return
	}

	for i := range webhooks {
		w := webhooks[i]
		go d.deliver(w, e)
	}
}

func (d *Dispatcher) deliver(w Webhook, e event.Event) {
	body, contentType := formatPayload(&w, e)

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			time.Sleep(backoff)
		}

		lastErr = d.send(w.URL, body, contentType)
		if lastErr == nil {
			d.logger.Debug("webhook delivered",
				"webhook", w.Name,
				"event", string(e.Type),
				"attempt", attempt+1,
			)
			return
		}

		d.logger.Warn("webhook delivery failed",
			"webhook", w.Name,
			"event", string(e.Type),
			"attempt", attempt+1,
			"error", lastErr,
		)
	}

	d.logger.Error("webhook delivery exhausted retries",
		"webhook", w.Name,
		"event", string(e.Type),
		"error", lastErr,
	)
}

func (d *Dispatcher) send(url string, body []byte, contentType string) error {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "Stillwater-Webhook/1.0")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	io.Copy(io.Discard, resp.Body)  //nolint:errcheck

	if resp.StatusCode >= 400 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}
