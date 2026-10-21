package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
)

// Sender delivers a notification on one channel.
type Sender interface {
	Send(ctx context.Context, ch config.Channel, n Notification) error
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, ch config.Channel, n Notification) error

// Send implements Sender.
func (f SenderFunc) Send(ctx context.Context, ch config.Channel, n Notification) error {
	return f(ctx, ch, n)
}

// Senders are the asynchronous channels' senders, by channel name. The
// terminal channel has none: it goes out with a hook reply.
type Senders map[string]Sender

// DefaultSenders sends for real: desktop notifications through the
// platform's tool, ntfy and webhooks over HTTPS.
func DefaultSenders() Senders {
	c := &http.Client{Timeout: sendTimeout}
	return Senders{
		config.ChannelDesktop: Desktop{},
		config.ChannelNtfy:    Ntfy{Client: c},
		config.ChannelWebhook: Webhook{Client: c},
	}
}

// sendTimeout bounds one attempt.
const sendTimeout = 10 * time.Second

// Ntfy publishes to an ntfy server (LLD §15). It uses ntfy's JSON form
// (POST to the server root), so non-ASCII titles need no header encoding.
type Ntfy struct{ Client *http.Client }

// Send implements Sender.
func (s Ntfy) Send(ctx context.Context, ch config.Channel, n Notification) error {
	if ch.Topic == "" {
		return errors.New("no topic (run: loopguard notify setup ntfy)")
	}
	server := strings.TrimRight(ch.Server, "/")
	if server == "" {
		server = config.DefaultNtfyServer
	}
	priority := 3
	if n.Event == config.EventHalt {
		priority = 4
	}
	body, _ := json.Marshal(map[string]any{
		"topic": ch.Topic, "title": n.Title(), "message": n.Body(), "priority": priority, "tags": []string{"loopguard"},
	})
	return post(ctx, s.Client, server, body)
}

// Webhook posts to a Slack or Discord incoming webhook, or any URL that
// takes JSON (generic).
type Webhook struct{ Client *http.Client }

// Send implements Sender.
func (s Webhook) Send(ctx context.Context, ch config.Channel, n Notification) error {
	if ch.URL == "" {
		return errors.New("no url")
	}
	body, _ := json.Marshal(WebhookPayload(ch.Format, n))
	return post(ctx, s.Client, ch.URL, body)
}

// WebhookPayload is the JSON a webhook gets in a format.
func WebhookPayload(format string, n Notification) any {
	text := n.Title() + "\n" + n.Body()
	switch format {
	case config.FormatDiscord:
		return map[string]string{"content": text}
	case config.FormatGeneric:
		return map[string]any{
			"source": "loopguard", "event": n.Event, "title": n.Title(), "body": n.Body(), "project": n.Project,
			"detector": n.Detector, "family": n.Family, "count": n.Count, "duration_seconds": int(n.Duration.Seconds()),
			"session": n.Session, "stop_command": n.StopCommand(), "sent_at": n.At.UTC().Format(time.RFC3339),
		}
	}
	return map[string]string{"text": text} // slack
}

// post sends a JSON body and checks for a 2xx answer. Errors never carry
// the URL: a webhook URL is a secret.
func post(ctx context.Context, c *http.Client, u string, body []byte) error {
	if c == nil {
		c = &http.Client{Timeout: sendTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "loopguard")
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("server answered %s", resp.Status)
	}
	return nil
}

// NewTopic returns a new ntfy topic name: "loopguard-" and 26 random
// characters (128 bits). ntfy topics are public, so whoever knows the name
// can read and send; it must not be guessable.
func NewTopic() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "loopguard-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}
