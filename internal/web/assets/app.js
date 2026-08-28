/* CrashCart viewer (Go build) — the only client JS besides htmx, Chart.js and
   the shadless runtimes. Theme toggle, detail sheet (shadless sheet slots
   mounted via RadixKernel), Chart.js rendering + hover tooltip. */
(() => {
  /* ── Theme ─────────────────────────────────────────────── */
  document.addEventListener("click", (e) => {
    var t = e.target.closest("#theme-toggle");
    if (!t) return;
    var el = document.documentElement;
    var next = el.classList.contains("dark") ? "light" : "dark";
    el.classList.toggle("dark", next === "dark");
    try {
      localStorage.setItem("cc-theme", next);
    } catch (err) {}
  });

  /* ── Timezone cookie: server uses it for hourly bucket alignment.
     Set before first fragment request; if it changed, reload once so
     the initial render picks up the right tz. ─────────────── */
  (() => {
    var tz = String(-new Date().getTimezoneOffset() / 60);
    var re = /(?:^|;\s*)cc-tz=([^;]*)/;
    try {
      var m = re.exec(document.cookie);
      if (!m || decodeURIComponent(m[1]) !== tz) {
        document.cookie = "cc-tz=" + encodeURIComponent(tz) + ";path=/;max-age=31536000;samesite=lax";
        // Reload only if the cookie actually stuck (cookies may be blocked —
        // an unconditional reload would loop forever) and at most once.
        if (!m && re.exec(document.cookie) && !sessionStorage.getItem("cc-tz-reloaded")) {
          sessionStorage.setItem("cc-tz-reloaded", "1");
          location.reload();
        }
      }
    } catch (err) {}
  })();

  /* ── Portal date picker (plain GET form, no htmx): submit on change so a
     mouse pick in the calendar popup navigates without needing Enter. */
  document.addEventListener("change", (e) => {
    var input = e.target;
    if (!(input instanceof HTMLInputElement) || input.name !== "anchor" || input.hasAttribute("hx-get")) return;
    if (input.form && input.form.method === "get") input.form.requestSubmit();
  });

  /* ── htmx: mark poll-triggered requests (no triggeringEvent) so the
     server skips HX-Push-Url and we don't spam history every 30s.
     Poll-ness is tagged on the xhr (beforeRequest's detail carries both
     xhr and requestConfig) so the later afterSwap can pair up with THIS
     request even if other shell requests interleave. ── */
  var lastUserNavAt = 0;
  document.body.addEventListener("htmx:configRequest", (e) => {
    if (!e.detail.triggeringEvent) e.detail.headers["X-Poll"] = "1";
  });
  document.body.addEventListener("htmx:beforeRequest", (e) => {
    var t = e.detail.target;
    if (!t) return;
    if (t.id === "shell" && e.detail.xhr) {
      var isPoll = !(e.detail.requestConfig && e.detail.requestConfig.triggeringEvent);
      e.detail.xhr._ccPoll = isPoll;
      if (!isPoll) lastUserNavAt = Date.now();
    } else if (t.id === "sheet-body") {
      sheetTrigger = e.detail.elt;
      // stash on the xhr (not the element — a shell swap may replace the
      // row before the detail response lands) so afterSwap can read it
      if (e.detail.xhr) e.detail.xhr._ccDetailStart = Date.now();
    }
  });

  /* ── Detail sheet (shadless sheet + RadixKernel) ──────────
     #sheet-portal (in the Layout, outside #shell so 30s polls don't
     close an open sheet) carries the sheet slots; htmx swaps the
     detail fragment into #sheet-body, then we mount the portal with
     the kernel — escape / backdrop click / focus trap / scroll lock
     are all handled by RadixKernel.wireDialog. */
  var sheetHandles = null;
  var sheetTrigger = null;

  /* A detail response must NOT open the sheet when it is stale: the user
     navigated while the request was in flight (detail started BEFORE the
     last user navigation). Clicks on filter buttons inside rows never reach
     the row's hx-get — its hx-trigger filters them out server-side. */
  function openSheet(reqXhr) {
    if (sheetHandles) return; // already open — content was just re-swapped
    var started = reqXhr && reqXhr._ccDetailStart;
    if (started && lastUserNavAt && started < lastUserNavAt) return; // stale
    var portal = document.getElementById("sheet-portal");
    var overlay = portal && portal.querySelector('[data-slot="sheet-overlay"]');
    var content = document.getElementById("sheet");
    if (!window.RadixKernel || !portal || !overlay || !content) return;
    portal.hidden = false;
    overlay.style.pointerEvents = "auto";
    content.style.pointerEvents = "auto";
    overlay.setAttribute("data-state", "open");
    content.setAttribute("data-state", "open");
    sheetHandles = window.RadixKernel.wireDialog({
      content: content,
      portal: portal,
      trigger: sheetTrigger,
      scrollLock: "overflow",
      onClosed: function () {
        sheetHandles = null;
        sheetTrigger = null;
        overlay.setAttribute("data-state", "closed");
        content.setAttribute("data-state", "closed");
        // wireDialog removes the portal on close; re-attach it hidden so
        // the next row click has a #sheet-body to swap into.
        setTimeout(function () {
          if (sheetHandles) return; // reopened in the meantime
          portal.hidden = true;
          document.body.appendChild(portal);
          document.getElementById("sheet-body").innerHTML =
            '<p class="animate-pulse p-4 text-xs text-muted-foreground">loading…</p>';
        }, 0);
      },
    });
  }

  function closeSheet() {
    if (sheetHandles) sheetHandles.close(true);
  }

  document.body.addEventListener("htmx:afterSwap", (e) => {
    var t = e.detail.target;
    if (t && t.id === "sheet-body") {
      openSheet(e.detail.xhr);
    } else if (t && t.id === "shell" && sheetHandles && !(e.detail.xhr && e.detail.xhr._ccPoll)) {
      // user navigation (filter/pagination/window) closes the sheet;
      // the 30s poll swaps #shell too but must leave it open
      closeSheet();
    }
  });
  document.addEventListener("click", (e) => {
    if (e.target.closest("#sheet-close")) closeSheet();
  });

  /* ── Copy button ([data-copy="#selector"]) in the detail sheet ── */
  document.addEventListener("click", (e) => {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    var src = document.querySelector(btn.getAttribute("data-copy"));
    if (!src || !navigator.clipboard) return;
    navigator.clipboard.writeText(src.textContent || "").then(function () {
      var prev = btn.textContent;
      btn.textContent = "Copied";
      setTimeout(function () { btn.textContent = prev; }, 1200);
    }, function () {});
  });

  /* ── Network error banner ──────────────────────────────── */
  document.body.addEventListener("htmx:responseError", () => {
    var b = document.getElementById("net-error");
    if (b) b.hidden = false;
  });
  document.body.addEventListener("htmx:afterOnLoad", () => {
    var b = document.getElementById("net-error");
    if (b) b.hidden = true;
  });

  /* ── shadless runtime (switch, …) — body-level delegation ── */
  if (window.shadless) window.shadless.init();

  /* ── Charts: Chart.js line charts on [data-chart] canvases ─
     Server embeds {points, series} as JSON; we render with Chart.js
     using theme tokens (shadless ships no chart component, so the
     renderer + external tooltip are project code). */
  var charts = new WeakMap();

  function cssColor(value, el) {
    var probe = document.createElement("span");
    probe.style.color = value;
    probe.style.display = "none";
    (el && el.parentElement ? el.parentElement : document.body).appendChild(probe);
    var color = getComputedStyle(probe).color;
    probe.remove();
    return color;
  }

  function externalTooltip(context) {
    var chart = context.chart;
    var tip = context.tooltip;
    var el = chart.canvas._ccTip;
    if (!el) {
      el = document.createElement("div");
      el.className = "chart-tooltip";
      el.setAttribute("role", "status");
      el.hidden = true;
      el._ccCanvas = chart.canvas;
      document.body.appendChild(el);
      chart.canvas._ccTip = el;
    }
    if (tip.opacity === 0) {
      el.hidden = true;
      return;
    }
    var frag = document.createDocumentFragment();
    if (tip.title && tip.title.length) {
      var title = document.createElement("div");
      title.className = "chart-tooltip-title";
      title.textContent = tip.title.join(", ");
      frag.append(title);
    }
    var items = document.createElement("div");
    items.className = "chart-tooltip-items";
    tip.dataPoints.forEach(function (p) {
      var row = document.createElement("div");
      row.className = "chart-tooltip-item";
      var ind = document.createElement("span");
      ind.className = "chart-tooltip-indicator";
      ind.style.setProperty("--chart-indicator-color", typeof p.dataset.borderColor === "string" ? p.dataset.borderColor : "");
      var label = document.createElement("span");
      label.className = "chart-tooltip-label";
      label.textContent = p.dataset.label || p.label;
      var value = document.createElement("span");
      value.className = "chart-tooltip-value";
      value.textContent = p.formattedValue != null ? p.formattedValue : String(p.raw);
      row.append(ind, label, value);
      items.append(row);
    });
    frag.append(items);
    el.replaceChildren(frag);
    el.hidden = false;
    var rect = chart.canvas.getBoundingClientRect();
    el.style.left = rect.left + tip.caretX + "px";
    el.style.top = rect.top + tip.caretY + "px";
  }

  function renderChart(canvas) {
    // returning false leaves the canvas unmarked so a later scan
    // (poll swap, theme flip) retries instead of skipping it forever
    if (!window.Chart) return false;
    var raw = canvas.getAttribute("data-chart");
    if (!raw) return false;
    var data;
    try {
      data = JSON.parse(raw);
    } catch (err) {
      return false;
    }
    // a malformed payload (or a Chart constructor throw) must not abort
    // the scan loop for the remaining canvases — treat it as a failure
    try {
      // {labels: ["08-22", …], series: [{name, token, data: [...]}, …]}
      var single = data.series.length === 1;
      var datasets = data.series.map(function (ser) {
        // severity tokens (chart-fatal / chart-error) — resolved against the
        // live theme so a theme flip repaints with fresh colors
        var token = "var(--" + (ser.token || "chart-fatal") + ")";
        var color = cssColor(token, canvas);
        var fill = cssColor("color-mix(in oklab, " + token + " " + (single ? 14 : 8) + "%, transparent)", canvas);
        return {
          label: ser.name,
          data: ser.data,
          borderColor: color,
          backgroundColor: fill,
          pointBackgroundColor: color,
          fill: true,
        };
      });
      if (charts.has(canvas)) charts.get(canvas).destroy();
      var grid = cssColor("var(--chart-grid)", canvas);
      var ticks = cssColor("var(--muted-foreground)", canvas);
      var font = { size: 10 };
      charts.set(
        canvas,
        new window.Chart(canvas, {
          type: "line",
          data: {
            labels: data.labels,
            datasets: datasets,
          },
          options: {
            responsive: true,
            maintainAspectRatio: false,
            animation: false,
            scales: {
              y: {
                beginAtZero: true,
                grace: "10%",
                ticks: { color: ticks, font: font, precision: 0, maxTicksLimit: 3, padding: 4 },
                grid: { color: grid, drawTicks: false },
                border: { display: false },
              },
              x: {
                grid: { display: false },
                ticks: {
                  color: ticks,
                  font: font,
                  padding: 6,
                  maxRotation: 0,
                  autoSkip: true,
                  maxTicksLimit: 7,
                  callback: function (v) {
                    return this.getLabelForValue(v);
                  },
                },
                border: { display: false },
              },
            },
            layout: { padding: { top: 4, right: 6 } },
            elements: {
              line: { tension: 0.25, borderWidth: 1.75 },
              point: { radius: 0, hitRadius: 10, hoverRadius: 3.5, hoverBorderWidth: 0 },
            },
            interaction: { mode: "index", intersect: false },
            plugins: { legend: { display: false }, tooltip: { enabled: false, external: externalTooltip } },
          },
        }),
      );
    } catch (err) {
      return false;
    }
    return true;
  }

  // poll swaps replace the canvases; drop tooltips whose canvas is gone
  // so the hidden divs don't accumulate on <body>
  function pruneTooltips() {
    var tips = document.querySelectorAll("body > .chart-tooltip");
    for (var i = 0; i < tips.length; i++) {
      var cv = tips[i]._ccCanvas;
      if (cv && !cv.isConnected) tips[i].remove();
    }
  }

  function scan(root) {
    pruneTooltips();
    var nodes = (root || document).querySelectorAll(".chart-box canvas[data-chart]");
    for (var i = 0; i < nodes.length; i++) {
      if (nodes[i].dataset.rendered) continue;
      if (renderChart(nodes[i])) nodes[i].dataset.rendered = "1"; // only mark on success
    }
  }

  // theme flip changes token values — repaint charts with fresh colors
  function rescan() {
    var nodes = document.querySelectorAll("canvas[data-chart]");
    for (var i = 0; i < nodes.length; i++) {
      var c = charts.get(nodes[i]);
      if (c) {
        c.destroy();
        delete nodes[i].dataset.rendered;
      }
    }
    scan(document);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () {
      scan(document);
    });
  } else {
    scan(document);
  }
  // outerHTML swaps replace the target element, so e.detail.target is the
  // detached old node — scan the document (already-rendered canvases skip).
  document.body.addEventListener("htmx:afterSwap", function () {
    scan(document);
  });
  new MutationObserver(function (records) {
    if (records.some(function (r) { return r.attributeName === "class"; })) rescan();
  }).observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
})();
