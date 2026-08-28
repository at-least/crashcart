package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/newlix/crashcart/internal/config"
	"github.com/newlix/crashcart/internal/ingest"
	"github.com/newlix/crashcart/internal/store"
)

// seed writes a week of synthetic events straight through the ingester so a
// fresh local database has something to look at.
func seed(ctx context.Context, cfg config.Config, st *store.Store, log *slog.Logger) error {
	ing := ingest.New(st.Pool(), ingest.Options{SampleRate: 1})
	rnd := rand.New(rand.NewPCG(7, 11))
	now := time.Now().UTC()

	releases := []string{"2.4.0", "2.4.1", "2.5.0"}
	models := []string{"iPhone16,1", "iPhone15,3", "Pixel 8", "SM-S928B"}
	oses := []string{"18.0", "17.5", "14", "15"}
	screens := []string{"CartFragment", "Checkout", "Home", "Profile"}
	errs := []struct{ typ, val, file, fn string }{
		{"NullPointerException", "Attempt to invoke virtual method on a null object reference", "com/example/CartFragment.kt", "onCreateView"},
		{"IOException", "Connection reset by peer", "com/example/net/Api.kt", "fetch"},
		{"IllegalStateException", "Fragment not attached", "com/example/Checkout.kt", "submit"},
		{"OutOfMemoryError", "Failed to allocate 8MB", "com/example/ImageLoader.kt", "decode"},
	}
	var total int
	for day := 6; day >= 0; day-- {
		var items []string
		n := 120 + rnd.IntN(80)
		for i := 0; i < n; i++ {
			ts := now.Add(-time.Duration(day)*24*time.Hour - time.Duration(rnd.IntN(86400))*time.Second)
			if ts.After(now) {
				ts = now
			}
			user := fmt.Sprintf("user-%03d", rnd.IntN(60))
			device := fmt.Sprintf("did-%04x", rnd.IntN(200))
			ev := map[string]any{
				"event_id":    fmt.Sprintf("%032x", rnd.Uint64()),
				"timestamp":   ts.Format(time.RFC3339),
				"platform":    "android",
				"environment": "production",
				"transaction": screens[rnd.IntN(len(screens))],
				"release":     releases[min(rnd.IntN(len(releases)+day), len(releases)-1)],
				"user":        map[string]any{"id": user},
				"tags":        map[string]any{"device_id": device, "build": fmt.Sprint(100 + rnd.IntN(5))},
				"sdk":         map[string]any{"name": "sentry.java.android"},
				"contexts": map[string]any{
					"device": map[string]any{"model": models[rnd.IntN(len(models))]},
					"os":     map[string]any{"version": oses[rnd.IntN(len(oses))]},
				},
				"breadcrumbs": map[string]any{"values": []map[string]any{
					{"timestamp": ts.Add(-30 * time.Second).Format(time.RFC3339), "category": "navigation", "message": "Home → Cart", "level": "info"},
					{"timestamp": ts.Add(-10 * time.Second).Format(time.RFC3339), "category": "http", "message": "GET /api/cart", "level": "info"},
					{"timestamp": ts.Add(-2 * time.Second).Format(time.RFC3339), "category": "ui", "message": "tap checkout", "level": "info"},
				}},
			}
			switch r := rnd.Float64(); {
			case r < 0.06:
				e := errs[rnd.IntN(len(errs))]
				ev["level"] = "fatal"
				ev["exception"] = exception(e.typ, e.val, e.file, e.fn, false, rnd)
			case r < 0.22:
				e := errs[rnd.IntN(len(errs))]
				ev["level"] = "error"
				ev["exception"] = exception(e.typ, e.val, e.file, e.fn, rnd.Float64() < 0.7, rnd)
			case r < 0.35:
				ev["level"] = "warning"
				ev["message"] = "Slow frame: 48ms"
			default:
				ev["level"] = "info"
				ev["message"] = []string{"Screen viewed", "Cart updated", "Sync complete", "Push received"}[rnd.IntN(4)]
			}
			b, _ := json.Marshal(ev)
			items = append(items, `{"type":"event"}`, string(b))
			total++
		}
		for i := 0; i < 20; i++ {
			status := "exited"
			if rnd.Float64() < 0.05 {
				status = "crashed"
			}
			s, _ := json.Marshal(map[string]any{"sid": fmt.Sprintf("%x", rnd.Uint64()), "status": status,
				"release": releases[rnd.IntN(len(releases))], "started": now.Add(-time.Duration(day) * 24 * time.Hour).Format(time.RFC3339)})
			items = append(items, `{"type":"session"}`, string(s))
		}
		body := `{"sent_at":"` + now.Format(time.RFC3339) + `"}` + "\n" + strings.Join(items, "\n") + "\n"
		// Ingest in ≤500-event envelopes.
		for _, chunk := range chunks(items, 400) {
			body = `{"sent_at":"` + now.Format(time.RFC3339) + `"}` + "\n" + strings.Join(chunk, "\n") + "\n"
			if _, err := ing.Ingest(ctx, []byte(body)); err != nil {
				return err
			}
		}
	}
	log.Info("seeded", "events", total)
	return nil
}

func exception(typ, val, file, fn string, handled bool, rnd *rand.Rand) map[string]any {
	return map[string]any{"values": []map[string]any{{
		"type": typ, "value": val,
		"mechanism": map[string]any{"handled": handled, "type": "generic"},
		"stacktrace": map[string]any{"frames": []map[string]any{
			{"filename": "android/os/Looper.java", "function": "loop", "in_app": false, "lineno": 201},
			{"filename": "android/app/ActivityThread.java", "function": "main", "in_app": false, "lineno": 7839},
			{"filename": file, "function": fn, "in_app": true, "lineno": 100 + rnd.IntN(50)},
			{"filename": "kotlin/coroutines/Continuation.kt", "function": "resume", "in_app": false, "lineno": 12},
		}},
	}}}
}

// chunks splits items (header+body pairs) into groups of at most n pairs.
func chunks(items []string, n int) [][]string {
	var out [][]string
	for i := 0; i < len(items); i += 2 * n {
		out = append(out, items[i:min(i+2*n, len(items))])
	}
	return out
}
