// Package seed writes demo data through the real ingest path: a week of
// unhandled, errors and messages for a mobile shop app, three releases with a
// spike of unhandled errors on one day, session aggregates so release health has a
// crash-free rate, and a screenshot attached to some of the crashes.
// Everything is deterministic (seeded rand) except the event ids, which
// are fresh on every run.
package seed

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	mrand "math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/ingest"
	"github.com/at-least/crashcart/internal/sentry"
)

// Days of history written, and the per-Ingest batch size.
const (
	Days      = 7
	batchSize = 200
	spikeDay  = 4 // day index (0 = oldest) with the crash spike
)

// Releases in rollout order; the share of traffic per day per release is in
// releaseMix.
var releases = []string{"2.3.0", "2.4.0", "2.4.1"}

// releaseMix[day] = weights of releases[i] on that day.
var releaseMix = [Days][3]float64{
	{1.0, 0.0, 0.0},
	{1.0, 0.0, 0.0},
	{0.9, 0.1, 0.0},
	{0.4, 0.6, 0.0},
	{0.2, 0.8, 0.0}, // spike day: 2.4.0 fully rolled out
	{0.1, 0.5, 0.4},
	{0.05, 0.25, 0.7},
}

// issueDef is one distinct crash/error signature.
type issueDef struct {
	platform    string
	errorType   string
	value       string
	transaction string
	level       string // fatal | error
	handled     bool
	mechanism   string
	frames      []sentry.Frame
	weight      float64         // relative frequency
	releases    map[string]bool // nil = all; otherwise only these
	spike       bool            // dominates the spike day
	unhandled   bool            // exception.mechanism.handled = false (with level fatal: a crash)
}

func inApp(b bool) *bool { return &b }

var issues = []issueDef{
	{
		platform: "android", errorType: "NullPointerException", transaction: "CartFragment", level: "fatal", unhandled: true,
		value:     "Attempt to invoke virtual method 'int java.util.List.size()' on a null object reference",
		mechanism: "UncaughtExceptionHandler", weight: 5,
		frames: []sentry.Frame{
			{Module: "android.os.Looper", Function: "loop", Filename: "Looper.java", Lineno: 288, InApp: inApp(false)},
			{Module: "android.os.Handler", Function: "dispatchMessage", Filename: "Handler.java", Lineno: 106, InApp: inApp(false)},
			{Module: "com.example.shop.ui.cart.CartFragment", Function: "onViewCreated", Filename: "CartFragment.kt", Lineno: 88, InApp: inApp(true)},
			{Module: "com.example.shop.ui.cart.CartFragment", Function: "renderItems", Filename: "CartFragment.kt", Lineno: 142, InApp: inApp(true)},
		},
	},
	{
		platform: "cocoa", errorType: "SIGABRT", transaction: "CheckoutViewController", level: "fatal", unhandled: true,
		value: "Fatal error: Unexpectedly found nil while unwrapping an Optional value", mechanism: "signalhandler", weight: 4,
		frames: []sentry.Frame{
			{Package: "/usr/lib/system/libdyld.dylib", InstrAddr: "0x1a2b3c4d0", Function: "start", InApp: inApp(false)},
			{Package: "/usr/lib/system/libsystem_kernel.dylib", InstrAddr: "0x1a2b3d000", Function: "__pthread_kill", InApp: inApp(false)},
			{Package: "/private/var/containers/Bundle/Application/Shop.app/Shop", InstrAddr: "0x1044f2a30", Function: "CheckoutViewController.placeOrder(_:)", Filename: "CheckoutViewController.swift", Lineno: 211, InApp: inApp(true)},
			{Package: "/private/var/containers/Bundle/Application/Shop.app/Shop", InstrAddr: "0x1044f2b8c", Function: "CheckoutViewController.total.getter", Filename: "CheckoutViewController.swift", Lineno: 57, InApp: inApp(true)},
		},
	},
	{
		platform: "android", errorType: "SocketTimeoutException", transaction: "ApiClient", level: "error", handled: true,
		value: "timeout", mechanism: "generic", weight: 6,
		frames: []sentry.Frame{
			{Module: "okhttp3.internal.http2.Http2Stream", Function: "waitForIo", Filename: "Http2Stream.kt", Lineno: 672, InApp: inApp(false)},
			{Module: "com.example.shop.net.ApiClient", Function: "get", Filename: "ApiClient.kt", Lineno: 64, InApp: inApp(true)},
			{Module: "com.example.shop.net.ApiClient", Function: "execute", Filename: "ApiClient.kt", Lineno: 91, InApp: inApp(true)},
		},
	},
	{
		platform: "cocoa", errorType: "NSInvalidArgumentException", transaction: "ProductDetailViewController", level: "fatal", unhandled: true,
		value:     "-[__NSCFString objectForKey:]: unrecognized selector sent to instance 0x282b4c1e0",
		mechanism: "NSException", weight: 3,
		frames: []sentry.Frame{
			{Package: "/System/Library/Frameworks/UIKitCore.framework/UIKitCore", InstrAddr: "0x1b41e2c40", Function: "-[UIApplication sendAction:to:from:forEvent:]", InApp: inApp(false)},
			{Package: "/private/var/containers/Bundle/Application/Shop.app/Shop", InstrAddr: "0x1044a1f10", Function: "ProductDetailViewController.addToCart(_:)", Filename: "ProductDetailViewController.swift", Lineno: 133, InApp: inApp(true)},
			{Package: "/private/var/containers/Bundle/Application/Shop.app/Shop", InstrAddr: "0x1044a20a4", Function: "ProductDetailViewController.variant(for:)", Filename: "ProductDetailViewController.swift", Lineno: 178, InApp: inApp(true)},
		},
	},
	{
		platform: "android", errorType: "IllegalStateException", transaction: "PaymentActivity", level: "fatal", unhandled: true,
		value:     "Fragment PaymentSheet not attached to an activity",
		mechanism: "UncaughtExceptionHandler", weight: 1, spike: true, releases: map[string]bool{"2.4.0": true},
		frames: []sentry.Frame{
			{Module: "androidx.fragment.app.Fragment", Function: "requireActivity", Filename: "Fragment.java", Lineno: 964, InApp: inApp(false)},
			{Module: "com.example.shop.ui.payment.PaymentSheet", Function: "onResult", Filename: "PaymentSheet.kt", Lineno: 52, InApp: inApp(true)},
			{Module: "com.example.shop.ui.payment.PaymentActivity", Function: "onActivityResult", Filename: "PaymentActivity.kt", Lineno: 117, InApp: inApp(true)},
		},
	},
	{
		platform: "cocoa", errorType: "DecodingError", transaction: "OrderHistoryViewController", level: "error", handled: true,
		value: "keyNotFound(CodingKeys(stringValue: \"shipped_at\"))", mechanism: "generic", weight: 2.5,
		frames: []sentry.Frame{
			{Package: "/private/var/containers/Bundle/Application/Shop.app/Shop", InstrAddr: "0x10450c2d0", Function: "OrderHistoryViewController.load()", Filename: "OrderHistoryViewController.swift", Lineno: 44, InApp: inApp(true)},
			{Package: "/private/var/containers/Bundle/Application/Shop.app/Shop", InstrAddr: "0x10450c3f8", Function: "OrderDecoder.decode(_:)", Filename: "OrderDecoder.swift", Lineno: 29, InApp: inApp(true)},
		},
	},
	{
		platform: "android", errorType: "OutOfMemoryError", transaction: "ProductGridFragment", level: "fatal", unhandled: true,
		value: "Failed to allocate a 48771088 byte allocation with 16777216 free bytes", mechanism: "UncaughtExceptionHandler", weight: 1.5,
		frames: []sentry.Frame{
			{Module: "android.graphics.BitmapFactory", Function: "decodeStream", Filename: "BitmapFactory.java", Lineno: 862, InApp: inApp(false)},
			{Module: "com.example.shop.image.ImageLoader", Function: "decode", Filename: "ImageLoader.kt", Lineno: 77, InApp: inApp(true)},
			{Module: "com.example.shop.ui.grid.ProductGridFragment", Function: "bind", Filename: "ProductGridFragment.kt", Lineno: 201, InApp: inApp(true)},
		},
	},
	{
		platform: "cocoa", errorType: "NSRangeException", transaction: "SearchViewController", level: "fatal", unhandled: true,
		value: "*** -[__NSArrayM objectAtIndexedSubscript:]: index 12 beyond bounds [0 .. 11]", mechanism: "NSException", weight: 2,
		releases: map[string]bool{"2.3.0": true, "2.4.0": true}, // fixed in 2.4.1
		frames: []sentry.Frame{
			{Package: "/System/Library/Frameworks/UIKitCore.framework/UIKitCore", InstrAddr: "0x1b4200f80", Function: "-[UITableView _createPreparedCellForRowAtIndexPath:]", InApp: inApp(false)},
			{Package: "/private/var/containers/Bundle/Application/Shop.app/Shop", InstrAddr: "0x1044e11c0", Function: "SearchViewController.tableView(_:cellForRowAt:)", Filename: "SearchViewController.swift", Lineno: 96, InApp: inApp(true)},
		},
	},
}

// message-only events (no exception, no issue).
var messages = []struct {
	level, message, transaction string
}{
	{"info", "Checkout started", "CheckoutViewController"},
	{"info", "Order placed", "CheckoutViewController"},
	{"info", "User signed in", "LoginFragment"},
	{"info", "Push token refreshed", "AppDelegate"},
	{"warning", "Slow network response: /api/catalog took 2.8s", "ApiClient"},
	{"warning", "Image cache evicted 120 entries under memory pressure", "ImageLoader"},
	{"warning", "Deprecated payment method selected", "PaymentActivity"},
}

var devices = map[string][]struct{ model, os string }{
	"android": {{"Pixel 8", "14"}, {"SM-S918B", "14"}, {"SM-A546E", "13"}, {"Pixel 6a", "13"}, {"CPH2449", "13"}},
	"cocoa":   {{"iPhone15,2", "17.5.1"}, {"iPhone16,1", "18.0"}, {"iPhone14,5", "17.4"}, {"iPad13,16", "17.5"}},
}

var locales = []string{"en_US", "en_GB", "de_DE", "fr_FR", "ja_JP", "pt_BR"}

// Run creates the demo project (if missing) and a week of events/sessions.
func Run(ctx context.Context, in *ingest.Ingester, slug string) error {
	st := in.Store
	p, err := st.GetProject(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		p, err = st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: slug, Name: displayName(slug), PublicKey: auth.NewProjectKey()})
	}
	if err != nil {
		return fmt.Errorf("seed: project: %w", err)
	}
	for _, typ := range []string{"new_issue", "regression", "unhandled_spike", "escalating"} {
		if _, err := st.UpsertAlertRule(ctx, sqlc.UpsertAlertRuleParams{ProjectID: p.ID, Type: sqlc.AlertType(typ), Enabled: true, CooldownMinutes: 60}); err != nil {
			return fmt.Errorf("seed: alert rule %s: %w", typ, err)
		}
	}

	now := time.Now().UTC()
	// Seeded with the clock: a re-run must produce new event ids (the
	// same ids at new times would dedupe as resends, or land as second
	// rows under the same event_id).
	g := &gen{rng: mrand.New(mrand.NewPCG(0x5eed^uint64(now.UnixNano()), uint64(p.ID))), now: now}
	start := now.Add(-Days * 24 * time.Hour).Truncate(time.Hour)
	for day := 0; day < Days; day++ {
		dayStart := start.Add(time.Duration(day) * 24 * time.Hour)
		items := g.day(day, dayStart)
		for len(items) > 0 {
			// An event with attachments goes in its own envelope: the
			// header's event_id says whose they are.
			n := min(batchSize, len(items))
			if items[0].attachments != nil {
				n = 1
			} else {
				for i := 1; i < n; i++ {
					if items[i].attachments != nil {
						n = i
						break
					}
				}
			}
			env := sentry.Parse(g.envelope(items[:n]), now)
			if _, err := in.Ingest(ctx, p, env, now); err != nil {
				return fmt.Errorf("seed: ingest day %d: %w", day, err)
			}
			items = items[n:]
		}
	}
	return nil
}

// gen holds the deterministic source and the clock.
type gen struct {
	rng *mrand.Rand
	now time.Time
}

// item is one envelope item: "event" or "sessions". An event may carry
// attachments (a screenshot), sent after it in its own envelope.
type item struct {
	typ         string
	body        []byte
	eventID     string
	attachments []attachment
}

type attachment struct {
	filename, contentType string
	body                  []byte
}

// day builds the envelope items for one day: exception events, message
// events and session aggregates (one aggregate item per release).
func (g *gen) day(day int, dayStart time.Time) []item {
	end := dayStart.Add(24 * time.Hour)
	if end.After(g.now) {
		end = g.now
	}
	span := end.Sub(dayStart)
	mix := releaseMix[day]

	nErrors := 220 + g.rng.IntN(40)
	if day == spikeDay {
		nErrors = 520
	}
	nMessages := 55 + g.rng.IntN(15)

	var items []item
	for i := 0; i < nErrors; i++ {
		rel := g.pick(mix)
		def := g.issue(day, rel)
		if def == nil {
			continue
		}
		ts := dayStart.Add(time.Duration(g.rng.Float64() * float64(span)))
		if day == spikeDay && def.spike {
			// The spike is concentrated in a few hours after the rollout.
			ts = dayStart.Add(9*time.Hour + time.Duration(g.rng.Float64()*float64(5*time.Hour)))
			if ts.After(end) {
				ts = end.Add(-time.Minute)
			}
		}
		body, id := g.exceptionEvent(def, rel, ts)
		it := item{typ: "event", body: body}
		// A crash screenshot on some of the unhandled ones (what the mobile
		// SDKs attach with attachScreenshot).
		if !def.handled && g.rng.IntN(6) == 0 {
			it.eventID = id
			it.attachments = []attachment{{"screenshot.png", "image/png", g.screenshot(def)}}
		}
		items = append(items, it)
	}
	for i := 0; i < nMessages; i++ {
		rel := g.pick(mix)
		ts := dayStart.Add(time.Duration(g.rng.Float64() * float64(span)))
		items = append(items, item{typ: "event", body: g.messageEvent(rel, ts)})
	}
	// Sessions: ~6000 per day split by release share, crash-free ≈ 99 %
	// (the spike release drops to ≈ 97 % on the spike day).
	for i, rel := range releases {
		share := mix[i]
		if share == 0 {
			continue
		}
		total := int(float64(5500+g.rng.IntN(1000)) * share)
		crashRate := 0.006 + g.rng.Float64()*0.006
		if day == spikeDay && rel == "2.4.0" {
			crashRate = 0.03
		}
		crashed := int(float64(total) * crashRate)
		errored := int(float64(total) * (0.01 + g.rng.Float64()*0.01))
		items = append(items, item{typ: "sessions", body: g.sessionAggregate(rel, dayStart, total-crashed-errored, crashed, errored)})
	}
	g.rng.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
	return items
}

func (g *gen) pick(mix [3]float64) string {
	r := g.rng.Float64() * (mix[0] + mix[1] + mix[2])
	for i, w := range mix {
		if r < w {
			return releases[i]
		}
		r -= w
	}
	return releases[len(releases)-1]
}

// issue picks a definition by weight for the given release; on the spike
// day the spike issue takes most of the extra volume.
func (g *gen) issue(day int, rel string) *issueDef {
	var total float64
	weights := make([]float64, len(issues))
	for i := range issues {
		d := &issues[i]
		if d.releases != nil && !d.releases[rel] {
			continue
		}
		w := d.weight
		if d.spike && day == spikeDay {
			w = 30
		}
		weights[i] = w
		total += w
	}
	if total == 0 {
		return nil
	}
	r := g.rng.Float64() * total
	for i, w := range weights {
		if w == 0 {
			continue
		}
		if r < w {
			return &issues[i]
		}
		r -= w
	}
	return nil
}

func (g *gen) device(platform string) (model, os string) {
	list := devices[platform]
	d := list[g.rng.IntN(len(list))]
	return d.model, d.os
}

func (g *gen) user() string { return fmt.Sprintf("user-%04d", 1+g.rng.IntN(1500)) }

func (g *gen) deviceID() string { return fmt.Sprintf("did-%08x", g.rng.Uint32()) }

func (g *gen) eventID() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(g.rng.UintN(256))
	}
	return hex.EncodeToString(b[:])
}

func (g *gen) base(platform, rel, transaction, level string, ts time.Time) map[string]any {
	model, osv := g.device(platform)
	osName, sdk := "Android", "sentry.java.android"
	if platform == "cocoa" {
		osName, sdk = "iOS", "sentry.cocoa"
	}
	return map[string]any{
		"event_id":    g.eventID(),
		"timestamp":   ts.Format(time.RFC3339Nano),
		"level":       level,
		"platform":    platform,
		"environment": "production",
		"release":     rel,
		"transaction": transaction,
		"sdk":         map[string]any{"name": sdk, "version": "8.9.0"},
		"user":        map[string]any{"id": g.user()},
		"tags":        map[string]any{"device_id": g.deviceID(), "locale": locales[g.rng.IntN(len(locales))]},
		"contexts": map[string]any{
			"device": map[string]any{"model": model, "family": model},
			"os":     map[string]any{"name": osName, "version": osv},
			"app":    map[string]any{"app_version": rel, "app_build": "1"},
		},
	}
}

func (g *gen) exceptionEvent(d *issueDef, rel string, ts time.Time) (body []byte, eventID string) {
	ev := g.base(d.platform, rel, d.transaction, d.level, ts)
	handled := d.handled
	ev["exception"] = map[string]any{"values": []map[string]any{{
		"type":       d.errorType,
		"value":      d.value,
		"mechanism":  map[string]any{"type": d.mechanism, "handled": handled},
		"stacktrace": map[string]any{"frames": d.frames},
	}}}
	ev["breadcrumbs"] = map[string]any{"values": g.breadcrumbs(d.transaction, ts)}
	b, _ := json.Marshal(ev)
	return b, ev["event_id"].(string)
}

// screenshot draws a phone screen (360×640) of the transaction the crash
// happened in: an app bar in the transaction's colour, a few cards, and
// the error dialog on top — a few KB of PNG.
func (g *gen) screenshot(d *issueDef) []byte {
	const w, h = 360, 640
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fill := func(x0, y0, x1, y1 int, c color.RGBA) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.SetRGBA(x, y, c)
			}
		}
	}
	hs := fnv.New32a()
	hs.Write([]byte(d.transaction))
	hue := hs.Sum32()
	bar := color.RGBA{R: uint8(40 + hue%120), G: uint8(60 + (hue>>8)%100), B: uint8(120 + (hue>>16)%100), A: 255}
	fill(0, 0, w, h, color.RGBA{245, 245, 247, 255})
	fill(0, 0, w, 24, color.RGBA{20, 20, 24, 255}) // status bar
	fill(0, 24, w, 80, bar)                        // app bar
	fill(16, 44, 16+len(d.transaction)*9, 60, color.RGBA{255, 255, 255, 255})
	for i := 0; i < 4; i++ { // list cards
		y := 100 + i*120
		fill(16, y, w-16, y+104, color.RGBA{255, 255, 255, 255})
		fill(28, y+12, 108, y+92, color.RGBA{220, 222, 228, 255})
		fill(120, y+16, w-40, y+30, color.RGBA{60, 60, 70, 255})
		fill(120, y+40, w-90, y+50, color.RGBA{160, 160, 170, 255})
	}
	fill(0, h-64, w, h, color.RGBA{255, 255, 255, 255}) // nav bar
	for y := 0; y < h; y++ {                            // scrim
		for x := 0; x < w; x++ {
			c := img.RGBAAt(x, y)
			img.SetRGBA(x, y, color.RGBA{c.R / 2, c.G / 2, c.B / 2, 255})
		}
	}
	fill(32, 250, w-32, 390, color.RGBA{255, 255, 255, 255}) // dialog
	fill(48, 268, 200, 284, color.RGBA{30, 30, 36, 255})     // title
	fill(48, 300, w-48, 308, color.RGBA{120, 120, 130, 255}) // text
	fill(48, 318, w-100, 326, color.RGBA{120, 120, 130, 255})
	fill(w-140, 350, w-48, 376, color.RGBA{uint8(200 + hue%50), 60, 60, 255}) // button
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func (g *gen) messageEvent(rel string, ts time.Time) []byte {
	m := messages[g.rng.IntN(len(messages))]
	platform := "android"
	if g.rng.IntN(2) == 0 {
		platform = "cocoa"
	}
	ev := g.base(platform, rel, m.transaction, m.level, ts)
	ev["logentry"] = map[string]any{"formatted": m.message}
	b, _ := json.Marshal(ev)
	return b
}

func (g *gen) breadcrumbs(transaction string, ts time.Time) []map[string]any {
	crumbs := []map[string]any{
		{"timestamp": ts.Add(-42 * time.Second).Format(time.RFC3339), "category": "navigation", "level": "info", "data": map[string]any{"from": "/home", "to": "/" + strings.ToLower(transaction)}},
		{"timestamp": ts.Add(-17 * time.Second).Format(time.RFC3339), "category": "http", "type": "http", "level": "info", "data": map[string]any{"method": "GET", "url": "https://api.example.com/v2/cart", "status_code": 200}},
		{"timestamp": ts.Add(-3 * time.Second).Format(time.RFC3339), "category": "ui.click", "level": "info", "message": "button[data-id=" + strings.ToLower(transaction) + "-primary]"},
	}
	return crumbs
}

func (g *gen) sessionAggregate(rel string, dayStart time.Time, exited, crashed, errored int) []byte {
	// Spread the day's sessions over four 6-hour buckets.
	var aggs []map[string]any
	for i := 0; i < 4; i++ {
		started := dayStart.Add(time.Duration(i) * 6 * time.Hour)
		if started.After(g.now) {
			break
		}
		aggs = append(aggs, map[string]any{
			"started": started.Format(time.RFC3339),
			"exited":  exited / 4, "crashed": crashed / 4, "errored": errored / 4,
		})
	}
	b, _ := json.Marshal(map[string]any{
		"aggregates": aggs,
		"attrs":      map[string]any{"release": rel, "environment": "production"},
	})
	return b
}

// envelope frames items as a Sentry envelope (explicit item lengths). A
// lone event with attachments is framed as the SDKs do: the header names
// the event, the attachment items follow it.
func (g *gen) envelope(items []item) []byte {
	var sb strings.Builder
	if len(items) == 1 && items[0].eventID != "" {
		fmt.Fprintf(&sb, `{"event_id":%q,"sent_at":%q}`+"\n", items[0].eventID, g.now.Format(time.RFC3339))
	} else {
		sb.WriteString(`{"sent_at":"` + g.now.Format(time.RFC3339) + `"}` + "\n")
	}
	for _, it := range items {
		fmt.Fprintf(&sb, `{"type":%q,"length":%d}`+"\n", it.typ, len(it.body))
		sb.Write(it.body)
		sb.WriteByte('\n')
		for _, a := range it.attachments {
			fmt.Fprintf(&sb, `{"type":"attachment","length":%d,"filename":%q,"content_type":%q,"attachment_type":"event.attachment"}`+"\n", len(a.body), a.filename, a.contentType)
			sb.Write(a.body)
			sb.WriteByte('\n')
		}
	}
	return []byte(sb.String())
}

func displayName(slug string) string {
	if slug == "demo" {
		return "Demo Shop"
	}
	return slug
}
