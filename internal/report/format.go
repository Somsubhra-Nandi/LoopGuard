package report

import (
	"fmt"
	"time"
)

// CompactCount shows a count as 950, 12.3k, 4.56M.
func CompactCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fG", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// ShortDuration formats d as 45s, 12m, 3h05m, or 2d04h.
func ShortDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

// Offset formats seconds since a session's first call: 0:42, 12:05,
// 1:02:03.
func Offset(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	if d < 0 {
		d = 0
	}
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// RatePct shows a recovery rate as 67%, or - while nothing is decided.
func RatePct(r *float64) string {
	if r == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *r*100)
}
