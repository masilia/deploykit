// Package notify sends operational messages. A failed notification never aborts the operation that triggered it.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Slack posts text to an incoming-webhook URL. The URL is a secret and is never included in an error.
type Slack struct {
	URL    string
	Client *http.Client
}

// Send posts the message. With an empty URL it does nothing (notifications are optional).
func (s Slack) Send(ctx context.Context, text string) error {
	if s.URL == "" {
		return nil
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack: invalid webhook URL")
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slack: the webhook is unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack: the webhook answered HTTP %d", resp.StatusCode)
	}
	return nil
}
