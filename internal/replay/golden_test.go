package replay

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
)

var update = flag.Bool("update", false, "rewrite testdata/transcripts/golden.json")

// golden is what the golden file pins: the report (minus run-specific
// fields) plus per-stream call counts.
type golden struct {
	Report  *Report        `json:"report"`
	Streams []goldenStream `json:"streams"`
}

type goldenStream struct {
	Stream    string `json:"stream"`
	ToolCalls int    `json:"tool_calls"`
	Failed    int    `json:"failed_calls"`
}

// TestGolden replays the anonymized real sessions in testdata/transcripts
// (see testdata/transcripts/README.md) and compares the result with
// golden.json. Run `go test ./internal/replay -run TestGolden -update`
// after an intended change and review the diff.
func TestGolden(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "transcripts")
	streams, err := Discover(root, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) == 0 {
		t.Fatal("no transcripts under testdata/transcripts")
	}
	res := Run(streams, Options{Thresholds: config.Default(), Timeline: true})
	g := golden{Report: NewReport(res, "testdata/transcripts", time.Time{}, ReportOptions{Top: -1}, time.Unix(0, 0), 0)}
	for key, calls := range res.Timelines {
		gs := goldenStream{Stream: key, ToolCalls: len(calls)}
		for _, c := range calls {
			if c.Failed {
				gs.Failed++
			}
		}
		g.Streams = append(g.Streams, gs)
	}
	sort.Slice(g.Streams, func(i, j int) bool { return g.Streams[i].Stream < g.Streams[j].Stream })

	var got bytes.Buffer
	enc := json.NewEncoder(&got)
	enc.SetIndent("", "  ")
	if err := enc.Encode(g); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "golden.json")
	if *update {
		if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got.Bytes()) {
		t.Errorf("replay of testdata/transcripts differs from golden.json; if intended, rerun with -update and review the diff.\ngot:\n%s", got.String())
	}
}
