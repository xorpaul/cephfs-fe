"use strict";

const $ = (id) => document.getElementById(id);
const TYPES = { f: "file", d: "dir", l: "symlink", h: "hardlink" };

let rows = [];         // last result, unsorted
let sort = { k: null, dir: 1 };
let running = null;    // AbortController of the search in flight
let volEntries = {};   // fs -> entries, for the full-scan warning
let lastParams = null; // params of the shown result, for the export links

const HINTS = {
  exact: "The whole entry name (not the path), case-sensitive. Uses the name index: fast.",
  prefix: "Names starting with this text, case-sensitive. Uses the name index: fast.",
  contains: "Names containing this text (at least 2 characters), case-sensitive. Reads every entry of the selected volumes.",
  regex: "A Go RE2 pattern on the name, e.g. ^tmp_[0-9a-f]+$. Anchored with ^ and a literal it uses the name index; otherwise it reads every entry and needs a literal of at least 2 characters.",
};

function fmtSize(n) {
  const u = ["B", "K", "M", "G", "T", "P"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + u[i];
}

function fmtTime(s) {
  const d = new Date(s * 1000);
  const p = (x) => String(x).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function el(tag, props = {}, ...kids) {
  const e = document.createElement(tag);
  Object.assign(e, props);
  for (const k of kids) e.append(k);
  return e;
}

// ---- dashboard (volume stats) -------------------------------------------

let statsTimer = null;

async function loadStats() {
  try {
    const r = await fetch("api/stats");
    const body = await r.json();
    if (!r.ok) throw new Error(body.error || r.statusText);
    renderDashboard(body.volumes);
    const pending = body.volumes.some((v) => v.pending);
    if (pending) {
      // Poll until all volumes have computed stats (back-off: 5 s then 15 s).
      const delay = statsTimer ? 15000 : 5000;
      statsTimer = setTimeout(loadStats, delay);
    } else {
      statsTimer = null;
    }
  } catch (e) {
    // Stats are informational; don't surface the error prominently.
    const tb = $("dash-rows");
    tb.innerHTML = "";
    const tr = document.createElement("tr");
    const td = document.createElement("td");
    td.colSpan = 10;
    td.className = "dash-loading err";
    td.textContent = "Could not load volume stats: " + e.message;
    tr.append(td);
    tb.append(tr);
  }
}

function dashCell(text, cls) {
  const td = document.createElement("td");
  if (cls) td.className = cls;
  td.textContent = text;
  return td;
}

function renderDashboard(volumes) {
  const tb = $("dash-rows");
  tb.textContent = "";
  if (!volumes.length) {
    const tr = document.createElement("tr");
    const td = document.createElement("td");
    td.colSpan = 10;
    td.className = "dash-loading";
    td.textContent = "No volumes found.";
    tr.append(td);
    tb.append(tr);
    return;
  }
  for (const v of volumes) {
    const tr = document.createElement("tr");
    tr.append(dashCell(v.fs));
    if (v.pending) {
      const td = document.createElement("td");
      td.colSpan = 9;
      td.className = "dash-loading";
      td.textContent = "Background scan running…";
      tr.append(td);
    } else if (v.error) {
      const td = document.createElement("td");
      td.colSpan = 9;
      td.className = "err";
      td.textContent = v.error;
      tr.append(td);
    } else {
      // Exact counts come from the index meta (newer cephfs-indexd);
      // older indexes only have planner estimates, marked with ≈.
      const est = v.counts_exact ? "" : "≈";
      const count = (n) => (n ? est + n.toLocaleString() : "—");
      const other = (v.symlinks || 0) + (v.hardlinks || 0) + (v.special || 0);
      const otherCell = dashCell(count(other), "num");
      otherCell.title = `symlinks ${(v.symlinks || 0).toLocaleString()}\n` +
        `hardlinks (extra names of multiply-linked files) ${(v.hardlinks || 0).toLocaleString()}\n` +
        (v.counts_exact ? `special (devices, FIFOs, sockets) ${(v.special || 0).toLocaleString()}` : "special: not counted (estimated counts)") +
        (v.counts_exact ? "" : "\nestimated from planner statistics; rare types are undercounted");
      tr.append(
        dashCell(fmtSize(v.table_bytes), "num"),
        dashCell(count(v.files), "num"),
        dashCell(count(v.dirs), "num"),
        otherCell,
        dashCell(v.started_at || "—"),
        dashCell(v.median_file_size != null ? fmtSize(v.median_file_size) : "—", "num"),
        dashCell(v.avg_file_size   != null ? fmtSize(v.avg_file_size)    : "—", "num"),
        dashCell(v.p90_file_size   != null ? fmtSize(v.p90_file_size)    : "—", "num"),
        dashCell(v.p99_file_size   != null ? fmtSize(v.p99_file_size)    : "—", "num"),
      );
    }
    tb.append(tr);
  }
}

// ---- volumes ------------------------------------------------------------

async function loadVolumes() {
  const box = $("volumes");
  try {
    const r = await fetch("api/volumes");
    const body = await r.json();
    if (!r.ok) throw new Error(body.error || r.statusText);
    const vs = body.volumes;
    $("version").textContent = "cephfs-fe " + body.version;
    const b = $("building");
    b.hidden = !body.building.length;
    b.textContent = body.building.length
      ? `Index build running for ${body.building.join(", ")}: it shares the disks, so searches are slower until it finishes.`
      : "";
    volEntries = Object.fromEntries(vs.map((v) => [v.fs, Number(v.entries) || 0]));
    box.textContent = "";
    for (const v of vs) {
      const cb = el("input", { type: "checkbox", value: v.fs, checked: true, disabled: !!v.error });
      cb.className = "fs";
      const lab = el("label", { className: "check" }, cb, v.fs);
      const tip = [v.prefix, v.started_at && "indexed " + v.started_at, v.entries && v.entries + " entries"].filter(Boolean);
      if (v.warning) { lab.append(el("span", { className: "badge", textContent: v.complete ? "!" : "partial" })); tip.push(v.warning); }
      if (v.error) { lab.append(el("span", { className: "badge err", textContent: "error" })); tip.push(v.error); }
      lab.title = tip.join("\n");
      box.append(lab);
    }
    if (!vs.length) box.textContent = "no indexes in the database";
  } catch (e) {
    box.textContent = "could not load volumes: " + e.message;
  }
  applyURL();
  updateHint();
}

function fsBoxes() { return [...document.querySelectorAll("input.fs:not(:disabled)")]; }

$("fs-all").addEventListener("change", (e) => fsBoxes().forEach((b) => (b.checked = e.target.checked)));
$("volumes").addEventListener("change", () => {
  const bs = fsBoxes();
  $("fs-all").checked = bs.every((b) => b.checked);
});

// ---- search -------------------------------------------------------------

function params() {
  const p = new URLSearchParams();
  p.set("pattern", $("pattern").value.trim());
  p.set("match", $("match").value);
  const bs = fsBoxes();
  if (!$("fs-all").checked) p.set("fs", bs.filter((b) => b.checked).map((b) => b.value).join(","));
  const t = document.querySelector("input[name=type]:checked").value;
  if (t) p.set("type", t);
  if ($("newer").value) p.set("newer", $("newer").value);
  if ($("uid").value.trim()) p.set("uid", $("uid").value.trim());
  return p;
}

// applyURL fills the form from the page URL, so searches can be linked.
function applyURL() {
  const p = new URLSearchParams(location.search);
  if (!p.has("pattern")) return;
  $("pattern").value = p.get("pattern");
  $("match").value = p.get("match") || (p.get("regex") === "1" ? "regex" : "exact");
  if (p.has("fs")) {
    const want = new Set(p.get("fs").split(","));
    fsBoxes().forEach((b) => (b.checked = want.has(b.value)));
    $("fs-all").checked = fsBoxes().every((b) => b.checked);
  }
  const t = document.querySelector(`input[name=type][value="${CSS.escape(p.get("type") || "")}"]`);
  if (t) t.checked = true;
  $("newer").value = p.get("newer") || "";
  $("uid").value = p.get("uid") || "";
  search();
}

async function search() {
  const p = params();
  if (!$("fs-all").checked && !p.get("fs")) { setStatus("Select at least one volume.", true); return; }
  history.replaceState(null, "", "?" + p);
  lastParams = p;
  running?.abort();
  const ac = (running = new AbortController());
  $("go").disabled = true;
  $("cancel").hidden = false;
  const t0 = performance.now();
  const tick = setInterval(() => setStatus(`Searching… ${((performance.now() - t0) / 1000).toFixed(0)}s`), 1000);
  setStatus("Searching…");
  try {
    const r = await fetch("api/search?" + p, { signal: ac.signal });
    const body = await r.json().catch(() => ({ error: r.statusText }));
    if (!r.ok) throw new Error(body.error || r.statusText);
    show(body);
    setStatus("");
  } catch (e) {
    if (e.name !== "AbortError") setStatus(e.message, true);
    else setStatus("Cancelled.");
  } finally {
    clearInterval(tick);
    if (running === ac) { running = null; $("go").disabled = false; $("cancel").hidden = true; }
  }
}

function setStatus(msg, err = false) {
  const s = $("status");
  s.textContent = msg;
  s.className = err ? "err" : "";
}

$("f").addEventListener("submit", (e) => { e.preventDefault(); search(); });
$("cancel").addEventListener("click", () => running?.abort());

// ---- results ------------------------------------------------------------

function show(res) {
  rows = res.rows;
  const sum = $("summary");
  sum.textContent = "";
  const shown = rows.length;
  const total = res.total.toLocaleString() + (res.total_capped ? "+" : "");
  sum.append(res.truncated
    ? el("span", { className: "warn", textContent: `${shown.toLocaleString()} of ${total} matches shown (an arbitrary subset; sorting reorders only these). Export for all of them.` })
    : `${total} match${res.total === 1 ? "" : "es"}`);
  sum.append(el("span", { className: "muted", textContent: ` · ${(res.took_ms / 1000).toFixed(1)}s · ${res.method}` }));
  const lossy = rows.filter((r) => r.lossy).length;
  const skipped = res.volumes.filter((v) => v.error).map((v) => v.fs);
  if (skipped.length) sum.append(el("span", { className: "err", textContent: ` · skipped ${skipped.join(", ")} (see per volume)` }));
  if (lossy) sum.append(el("span", { className: "warn", textContent: ` · ${lossy} path(s) not valid UTF-8, shown with �` }));

  const vb = $("vols");
  vb.textContent = "";
  for (const v of res.volumes) {
    vb.append(el("tr", {},
      el("td", { textContent: v.fs }),
      el("td", { className: "num", textContent: v.matches.toLocaleString() + (v.capped ? "+" : "") }),
      el("td", { className: "num", textContent: v.candidates.toLocaleString() }),
      el("td", { className: "num", textContent: v.query_ms + " ms" }),
      el("td", { className: "num", textContent: v.resolve_ms + " ms" }),
      el("td", { className: v.error ? "err" : v.warning ? "warn" : "", textContent: v.error ? "skipped: " + v.error : v.warning || "complete" })));
  }
  for (const f of ["csv", "jsonl"]) {
    const q = new URLSearchParams(lastParams);
    q.set("format", f);
    $("export-" + f).dataset.url = "api/export?" + q;
  }
  $("results").hidden = false;
  render();
}

function visible() {
  const f = $("filter").value;
  let out = f ? rows.filter((r) => r.path.includes(f)) : rows.slice();
  if (sort.k) {
    const k = sort.k, d = sort.dir;
    out.sort((a, b) => {
      const x = a[k] ?? -1, y = b[k] ?? -1;
      return (x < y ? -1 : x > y ? 1 : 0) * d;
    });
  }
  return out;
}

function render() {
  const tb = $("rows");
  const frag = document.createDocumentFragment();
  for (const r of visible()) {
    const path = el("td", { className: "path", textContent: r.path });
    if (r.lossy) { path.classList.add("lossy"); path.title = "Name is not valid UTF-8; replaced bytes are shown as �, so this path cannot be copied verbatim."; }
    frag.append(el("tr", {},
      path,
      el("td", { textContent: TYPES[r.type] || r.type }),
      el("td", { className: "num", textContent: r.uid ?? "-" }),
      el("td", { className: "num", textContent: fmtSize(r.size), title: r.size.toLocaleString() + " bytes" }),
      el("td", { textContent: fmtTime(r.mtime) })));
  }
  tb.textContent = "";
  tb.append(frag);
  document.querySelectorAll("#table th").forEach((th) => {
    th.classList.toggle("asc", th.dataset.k === sort.k && sort.dir === 1);
    th.classList.toggle("desc", th.dataset.k === sort.k && sort.dir === -1);
  });
}

document.querySelector("#table thead").addEventListener("click", (e) => {
  const k = e.target.dataset?.k;
  if (!k) return;
  sort = { k, dir: sort.k === k ? -sort.dir : 1 };
  render();
});
$("filter").addEventListener("input", render);

$("copy").addEventListener("click", async () => {
  const text = visible().map((r) => r.path).join("\n") + "\n";
  const b = $("copy");
  try {
    await navigator.clipboard.writeText(text);
    b.textContent = "Copied";
  } catch {
    b.textContent = "Copy failed";
  }
  setTimeout(() => (b.textContent = "Copy paths"), 1500);
});

async function triggerExport(btn) {
  const url = btn.dataset.url;
  if (!url) return;
  const origText = btn.textContent;
  btn.disabled = true;
  btn.textContent = "Downloading…";
  try {
    const r = await fetch(url);
    if (!r.ok) throw new Error(r.statusText);
    const cd = r.headers.get("content-disposition") || "";
    const m = cd.match(/filename="([^"]+)"/);
    const filename = m ? m[1] : "export";
    const blob = await r.blob();
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    setTimeout(() => { URL.revokeObjectURL(a.href); a.remove(); }, 60000);
  } catch (e) {
    alert("Export failed: " + e.message);
  } finally {
    btn.disabled = false;
    btn.textContent = origText;
  }
}

for (const f of ["csv", "jsonl"]) {
  $("export-" + f).addEventListener("click", function() { triggerExport(this); });
}

// Hint per match mode, and a warning before a search that reads every
// entry of big volumes.
function updateHint() {
  const m = $("match").value;
  $("hint").textContent = HINTS[m];
  $("pattern").placeholder = m === "regex" ? "^settings\\.py$" : "backup.json";
  const pat = $("pattern").value;
  const indexed = m === "exact" || m === "prefix" || (m === "regex" && /^\^[^\\.+*?()[\]{}|$^]/.test(pat));
  const sel = fsBoxes().filter((b) => b.checked).map((b) => b.value);
  const n = sel.reduce((a, fs) => a + (volEntries[fs] || 0), 0);
  const w = $("scanwarn");
  w.hidden = indexed || n < 100e6;
  w.textContent = w.hidden ? "" : `This reads all ${(n / 1e6).toFixed(0)} M entries of the selected volumes; on these disks that can take many minutes. Prefer exact name or starts with.`;
}
$("match").addEventListener("change", updateHint);
$("pattern").addEventListener("input", updateHint);
$("volumes").addEventListener("change", updateHint);
$("fs-all").addEventListener("change", updateHint);

loadVolumes();
loadStats();
