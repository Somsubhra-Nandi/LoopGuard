package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
)

// openBrowser opens a URL in the default browser; tests replace it.
var openBrowser = func(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 takes the URL as one argument: no shell parses it.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func newDashboardCmd() *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the local dashboard in your browser",
		Long: `Open LoopGuard's dashboard: usage, API-equivalent cost, loops, sessions
with their timelines, the top costs, and the settings in effect. It is
served by the daemon on 127.0.0.1 only (the daemon is started if needed).

This prints a one-time login link and opens it in your default browser.
The link works once, for 2 minutes; it signs that browser in for 12 hours
(until the daemon restarts). Run the command again for a new link.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			info, h, up := running(dir)
			if !up {
				if err := startDetached(out, dir); err != nil {
					return err
				}
				if info, h, up = running(dir); !up {
					return errors.New("the daemon did not come up")
				}
			}
			var link daemon.LoginLink
			code, err := apiRequest(dir, info.Port, http.MethodPost, "/v1/dashboard/login", nil, &link)
			switch {
			case err != nil:
				return err
			case code == http.StatusNotFound:
				return fmt.Errorf("the running daemon (version %s) has no dashboard; restart it with: loopguard daemon stop, then loopguard dashboard", h.Version)
			case code != http.StatusOK || link.URL == "":
				return fmt.Errorf("the daemon answered %d %s", code, http.StatusText(code))
			}
			printLink(out, link.URL)
			if !noOpen {
				if err := openBrowser(link.URL); err != nil {
					fmt.Fprintf(out, "Couldn't open a browser (%v); open the link yourself.\n", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the link without opening a browser")
	return cmd
}

func printLink(out io.Writer, url string) {
	fmt.Fprintf(out, "Dashboard: %s\n", url)
	fmt.Fprintln(out, "The link works once, for 2 minutes. Run loopguard dashboard again for a new one.")
}
