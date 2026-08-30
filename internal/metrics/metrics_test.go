package metrics

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	c := NewCounter("crashcart_test_things_total", "Things, by kind.", "kind")
	c.Inc("a")
	c.Add(2, "b\"q")
	c.Inc("a", "extra") // label mismatch: ignored
	plain := NewCounter("crashcart_test_plain_total", "Plain.")
	NewGauge("crashcart_test_gauge", "A gauge.", func(context.Context) float64 { return 1.5 })
	NewGauge("crashcart_test_nan", "Unavailable.", func(context.Context) float64 { return math.NaN() })
	var b bytes.Buffer
	Write(context.Background(), &b)
	out := b.String()
	for _, want := range []string{
		"# TYPE crashcart_test_things_total counter",
		`crashcart_test_things_total{kind="a"} 1`,
		`crashcart_test_things_total{kind="b\"q"} 2`,
		"crashcart_test_plain_total 0",
		"crashcart_test_gauge 1.5",
		"crashcart_uptime_seconds ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "crashcart_test_nan ") {
		t.Errorf("NaN gauge must be omitted:\n%s", out)
	}
	if c.Value("a") != 1 || plain.Value() != 0 {
		t.Errorf("Value: %d %d", c.Value("a"), plain.Value())
	}
}
