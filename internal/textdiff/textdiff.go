// Package textdiff renders a unified line diff of two small texts (a
// settings file before and after `loopguard init`). It uses a plain LCS
// table, which is fine for files of a few thousand lines.
package textdiff

import (
	"fmt"
	"strings"
)

// maxCells bounds the LCS table; larger inputs get a whole-file diff.
const maxCells = 16 << 20

// Unified returns the diff of a and b in unified format with ctx lines of
// context, or "" if they are equal. Line endings are ignored.
func Unified(a, b, nameA, nameB string, ctx int) string {
	if a == b {
		return ""
	}
	x, y := lines(a), lines(b)
	ops := diff(x, y)
	if !changed(ops) {
		return ""
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", nameA, nameB)
	// Group the edits into hunks with ctx lines of context.
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-ctx, 0)
		end := i
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				end = j
			} else if j-end > 2*ctx {
				break
			}
		}
		end = min(end+ctx+1, len(ops))
		ai, bi := ops[start].a, ops[start].b
		var na, nb int
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				na++
			}
			if op.kind != '-' {
				nb++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", rng(ai, na), rng(bi, nb))
		for _, op := range ops[start:end] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

func changed(ops []op) bool {
	for _, o := range ops {
		if o.kind != ' ' {
			return true
		}
	}
	return false
}

func rng(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

func lines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

type op struct {
	kind byte // ' ', '-', '+'
	text string
	a, b int // line index in a and b where this op starts
}

func diff(x, y []string) []op {
	n, m := len(x), len(y)
	if (n+1)*(m+1) > maxCells {
		var ops []op
		for i, l := range x {
			ops = append(ops, op{'-', l, i, 0})
		}
		for j, l := range y {
			ops = append(ops, op{'+', l, n, j})
		}
		return ops
	}
	// lcs[i][j] = length of the LCS of x[i:] and y[j:].
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			ops = append(ops, op{' ', x[i], i, j})
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', x[i], i, j})
			i++
		default:
			ops = append(ops, op{'+', y[j], i, j})
			j++
		}
	}
	return ops
}
