package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/notify"
)

// notifySenders sends `loopguard notify test` (tests replace it).
var notifySenders = notify.DefaultSenders

func newNotifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "notify [status|setup|test]",
		Short: "Show, set up, or test notifications (desktop, terminal, ntfy, webhook)",
		Long: `LoopGuard notifies you about loops at the notify, assist, and guard levels
(not in observe). The channels and the events each one gets are set under
notify: in config.yaml:

  notify:
    on: [loop_detected, halt]       # events for channels without their own on:
    desktop:  { enabled: true }      # Windows toast, macOS Notification Center, libnotify
    terminal: { enabled: false }     # in Claude Code's terminal (interactive sessions only)
    ntfy:     { enabled: false, server: https://ntfy.sh, topic: "", on: [halt] }
    webhook:  { enabled: false, url: "", format: slack }   # slack | discord | generic

Events: loop_detected, nudge, deny, halt, stall. A notification names the
project folder, the detector, the command, a count, how long the loop has
lasted, and how to stop it; never file paths, error lines, or code. At most
one per loop every 10 minutes (a halt always gets through) and 5 per hour,
per channel.

  loopguard notify                     show the channels
  loopguard notify setup ntfy          phone notifications through ntfy
  loopguard notify setup webhook URL   Slack, Discord, or any JSON webhook
  loopguard notify test                send a test to every enabled channel`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] != "status" {
				return fmt.Errorf("unknown notify command %q (use status, setup, or test)", args[0])
			}
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			return printNotify(cmd.OutOrStdout(), dir)
		},
	}
	setup := &cobra.Command{
		Use:   "setup ntfy|webhook",
		Short: "Set up phone (ntfy) or webhook notifications",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errors.New("give a channel: loopguard notify setup ntfy, or loopguard notify setup webhook URL")
		},
	}
	setup.AddCommand(newSetupNtfyCmd(), newSetupWebhookCmd())
	cmd.AddCommand(setup, newNotifyTestCmd(), &cobra.Command{
		Use: "status", Short: "Show the notification channels", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			return printNotify(cmd.OutOrStdout(), dir)
		},
	})
	return cmd
}

// notifyConfig reads the notification settings from config.yaml.
func notifyConfig(dir string) (config.Notify, config.ModeInfo, error) {
	e := config.NewLoader(dir, config.LoaderOptions{}).For("")
	if e.Global.Err != nil {
		return config.Notify{}, config.ModeInfo{}, fmt.Errorf("%s is invalid: %v", e.Global.Path, e.Global.Err)
	}
	return e.Notify, e.ModeAt(time.Now()), nil
}

func printNotify(out io.Writer, dir string) error {
	n, mode, err := notifyConfig(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Level: %s (%s)\n", mode.Mode, mode.Why)
	if !config.Notifies(mode.Mode) {
		fmt.Fprintln(out, "  Nothing is sent at this level; notifications start at: loopguard mode notify")
	}
	for _, name := range []string{config.ChannelDesktop, config.ChannelTerminal, config.ChannelNtfy, config.ChannelWebhook} {
		ch := n.Channels()[name]
		if !ch.Enabled {
			fmt.Fprintf(out, "  %-9s off\n", name)
			continue
		}
		on := ch.On
		if on == nil {
			on = n.On
		}
		fmt.Fprintf(out, "  %-9s on   %s%s\n", name, strings.Join(on, ", "), channelDetail(name, ch))
	}
	fmt.Fprintln(out, "Limits: 1 per loop per 10 min (a halt always gets through), 5 per hour, per channel.")
	return nil
}

// channelDetail describes where a channel sends; a webhook only by host
// (its URL is a secret).
func channelDetail(name string, ch config.Channel) string {
	switch name {
	case config.ChannelDesktop:
		ok, how := notify.DesktopAvailable()
		if !ok {
			return " (unavailable: " + how + ")"
		}
		return " (" + how + ")"
	case config.ChannelTerminal:
		return " (interactive Claude Code sessions)"
	case config.ChannelNtfy:
		if ch.Topic == "" {
			return " (no topic: loopguard notify setup ntfy)"
		}
		return " (topic " + ch.Topic + " on " + ch.Server + ")"
	case config.ChannelWebhook:
		host := "no url"
		if u, err := url.Parse(ch.URL); err == nil && u.Host != "" {
			host = u.Host
		}
		return " (" + ch.Format + " webhook at " + host + ")"
	}
	return ""
}

func newSetupNtfyCmd() *cobra.Command {
	var server string
	var newTopic bool
	cmd := &cobra.Command{
		Use:   "ntfy [--server URL] [--new-topic]",
		Short: "Send notifications to your phone through ntfy",
		Long: `Turn on ntfy notifications (https://ntfy.sh: free phone push, no account).
ntfy topics are public: anyone who knows a topic's name can read and send
to it. So LoopGuard makes a long random name (128 bits), keeps it in
config.yaml, and you subscribe to it in the ntfy app. An existing topic is
kept unless --new-topic is given. --server sets a self-hosted ntfy server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			n, mode, err := notifyConfig(dir)
			if err != nil {
				return err
			}
			topic := n.Ntfy.Topic
			if topic == "" || newTopic {
				if topic, err = notify.NewTopic(); err != nil {
					return err
				}
			}
			edits := []config.Edit{{Key: "notify.ntfy.enabled", Value: "true"}, {Key: "notify.ntfy.topic", Value: topic}}
			if server != "" {
				edits = append(edits, config.Edit{Key: "notify.ntfy.server", Value: strings.TrimRight(server, "/")})
			} else {
				server = n.Ntfy.Server
			}
			path := filepath.Join(dir, config.GlobalFile)
			if err := config.EditFile(path, edits...); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "ntfy notifications are on (%s).\n\n", path)
			fmt.Fprintf(out, "Your topic: %s\n", topic)
			fmt.Fprintf(out, "Server:     %s\n", server)
			fmt.Fprintln(out, "Keep the topic private: anyone who knows it can read your notifications and send to it.")
			fmt.Fprintln(out, "\nOn your phone:")
			fmt.Fprintln(out, "  1. Install the ntfy app (Android: Google Play or F-Droid; iPhone: App Store).")
			fmt.Fprintf(out, "  2. Tap +, enter the topic %s", topic)
			if server != config.DefaultNtfyServer {
				fmt.Fprintf(out, ", check \"Use another server\" and enter %s", server)
			}
			fmt.Fprintln(out, ", and subscribe.")
			fmt.Fprintf(out, "  Or open %s/%s in a browser.\n", server, topic)
			fmt.Fprintln(out, "\nThen send a test: loopguard notify test")
			if !config.Notifies(mode.Mode) {
				fmt.Fprintf(out, "\nNote: the level is %s, which sends nothing. Notifications start at: loopguard mode notify\n", mode.Mode)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "ntfy server (default "+config.DefaultNtfyServer+")")
	cmd.Flags().BoolVar(&newTopic, "new-topic", false, "make a new topic even if one is set")
	return cmd
}

func newSetupWebhookCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "webhook URL [--format slack|discord|generic]",
		Short: "Send notifications to a Slack, Discord, or generic JSON webhook",
		Long: `Turn on webhook notifications: a Slack or Discord incoming webhook URL, or
any URL that accepts a JSON POST (generic: event, title, body, project,
detector, family, count, duration_seconds, session, stop_command). The URL
is a secret; it is kept in config.yaml and never printed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			u, err := url.Parse(args[0])
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return errors.New("the webhook URL must be an http(s) URL")
			}
			path := filepath.Join(dir, config.GlobalFile)
			if err := config.EditFile(path, config.Edit{Key: "notify.webhook.enabled", Value: "true"},
				config.Edit{Key: "notify.webhook.url", Value: args[0]}, config.Edit{Key: "notify.webhook.format", Value: format}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s webhook notifications to %s are on (%s).\nSend a test: loopguard notify test\n", format, u.Host, path)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", config.FormatSlack, "slack, discord, or generic")
	return cmd
}

func newNotifyTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Send a test notification to every enabled channel",
		Long: `Send a test notification to every enabled channel, whatever the level, and
report each result. The terminal channel can't be tested from here (it goes
out with Claude Code's hook replies); a test sequence is printed if this
command runs in a terminal.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			n, _, err := notifyConfig(dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			test := notify.Notification{Event: notify.EventTest, Project: "test", At: time.Now()}
			senders := notifySenders()
			failed, sent := 0, 0
			for _, name := range []string{config.ChannelDesktop, config.ChannelNtfy, config.ChannelWebhook} {
				ch := n.Channels()[name]
				if !ch.Enabled {
					continue
				}
				sent++
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				err := senders[name].Send(ctx, ch, test)
				cancel()
				if err != nil {
					failed++
					fmt.Fprintf(out, "  %-9s failed: %v\n", name, err)
				} else {
					fmt.Fprintf(out, "  %-9s sent%s\n", name, channelDetail(name, ch))
				}
			}
			if n.Terminal.Enabled {
				if f, ok := out.(*os.File); ok && isTerminal(f) {
					fmt.Fprint(out, "\x1b]777;notify;LoopGuard;"+test.Text()+"\a")
					fmt.Fprintln(out, "  terminal  printed a test sequence (shown as a notification if the terminal supports OSC 777)")
				} else {
					fmt.Fprintln(out, "  terminal  on (can't be tested here: it goes out with Claude Code's hook replies)")
				}
			}
			if sent == 0 && !n.Terminal.Enabled {
				fmt.Fprintln(out, "No channel is enabled; see: loopguard notify")
			}
			if failed > 0 {
				return errSilent
			}
			return nil
		},
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
