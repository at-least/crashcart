/* CrashCart viewer — the only client JS besides htmx and the shadless
   runtimes. Theme toggle, row navigation, keyboard triage on the issues
   list, the SSE "new issues" banner, copy buttons, auto-submitting filter
   forms. No dependencies. */
(() => {
  /* ── Theme ─────────────────────────────────────────────── */
  document.addEventListener("click", (e) => {
    var t = e.target.closest("#theme-toggle");
    if (!t) return;
    var el = document.documentElement;
    var next = el.classList.contains("dark") ? "light" : "dark";
    el.classList.toggle("dark", next === "dark");
    try { localStorage.setItem("cc-theme", next); } catch (err) {}
  });

  /* ── Row navigation: <tr data-href> opens on click / Enter unless a
     control inside the row was the target. ───────────────── */
  function rowTarget(e) {
    var row = e.target.closest("tr[data-href]");
    if (!row || e.target.closest("a,button,input,select,label")) return null;
    return row;
  }
  document.addEventListener("click", (e) => {
    var row = rowTarget(e);
    if (row) location.href = row.getAttribute("data-href");
  });

  /* ── Filter forms: submit on select/checkbox change ───── */
  document.addEventListener("change", (e) => {
    var f = e.target.form;
    if (!f || !f.hasAttribute("data-autosubmit")) return;
    if (e.target.matches("select,input[type=checkbox]")) f.requestSubmit();
  });

  /* ── Channel kind switches the visible field ──────────── */
  document.addEventListener("change", (e) => {
    if (!e.target.matches("[data-channel-kind]")) return;
    var kind = e.target.value;
    e.target.closest("form").querySelectorAll("[data-channel-field]").forEach((el) => {
      el.hidden = el.getAttribute("data-channel-field") !== kind;
    });
  });

  /* ── Copy buttons ([data-copy="#selector"]) ────────────── */
  document.addEventListener("click", (e) => {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    var src = document.querySelector(btn.getAttribute("data-copy"));
    if (!src || !navigator.clipboard) return;
    navigator.clipboard.writeText(src.textContent || "").then(() => {
      var prev = btn.textContent;
      btn.textContent = "Copied";
      setTimeout(() => { btn.textContent = prev; }, 1200);
    }, () => {});
  });

  /* ── Reload button in the banner ───────────────────────── */
  document.addEventListener("click", (e) => {
    if (e.target.closest("[data-action=reload]")) location.reload();
  });

  /* ── Form errors from htmx (4xx text bodies) ───────────── */
  document.body.addEventListener("htmx:responseError", (e) => {
    var form = e.detail.elt && e.detail.elt.closest("form");
    var out = form && form.querySelector("[data-form-error]");
    if (out) { out.textContent = e.detail.xhr.responseText || "Request failed"; out.hidden = false; }
  });

  /* ── Issues list: selection count + select-all ─────────── */
  function issueRows() { return Array.from(document.querySelectorAll('table[data-keyboard="issues"] tbody tr[data-fp]')); }
  function selectedFps() {
    return issueRows().filter((r) => r.querySelector("input.cb").checked).map((r) => r.getAttribute("data-fp"));
  }
  function updateBulk() {
    var n = selectedFps().length;
    var el = document.getElementById("bulk-n");
    if (el) el.textContent = String(n);
    var bar = document.getElementById("bulk-bar");
    if (bar) bar.hidden = issueRows().length === 0;
  }
  document.addEventListener("change", (e) => {
    if (e.target.id === "select-all") {
      issueRows().forEach((r) => { r.querySelector("input.cb").checked = e.target.checked; });
    }
    if (e.target.matches("input.cb")) updateBulk();
  });
  document.body.addEventListener("htmx:afterSwap", updateBulk);
  updateBulk();

  /* ── Keyboard triage: j/k move, x select, Enter open,
     r resolve, i ignore (selected rows, else the focused one) ── */
  var focused = -1;
  function focusRow(i) {
    var rows = issueRows();
    if (!rows.length) return;
    focused = Math.max(0, Math.min(rows.length - 1, i));
    rows.forEach((r, j) => r.classList.toggle("is-focused", j === focused));
    rows[focused].focus({ preventScroll: true });
    rows[focused].scrollIntoView({ block: "nearest" });
  }
  function bulk(status) {
    var form = document.getElementById("bulk-form");
    if (!form) return;
    var fps = selectedFps();
    var rows = issueRows();
    if (!fps.length && focused >= 0 && rows[focused]) fps = [rows[focused].getAttribute("data-fp")];
    if (!fps.length) return;
    var body = new URLSearchParams();
    fps.forEach((fp) => body.append("fp", fp));
    body.append("status", status);
    var ignore = form.querySelector('select[name="ignore"]');
    if (status === "ignored" && ignore) body.append("ignore", ignore.value);
    fetch(form.getAttribute("hx-post"), {
      method: "POST",
      headers: { "HX-Request": "true", "Content-Type": "application/x-www-form-urlencoded" },
      body: body.toString(),
      credentials: "same-origin",
    }).then((res) => res.text()).then((html) => {
      var table = document.getElementById("issues-table");
      if (!table) return;
      var tpl = document.createElement("template");
      tpl.innerHTML = html.trim();
      var next = tpl.content.firstElementChild;
      table.replaceWith(next);
      if (window.htmx) window.htmx.process(next);
      updateBulk();
      focusRow(focused);
    });
  }
  document.addEventListener("keydown", (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.target.matches("input,select,textarea") || e.target.isContentEditable) return;
    if (!document.querySelector('table[data-keyboard="issues"]')) return;
    var rows = issueRows();
    switch (e.key) {
      case "j": focusRow(focused + 1); break;
      case "k": focusRow(focused - 1); break;
      case "x":
        if (focused >= 0 && rows[focused]) {
          var cb = rows[focused].querySelector("input.cb");
          cb.checked = !cb.checked;
          updateBulk();
        }
        break;
      case "Enter":
        if (focused >= 0 && rows[focused]) location.href = rows[focused].getAttribute("data-href");
        break;
      case "r": bulk("resolved"); break;
      case "i": bulk("ignored"); break;
      default: return;
    }
    e.preventDefault();
  });

  /* ── SSE: "N new issues — refresh" banner (never auto-refreshes) ── */
  var shell = document.getElementById("shell");
  var streamURL = shell && shell.getAttribute("data-stream");
  if (streamURL && window.EventSource) {
    var baseline = parseInt(shell.getAttribute("data-regressions") || "0", 10) || 0;
    var es = new EventSource(streamURL);
    es.addEventListener("issues", (ev) => {
      var d;
      try { d = JSON.parse(ev.data); } catch (err) { return; }
      var parts = [];
      if (d.new > 0) parts.push(d.new + (d.new === 1 ? " new issue" : " new issues"));
      if (d.regressions > baseline) parts.push((d.regressions - baseline) + (d.regressions - baseline === 1 ? " new regression" : " new regressions"));
      var box = document.getElementById("new-issues");
      var text = document.getElementById("new-issues-text");
      if (!box || !text) return;
      if (!parts.length) { box.hidden = true; return; }
      text.textContent = parts.join(", ");
      box.hidden = false;
    });
    window.addEventListener("pagehide", () => es.close());
  }

  /* ── shadless runtime (switch, …) — body-level delegation ── */
  if (window.shadless && window.shadless.init) window.shadless.init();
})();
