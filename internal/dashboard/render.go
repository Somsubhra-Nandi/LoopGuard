package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/report"
)

// pages holds one template set per page (the layout plus the page).
type pages struct {
	byName map[string]*template.Template
}

func mustParsePages() *pages {
	layout := template.Must(template.New("layout.tmpl").Funcs(funcs).ParseFS(assets, "assets/templates/layout.tmpl"))
	names, err := fs.Glob(assets, "assets/templates/*.tmpl")
	if err != nil {
		panic(err)
	}
	p := &pages{byName: map[string]*template.Template{}}
	for _, n := range names {
		base := path.Base(n)
		if base == "layout.tmpl" {
			continue
		}
		t := template.Must(template.Must(layout.Clone()).ParseFS(assets, n))
		p.byName[strings.TrimSuffix(base, ".tmpl")] = t
	}
	return p
}

// view is what the layout needs, plus the page's own data.
type view struct {
	Title   string
	Nav     string // overview | sessions | costs | settings
	CSRF    string // "": signed out (no nav)
	Version string
	Now     time.Time
	Notices []string
	Data    any
}

func (d *Dashboard) render(w http.ResponseWriter, status int, page string, v view) {
	t := d.pages.byName[page]
	v.Version, v.Now = d.o.Version, d.now()
	var b bytes.Buffer
	if t == nil {
		d.o.Log.Error("dashboard: no template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(&b, "layout.tmpl", v); err != nil {
		d.o.Log.Error("dashboard: rendering", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

// errorPage shows a message (signed-out pages have no navigation).
func (d *Dashboard) errorPage(w http.ResponseWriter, _ *http.Request, status int, title, msg string) {
	d.render(w, status, "message", view{Title: title, Data: message{Title: title, Text: msg}})
}

type message struct {
	Title, Text string
	Back        string // a link back ("": none)
	OK          bool
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

var funcs = template.FuncMap{
	"usd":   cost.FormatUSD,
	"count": report.CompactCount,
	// tokensf rounds an average token count as the CLI's tables do.
	"tokensf": func(f float64) string { return report.CompactCount(int64(f + 0.5)) },
	"num":     func(n any) string { return fmt.Sprint(n) },
	"dur": func(sec float64) string {
		return report.ShortDuration(time.Duration(sec * float64(time.Second)))
	},
	"since":  func(d time.Duration) string { return report.ShortDuration(d) },
	"offset": report.Offset,
	"rate":   report.RatePct,
	"share":  func(f float64) string { return fmt.Sprintf("%.1f%%", 100*f) },
	"short":  shortID,
	"base":   baseName,
	"clip":   report.Clip,
	"title":  func(s string) string { return strings.ToUpper(s[:1]) + s[1:] },
	"words":  func(s string) string { return strings.ReplaceAll(s, "_", " ") },
	"join":   strings.Join,
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// baseName is the last element of a path with either separator (a cwd may
// come from another OS).
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
