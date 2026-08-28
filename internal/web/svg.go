package web

import (
	"fmt"
	"html"
	"strings"
)

// Server-side charts: small inline SVGs with a <title> per bar so the
// browser shows a native tooltip on hover. Colors are CSS custom properties
// (severity ramp / series palette) so both themes paint them correctly.

// Series is one stacked component of a bar chart.
type Series struct {
	Name  string
	Token string // color token without the leading "--", e.g. "level-fatal" or "series-1"
}

// Color is the CSS value of the series token.
func (s Series) Color() string { return "var(--" + s.Token + ")" }

// Bucket is one bar: a label and one value per series.
type Bucket struct {
	Label  string
	Values []int64
}

// HealthPoint is one crash-free bar.
type HealthPoint struct {
	Label          string
	Total, Crashed int64
}

const (
	chartW = 600.0
	chartH = 140.0
)

// sparkline draws values as a filled area, 120×28.
func sparkline(values []int64) string {
	const w, h = 120.0, 28.0
	n := len(values)
	if n == 0 {
		return `<svg class="spark" viewBox="0 0 120 28" width="120" height="28" aria-hidden="true"></svg>`
	}
	var maxV int64 = 1
	var total int64
	for _, v := range values {
		total += v
		if v > maxV {
			maxV = v
		}
	}
	step := w / float64(max(n-1, 1))
	var pts strings.Builder
	for i, v := range values {
		x := float64(i) * step
		if n == 1 {
			x = w / 2
		}
		y := h - 2 - float64(v)/float64(maxV)*(h-4)
		fmt.Fprintf(&pts, "%s%.1f,%.1f", sep(i), x, y)
	}
	line := pts.String()
	area := fmt.Sprintf("0,%.0f %s %.0f,%.0f", h, line, w, h)
	return fmt.Sprintf(`<svg class="spark" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" role="img" aria-label="%s events over 7 days"><title>%s events in the last 7 days</title><polygon points="%s" fill="var(--spark-fill)"/><polyline points="%s" fill="none" stroke="var(--spark-line)" stroke-width="1.25" stroke-linejoin="round"/></svg>`,
		w, h, w, h, compact(total), compact(total), area, line)
}

func sep(i int) string {
	if i == 0 {
		return ""
	}
	return " "
}

// bars draws a stacked bar chart; each bar carries a <title> with its
// label and per-series values.
func bars(buckets []Bucket, series []Series) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="Timeline">`, chartW, chartH)
	var maxV int64 = 1
	for _, bk := range buckets {
		var sum int64
		for _, v := range bk.Values {
			sum += v
		}
		if sum > maxV {
			maxV = sum
		}
	}
	// grid: quarter lines
	for i := 1; i <= 3; i++ {
		y := chartH - float64(i)/4*chartH
		fmt.Fprintf(&b, `<line x1="0" x2="%.0f" y1="%.1f" y2="%.1f" stroke="var(--chart-grid)" stroke-width="1" vector-effect="non-scaling-stroke"/>`, chartW, y, y)
	}
	n := len(buckets)
	if n == 0 {
		b.WriteString(`</svg>`)
		return b.String()
	}
	slot := chartW / float64(n)
	gap := slot * 0.18
	if gap > 6 {
		gap = 6
	}
	bw := slot - gap
	for i, bk := range buckets {
		x := float64(i)*slot + gap/2
		var title strings.Builder
		title.WriteString(html.EscapeString(bk.Label))
		var sum int64
		for _, v := range bk.Values {
			sum += v
		}
		if len(series) > 1 {
			fmt.Fprintf(&title, " — %d total", sum)
		}
		fmt.Fprintf(&b, `<g class="bar"><title>%s`, title.String())
		for j, s := range series {
			if j < len(bk.Values) {
				fmt.Fprintf(&b, "\n%s: %d", html.EscapeString(s.Name), bk.Values[j])
			}
		}
		b.WriteString(`</title>`)
		// hit area so empty buckets still show a title
		fmt.Fprintf(&b, `<rect x="%.2f" y="0" width="%.2f" height="%.0f" fill="transparent"/>`, x, bw, chartH)
		y := chartH
		for j, s := range series {
			if j >= len(bk.Values) || bk.Values[j] <= 0 {
				continue
			}
			h := float64(bk.Values[j]) / float64(maxV) * (chartH - 4)
			y -= h
			fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s"/>`, x, y, bw, h, s.Color())
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// crashFreeBars draws one bar per point at the crash-free rate (0–100%),
// colored by health, with a title carrying the sessions and the rate.
func crashFreeBars(points []HealthPoint) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="Crash-free sessions">`, chartW, chartH)
	for _, p := range []float64{0.25, 0.5, 0.75} {
		y := chartH - p*chartH
		fmt.Fprintf(&b, `<line x1="0" x2="%.0f" y1="%.1f" y2="%.1f" stroke="var(--chart-grid)" stroke-width="1" vector-effect="non-scaling-stroke"/>`, chartW, y, y)
	}
	n := len(points)
	if n == 0 {
		b.WriteString(`</svg>`)
		return b.String()
	}
	slot := chartW / float64(n)
	gap := min(slot*0.18, 6)
	bw := slot - gap
	for i, p := range points {
		x := float64(i)*slot + gap/2
		fmt.Fprintf(&b, `<g class="bar"><title>%s`, html.EscapeString(p.Label))
		if p.Total <= 0 {
			fmt.Fprintf(&b, "\nno sessions</title><rect x=\"%.2f\" y=\"0\" width=\"%.2f\" height=\"%.0f\" fill=\"transparent\"/></g>", x, bw, chartH)
			continue
		}
		rate := float64(p.Total-p.Crashed) / float64(p.Total)
		fmt.Fprintf(&b, "\ncrash-free: %s\nsessions: %d\ncrashed: %d</title>", crashFree(p.Total, p.Crashed), p.Total, p.Crashed)
		color := "var(--ok)"
		switch {
		case rate < 0.95:
			color = "var(--level-fatal)"
		case rate < 0.99:
			color = "var(--level-warning)"
		}
		h := max(rate*(chartH-2), 2)
		fmt.Fprintf(&b, `<rect x="%.2f" y="0" width="%.2f" height="%.0f" fill="transparent"/>`, x, bw, chartH)
		fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s"/></g>`, x, chartH-h, bw, h, color)
	}
	b.WriteString(`</svg>`)
	return b.String()
}
