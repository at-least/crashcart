// Package metrics is the process's counters and gauges, served at
// GET /metrics in the Prometheus text format (no client library: the
// format is a few lines per metric). Counters are process-local and
// monotonic since start; gauges are read at scrape time, some from the
// database (queue depth, dirty hours), so they are the same on every
// replica.
//
// Naming: crashcart_<subsystem>_<what>_total for counters, plain for
// gauges; labels are low-cardinality only (outcome, kind, type — never a
// project or an id).
package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Counter is a monotonic count, optionally split by label values.
type Counter struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	series     map[string]*atomic.Int64 // key: joined label values
}

// Gauge is a value read at scrape time.
type Gauge struct {
	name, help string
	read       func(ctx context.Context) float64
}

var (
	mu       sync.Mutex
	counters []*Counter
	gauges   []*Gauge
	started  = time.Now()
)

// NewCounter registers a counter. labels are the label names each
// observation carries, in order (none for a plain counter).
func NewCounter(name, help string, labels ...string) *Counter {
	c := &Counter{name: name, help: help, labels: labels, series: map[string]*atomic.Int64{}}
	mu.Lock()
	counters = append(counters, c)
	mu.Unlock()
	return c
}

// NewGauge registers a gauge computed by read at every scrape. A read
// that fails should return NaN; the scrape then omits the sample.
func NewGauge(name, help string, read func(ctx context.Context) float64) *Gauge {
	g := &Gauge{name: name, help: help, read: read}
	mu.Lock()
	gauges = append(gauges, g)
	mu.Unlock()
	return g
}

// Add counts n for the given label values (as many as the counter has
// labels; a mismatch is a programming error and is ignored).
func (c *Counter) Add(n int64, values ...string) {
	if len(values) != len(c.labels) {
		return
	}
	key := strings.Join(values, "\x00")
	c.mu.Lock()
	s, ok := c.series[key]
	if !ok {
		s = &atomic.Int64{}
		c.series[key] = s
	}
	c.mu.Unlock()
	s.Add(n)
}

// Inc counts one.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Value returns the count for the label values (tests, health).
func (c *Counter) Value(values ...string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.series[strings.Join(values, "\x00")]; ok {
		return s.Load()
	}
	return 0
}

// Write renders every metric in the Prometheus text format.
func Write(ctx context.Context, w io.Writer) {
	mu.Lock()
	cs := append([]*Counter(nil), counters...)
	gs := append([]*Gauge(nil), gauges...)
	mu.Unlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].name < cs[j].name })
	sort.Slice(gs, func(i, j int) bool { return gs[i].name < gs[j].name })
	fmt.Fprintf(w, "# HELP crashcart_uptime_seconds Seconds since the process started.\n# TYPE crashcart_uptime_seconds gauge\ncrashcart_uptime_seconds %d\n", int64(time.Since(started).Seconds()))
	for _, c := range cs {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
		c.mu.Lock()
		keys := make([]string, 0, len(c.series))
		for k := range c.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 && len(c.labels) == 0 {
			fmt.Fprintf(w, "%s 0\n", c.name)
		}
		for _, k := range keys {
			v := c.series[k].Load()
			if len(c.labels) == 0 {
				fmt.Fprintf(w, "%s %d\n", c.name, v)
				continue
			}
			vals := strings.Split(k, "\x00")
			parts := make([]string, len(c.labels))
			for i, l := range c.labels {
				parts[i] = l + `="` + escape(vals[i]) + `"`
			}
			fmt.Fprintf(w, "%s{%s} %d\n", c.name, strings.Join(parts, ","), v)
		}
		c.mu.Unlock()
	}
	for _, g := range gs {
		v := g.read(ctx)
		if v != v { // NaN: unavailable
			continue
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", g.name, g.help, g.name, g.name, v)
	}
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// Handler serves GET /metrics.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		Write(ctx, w)
	})
}
