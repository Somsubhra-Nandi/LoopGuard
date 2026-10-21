package config

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"
)

// Notification events (LLD §15): what a channel's on: list may name.
const (
	EventLoopDetected = "loop_detected" // a loop (D1-D3) reached the nudge threshold
	EventNudge        = "nudge"         // a nudge reached the agent
	EventDeny         = "deny"          // a deny reached the agent
	EventHalt         = "halt"          // a halt reached the agent
	EventStall        = "stall"         // no progress for a while (D5)
	EventBudgetSoft   = "budget_soft"   // a soft budget was passed (D6)
	EventBudgetHard   = "budget_hard"   // a hard budget was passed (D6)
)

// Events lists the notification events.
var Events = []string{EventLoopDetected, EventNudge, EventDeny, EventHalt, EventStall, EventBudgetSoft, EventBudgetHard}

// Notification channels.
const (
	ChannelDesktop  = "desktop"
	ChannelTerminal = "terminal"
	ChannelNtfy     = "ntfy"
	ChannelWebhook  = "webhook"
)

// Webhook formats.
const (
	FormatSlack   = "slack"
	FormatDiscord = "discord"
	FormatGeneric = "generic"
)

// DefaultNtfyServer is the public ntfy server.
const DefaultNtfyServer = "https://ntfy.sh"

// Notify is the notification configuration in effect.
type Notify struct {
	// On is the event list of channels that don't set their own.
	On       []string
	Desktop  Channel
	Terminal Channel
	Ntfy     Channel
	Webhook  Channel
}

// Channel is one notification channel.
type Channel struct {
	Enabled bool
	On      []string // nil: Notify.On
	Server  string   // ntfy
	Topic   string   // ntfy
	URL     string   // webhook
	Format  string   // webhook: slack | discord | generic
}

// Wants reports whether the channel is on and sends event.
func (n Notify) Wants(ch Channel, event string) bool {
	if !ch.Enabled {
		return false
	}
	on := ch.On
	if on == nil {
		on = n.On
	}
	return slices.Contains(on, event)
}

// Channels returns the channels by name.
func (n Notify) Channels() map[string]Channel {
	return map[string]Channel{ChannelDesktop: n.Desktop, ChannelTerminal: n.Terminal, ChannelNtfy: n.Ntfy, ChannelWebhook: n.Webhook}
}

// DefaultNotify returns the defaults: desktop notifications for loops,
// halts, and budgets passed, nothing else.
func DefaultNotify() Notify {
	return Notify{
		On:       []string{EventLoopDetected, EventHalt, EventBudgetSoft, EventBudgetHard},
		Desktop:  Channel{Enabled: true},
		Terminal: Channel{},
		Ntfy:     Channel{Server: DefaultNtfyServer},
		Webhook:  Channel{Format: FormatSlack},
	}
}

// NotifyFile is the notify: section of config.yaml.
type NotifyFile struct {
	On       *[]string    `yaml:"on"`
	Desktop  *ChannelFile `yaml:"desktop"`
	Terminal *ChannelFile `yaml:"terminal"`
	Ntfy     *ChannelFile `yaml:"ntfy"`
	Webhook  *ChannelFile `yaml:"webhook"`
}

// ChannelFile is one channel's section. It may also be a plain boolean
// (`desktop: true`), the shape of earlier LLD samples.
type ChannelFile struct {
	Enabled *bool     `yaml:"enabled"`
	On      *[]string `yaml:"on"`
	Server  *string   `yaml:"server"`
	Topic   *string   `yaml:"topic"`
	URL     *string   `yaml:"url"`
	Format  *string   `yaml:"format"`
}

// UnmarshalYAML accepts a mapping or a boolean.
func (c *ChannelFile) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var b bool
		if err := n.Decode(&b); err != nil {
			return fmt.Errorf("line %d: a notification channel is true, false, or a mapping", n.Line)
		}
		*c = ChannelFile{Enabled: &b}
		return nil
	}
	type plain ChannelFile
	return n.Decode((*plain)(c))
}

// apply overrides n with the keys set in f.
func (n Notify) apply(f *NotifyFile) Notify {
	if f == nil {
		return n
	}
	if f.On != nil {
		n.On = append([]string{}, (*f.On)...)
	}
	n.Desktop = n.Desktop.apply(f.Desktop)
	n.Terminal = n.Terminal.apply(f.Terminal)
	n.Ntfy = n.Ntfy.apply(f.Ntfy)
	n.Webhook = n.Webhook.apply(f.Webhook)
	return n
}

// apply overrides c with the keys set in f. A topic or URL without an
// enabled key turns the channel on (the shape of earlier samples, where
// an empty topic meant off).
func (c Channel) apply(f *ChannelFile) Channel {
	if f == nil {
		return c
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&c.Server, f.Server)
	set(&c.Topic, f.Topic)
	set(&c.URL, f.URL)
	set(&c.Format, f.Format)
	if f.On != nil {
		c.On = append([]string{}, (*f.On)...)
	}
	switch {
	case f.Enabled != nil:
		c.Enabled = *f.Enabled
	case f.Topic != nil:
		c.Enabled = *f.Topic != ""
	case f.URL != nil:
		c.Enabled = *f.URL != ""
	}
	return c
}

// topicRE is what LoopGuard accepts as an ntfy topic (ntfy's own rule).
var topicRE = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

// validate checks the values set in f.
func (f *NotifyFile) validate(bad func(string, ...any)) {
	if f == nil {
		return
	}
	events := func(key string, on *[]string) {
		if on == nil {
			return
		}
		for _, e := range *on {
			if !slices.Contains(Events, e) {
				bad("%s: unknown event %q (use %v)", key, e, Events)
			}
		}
	}
	events("notify.on", f.On)
	for name, c := range map[string]*ChannelFile{ChannelDesktop: f.Desktop, ChannelTerminal: f.Terminal, ChannelNtfy: f.Ntfy, ChannelWebhook: f.Webhook} {
		if c == nil {
			continue
		}
		events("notify."+name+".on", c.On)
	}
	if c := f.Ntfy; c != nil {
		if c.Topic != nil && *c.Topic != "" && !topicRE.MatchString(*c.Topic) {
			bad("notify.ntfy.topic: use letters, digits, - and _ (at most 64)")
		}
		if c.Server != nil {
			if err := httpURL(*c.Server); err != nil {
				bad("notify.ntfy.server: %v", err)
			}
		}
	}
	if c := f.Webhook; c != nil {
		if c.URL != nil && *c.URL != "" {
			if err := httpURL(*c.URL); err != nil {
				bad("notify.webhook.url: %v", err) // never print the URL: it is a secret
			}
		}
		if c.Format != nil && !slices.Contains([]string{FormatSlack, FormatDiscord, FormatGeneric}, *c.Format) {
			bad("notify.webhook.format: %q is not slack, discord, or generic", *c.Format)
		}
	}
}

func httpURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("not an http(s) URL")
	}
	return nil
}
