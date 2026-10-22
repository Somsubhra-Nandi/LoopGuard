package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
)

func newBudgetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "budget show | set <scope.key> <value> | clear <scope.key>",
		Aliases: []string{"budgets"},
		Short:   "Show or change the cost, token, time, and tool-call budgets in config.yaml",
		Long: `Budgets are all off by default. A soft budget notifies you (and, at the
assist and guard levels, sends the agent one note); a hard budget stops the
turn at the guard level. Costs are API-equivalent: token usage at API prices.

Keys are <scope>.<soft|hard>_<metric>:
  scope   session (one session, its subagents included), daily (all
          sessions of the local day), repo (one repo's sessions of the day)
  metric  usd (API-equivalent cost), tokens, wall_minutes, tool_calls

  loopguard budget set daily.soft_usd 20
  loopguard budget set session.hard_tool_calls 500
  loopguard budget clear daily.soft_usd

set and clear edit config.yaml in the LoopGuard home, keeping its comments
and other keys; the running daemon picks the change up within a few seconds.
A repo's .loopguard.yaml may lower a budget (or set one config.yaml leaves
off), never raise it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return showBudgets(cmd.OutOrStdout()) },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the budgets in config.yaml",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return showBudgets(cmd.OutOrStdout()) },
	}, &cobra.Command{
		Use:     "set <scope.key> <value>",
		Short:   "Set a budget (e.g. daily.soft_usd 20)",
		Example: "  loopguard budget set daily.soft_usd 20\n  loopguard budget set session.hard_wall_minutes 180",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, metric, level, err := config.ParseBudgetKey(args[0])
			if err != nil {
				return err
			}
			v, err := config.ParseBudgetValue(metric, args[1])
			if err != nil {
				return err
			}
			path, err := globalConfig()
			if err != nil {
				return err
			}
			key := config.BudgetKey(scope, metric, level)
			if err := config.EditFile(path, config.Edit{Key: key, Value: strconv.FormatFloat(v, 'f', -1, 64)}); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Set %s: %s in %s\n", key, budgetValue(metric, v), path)
			return showBudgets(out)
		},
	}, &cobra.Command{
		Use:   "clear <scope.key>",
		Short: "Turn a budget off",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, metric, level, err := config.ParseBudgetKey(args[0])
			if err != nil {
				return err
			}
			path, err := globalConfig()
			if err != nil {
				return err
			}
			key := config.BudgetKey(scope, metric, level)
			out := cmd.OutOrStdout()
			if !fileExists(path) {
				fmt.Fprintf(out, "%s is not set (there is no %s).\n", key, path)
				return nil
			}
			if err := config.EditFile(path, config.Edit{Key: key, Remove: true}); err != nil {
				return err
			}
			fmt.Fprintf(out, "Cleared %s in %s\n", key, path)
			return showBudgets(out)
		},
	})
	return cmd
}

func globalConfig() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, config.GlobalFile), nil
}

// showBudgets lists the budgets config.yaml sets.
func showBudgets(out io.Writer) error {
	dir, err := home.Dir()
	if err != nil {
		return err
	}
	g := config.NewLoader(dir, config.LoaderOptions{}).For("")
	if g.Global.Err != nil {
		return errors.New(g.Global.Path + " is invalid, so no budget applies: " + g.Global.Err.Error())
	}
	b := g.Budgets
	if !b.Any() {
		fmt.Fprintf(out, "No budget set. Example: %s\n", config.ExampleBudget)
		return nil
	}
	fmt.Fprintln(out, "Budgets (costs are API-equivalent; soft: notify and one note to the agent; hard: stop the turn at the guard level)")
	for _, scope := range config.Scopes {
		for _, m := range config.Metrics {
			l := b.Scope(scope).Get(m)
			for _, x := range []struct {
				level string
				v     float64
			}{{"soft", l.Soft}, {"hard", l.Hard}} {
				if x.v > 0 {
					fmt.Fprintf(out, "  %-34s %s\n", config.BudgetKey(scope, m, x.level)[len("budgets."):], budgetValue(m, x.v))
				}
			}
		}
	}
	return nil
}

func budgetValue(metric string, v float64) string {
	if metric == config.MetricCost {
		return cost.FormatUSD(v)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
