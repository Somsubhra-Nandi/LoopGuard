package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
)

func labelsPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return replay.DefaultLabelsPath()
}

// runLabel asks for a verdict on each detected loop that has none yet.
func runLabel(cmd *cobra.Command, res *replay.Result, labelsFlag string) error {
	path, err := labelsPath(labelsFlag)
	if err != nil {
		return err
	}
	labels, err := replay.ReadLabels(path)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	in := bufio.NewReader(cmd.InOrStdin())
	var todo []*replay.Loop
	for _, l := range res.Loops {
		if _, ok := replay.LabelFor(labels, l); !ok {
			todo = append(todo, l)
		}
	}
	fmt.Fprintf(out, "%d loops detected, %d already labeled, %d to label. Labels go to %s\n",
		len(res.Loops), len(res.Loops)-len(todo), len(todo), path)
	saved := 0
	for i, l := range todo {
		fmt.Fprintf(out, "\n[%d/%d] %s · %s · %d× over %ds · %s\n", i+1, len(todo), l.Detector, loopSubject(l),
			l.Count, int(l.DurationSeconds), replay.RepoName(l.Repo))
		fmt.Fprintf(out, "  session %s, calls %d-%d, first signal at call %d\n", l.Stream(), l.FirstCall, l.LastCall, l.FirstSignalCall)
		for _, e := range l.ErrorLines {
			fmt.Fprintf(out, "  │ %s\n", e)
		}
		for _, w := range l.WouldHave {
			fmt.Fprintf(out, "  would %s at call %d: %s\n", w.Action, w.Call, w.Reason)
		}
		verdict, quit, err := ask(out, in)
		if err != nil {
			return err
		}
		if quit {
			break
		}
		if verdict == "" {
			continue
		}
		if err := replay.AppendLabel(path, replay.DetectedLabel(l, verdict, now())); err != nil {
			return err
		}
		saved++
	}
	fmt.Fprintf(out, "\nSaved %d labels.\n", saved)
	return nil
}

// ask reads y/n/s/q; EOF quits.
func ask(out io.Writer, in *bufio.Reader) (verdict string, quit bool, err error) {
	for {
		fmt.Fprint(out, "  [y] real loop / [n] false positive / [s] skip / [q] quit: ")
		line, err := in.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return replay.VerdictReal, false, nil
		case "n", "no":
			return replay.VerdictFalsePositive, false, nil
		case "s", "skip":
			return "", false, nil
		case "q", "quit":
			return "", true, nil
		}
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(out)
			return "", true, nil
		}
	}
}

func loopSubject(l *replay.Loop) string {
	switch {
	case l.File != "":
		return l.File
	case l.Family != "":
		return "`" + l.Family + "`"
	}
	return l.LoopKey
}

func newLabelCmd() *cobra.Command {
	var labelsFlag string
	cmd := &cobra.Command{
		Use:   "label",
		Short: "Record labels for the detector evaluation",
		Args:  cobra.NoArgs,
	}
	missed := &cobra.Command{
		Use:   "missed <session-id[/agent-id]> <from-call> <to-call> [note]",
		Short: "Record a loop the detectors missed",
		Long: `Record a loop the detectors missed, by session and call range. Call numbers
are the ones shown by 'loopguard replay --session <id> --calls'.`,
		Args: cobra.RangeArgs(3, 4),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, err1 := strconv.Atoi(args[1])
			to, err2 := strconv.Atoi(args[2])
			if err1 != nil || err2 != nil || from < 1 || to < from {
				return fmt.Errorf("invalid call range %s-%s", args[1], args[2])
			}
			session, agent := replay.ParseStream(args[0])
			l := replay.Label{Kind: "missed", SessionID: session, AgentID: agent, FromCall: from, ToCall: to,
				Verdict: replay.VerdictMissed, LabeledAt: now().UTC()}
			if len(args) == 4 {
				l.Note = args[3]
			}
			path, err := labelsPath(labelsFlag)
			if err != nil {
				return err
			}
			if err := replay.AppendLabel(path, l); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Recorded missed loop %s calls %d-%d in %s\n", args[0], from, to, path)
			return err
		},
	}
	missed.Flags().StringVar(&labelsFlag, "labels", "", "labels file (default ~/.loopguard/labels.jsonl)")
	cmd.AddCommand(missed)
	return cmd
}

func newEvalCmd() *cobra.Command {
	var path, labelsFlag string
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Precision, recall, and detection delay per detector on labeled transcripts",
		Long: `Replay every transcript under --path and compare the detected loops with the
labels from 'loopguard replay --label' and 'loopguard label missed'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return errors.New("--path is required (the transcripts directory the labels refer to)")
			}
			lp, err := labelsPath(labelsFlag)
			if err != nil {
				return err
			}
			labels, err := replay.ReadLabels(lp)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(labels) == 0 {
				_, err := fmt.Fprintf(out, "eval: no labels in %s; run 'loopguard replay --label' first\n", lp)
				return err
			}
			streams, err := replay.Discover(path, replay.Filter{})
			if err != nil {
				return err
			}
			res := replay.Run(streams, replay.Options{Thresholds: config.Default()})
			fmt.Fprintf(out, "%d streams, %d loops detected, %d labels\n\n", res.Streams, len(res.Loops), len(labels))
			return replay.Evaluate(res.Loops, labels).WriteText(out)
		},
	}
	cmd.Flags().StringVar(&path, "path", os.Getenv("LOOPGUARD_EVAL_DIR"), "transcripts directory (default $LOOPGUARD_EVAL_DIR)")
	cmd.Flags().StringVar(&labelsFlag, "labels", "", "labels file (default ~/.loopguard/labels.jsonl)")
	return cmd
}

// writeCalls prints the call timeline of each replayed stream.
func writeCalls(out io.Writer, res *replay.Result) error {
	if len(res.Timelines) == 0 {
		_, err := fmt.Fprintln(out, "no matching session")
		return err
	}
	keys := make([]string, 0, len(res.Timelines))
	for k := range res.Timelines {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(out, "%s\n", k)
		for _, c := range res.Timelines[k] {
			status := "ok  "
			if c.Failed {
				status = "FAIL"
			}
			what := c.Command
			if what == "" {
				what = c.File
			}
			fmt.Fprintf(out, "%5d  %s  %s  %-6s %s\n", c.N, c.At.Local().Format("01-02 15:04:05"), status, shortTool(c.Tool), what)
		}
	}
	return nil
}

func shortTool(t string) string {
	t = strings.TrimPrefix(t, "other:")
	if len(t) > 24 {
		t = t[:24]
	}
	return t
}

func runExport(cmd *cobra.Command, path, session, outPath string) error {
	streams, err := replay.Discover(path, replay.Filter{Session: session})
	if err != nil {
		return err
	}
	var main []replay.Stream
	for _, s := range streams {
		if s.AgentID == "" {
			main = append(main, s)
		}
	}
	switch len(main) {
	case 0:
		return fmt.Errorf("no session matching %q under %s", session, path)
	case 1:
	default:
		return fmt.Errorf("%d sessions match %q; use a longer id", len(main), session)
	}
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	st, err := replay.Export(main[0], f, config.Default())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s: %d lines, %d tool calls, %d prompts, %d files renamed\n",
		outPath, st.Lines, st.ToolCalls, st.Prompts, st.Files)
	return err
}
