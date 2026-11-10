// Renders the shared app.Session. Everything from the card (names, paths) goes into the page as
// text (textContent), never as HTML.

const $ = (id) => document.getElementById(id);
let csrf = "";
let view = null;
let lastAnnounced = -1;
let lastSig = "";
const drafts = {}; // what the person typed survives re-renders (the card list refreshes itself)
const speeds = [];

function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === false || v == null) continue;
    if (k === "class") el.className = v;
    else if (k === "style") el.style.cssText = v; // CSSOM: the CSP blocks style attributes, not this
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (v === true) el.setAttribute(k, "");
    else el.setAttribute(k, v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

function gb(n) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GB";
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  return Math.ceil(n / 1024) + " KB";
}
const commas = (n) => Number(n || 0).toLocaleString("en");

async function api(path, opts = {}) {
  const r = await fetch(path, { credentials: "same-origin", ...opts });
  if (r.status === 403) { showGone(); throw new Error("forbidden"); }
  return r;
}

async function doCmd(name, args = {}) {
  const r = await api("/api/do", {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-CSRF": csrf },
    body: JSON.stringify({ name, args }),
  });
  if (r.status === 422) {
    const { error } = await r.json();
    await refresh();
    if (!$("main").querySelector('[role="alert"]')) flash(error); // the step shows view.error itself
    return false;
  }
  await refresh();
  return true;
}

function flash(msg) {
  const main = $("main");
  const old = main.querySelector(".flash");
  if (old) old.remove();
  main.prepend(h("div", { class: "note error flash", role: "alert" }, h("span", { class: "kind" }, "Not done: "), msg));
}

function showGone() {
  document.body.replaceChildren(h("main", { id: "main", class: "layout" },
    h("div", {}, h("h1", {}, "This window is no longer connected"),
      h("p", {}, "Run aio-sdcard-doctor gui again: it reopens the running copy, and anything that was working carries on."))));
}

async function refresh() {
  const r = await api("/api/view");
  const d = await r.json();
  view = d.View;
  render();
}

// ---- frame ------------------------------------------------------------------------------------

const stepNames = {
  drive: "Card", action: "What to do", choice: "What to copy", dest: "Where", options: "Options",
  review: "Check", elevate: "Administrator", run: "Working", "recover-list": "Pick files", results: "Done",
};

function renderFrame() {
  const c = view.card;
  const id = $("card-id");
  if (c) {
    const bits = [c.Label || c.Root, c.FS, c.Total ? gb(c.Total) : "", c.Removable ? "removable" : "", c.Root];
    id.replaceChildren(h("strong", {}, bits.filter(Boolean).join(" · ")), " ",
      h("span", { class: "badge good" }, "Only read"));
  } else id.textContent = "No card chosen";

  const steps = view.steps && view.steps.length > 2 ? view.steps : ["drive", "action"];
  const at = steps.indexOf(view.step);
  $("steps").replaceChildren(...steps.map((s, i) =>
    h("li", { "aria-current": s === view.step ? "step" : false, class: i < at ? "done" : false }, stepNames[s] || s)));

  const p = view.progress;
  if (view.step === "run" && p && p.Total) {
    document.title = `${Math.floor((100 * p.Done) / p.Total)}% · ${view.runLabel || "Working"}`;
  } else if (view.step === "results") {
    document.title = "Done — SD card doctor";
  } else document.title = "SD card doctor";
}

// ---- steps ------------------------------------------------------------------------------------

function errorNote() {
  return view.error ? h("div", { class: "note error", role: "alert" }, h("span", { class: "kind" }, "Problem: "), view.error) : null;
}

function busyNote() {
  return view.busy ? h("p", { class: "note", role: "status" }, view.busy + "…") : null;
}

function driveTile(d) {
  const badges = [];
  if (d.Removable) badges.push(h("span", { class: "badge good" }, "Removable"));
  if (d.System) badges.push(h("span", { class: "badge warn" }, "System"));
  if (d.Locked) badges.push(h("span", { class: "badge bad" }, "Locked"));
  if (d.WriteProtected) badges.push(h("span", { class: "badge" }, "Write-protected"));
  if (d.RAW) badges.push(h("span", { class: "badge warn" }, "File system not readable by the computer"));
  const used = d.Total ? Math.round((100 * (d.Total - d.Free)) / d.Total) : 0;
  return h("button", { class: "tile", type: "button", onclick: () => doCmd("select-drive", { root: d.Root }) },
    h("span", { class: "title" }, d.Label || d.Root),
    h("span", { class: "desc" }, [d.Root, d.FS, d.Total ? gb(d.Total) : ""].filter(Boolean).join(" · ")),
    d.Total ? h("span", { class: "meter", role: "img", "aria-label": `${used}% used` }, h("span", { style: `width:${used}%` })) : null,
    h("span", { class: "badges" }, badges));
}

function stepDrive() {
  const cards = view.drives.filter((d) => d.Removable);
  const others = view.drives.filter((d) => !d.Removable);
  let path = drafts.path || "";
  const input = h("input", { type: "text", id: "path", autocomplete: "off", value: path, placeholder: "e.g. D:\\card.img or D:\\copy of the card",
    oninput: (e) => (drafts.path = path = e.target.value) });
  return [
    h("h1", {}, "Choose the card"),
    h("p", { class: "muted" }, "Nothing on the card is changed unless you choose Card tools later and confirm."),
    cards.length ? h("div", { class: "grid" }, cards.map(driveTile))
      : h("p", { class: "note", role: "status" }, "Insert the card — it will appear here automatically."),
    others.length ? [h("h2", {}, "Other drives"), h("div", { class: "grid" }, others.map(driveTile))] : null,
    view.hiddenSystem ? h("button", { type: "button", class: "link", onclick: () => doCmd("show-system") },
      `Show system drives (${view.hiddenSystem} hidden)`) : null,
    h("h2", {}, "Or use a folder or a card image file"),
    h("label", { for: "path" }, "Folder or image file"),
    input,
    h("div", { class: "actions-row" }, h("button", { type: "button", onclick: () => doCmd("select-path", { path }) }, "Use this")),
  ];
}

function stepAction() {
  const acts = view.actions || [];
  const tile = (a) => h("button", { type: "button", class: "tile" + (a.danger ? " danger-tile" : ""), disabled: !a.enabled,
    "aria-describedby": a.reason ? `why-${a.name}` : false, onclick: () => doCmd("choose-action", { action: a.name }) },
    h("span", { class: "title" }, a.label),
    h("span", { class: "desc" }, a.desc + (a.admin ? " (asks for administrator rights)" : "")),
    a.reason ? h("span", { class: "why", id: `why-${a.name}` }, a.reason) : null);
  const rescue = acts.find((a) => a.name === "rescue");
  return [
    h("h1", {}, "What do you want to do?"),
    rescue && rescue.enabled ? h("button", { type: "button", class: "primary quick", onclick: () => doCmd("quick-rescue") },
      h("span", { class: "title" }, "Copy everything off this card"),
      h("span", { class: "desc" }, "Every file, nothing skipped — you'll check the details before it starts")) : null,
    h("h2", {}, "Or choose"),
    h("div", { class: "grid" }, acts.filter((a) => !a.danger).map(tile)),
    h("section", { class: "tools", "aria-labelledby": "tools-h" },
      h("h2", { id: "tools-h" }, "Card tools — these change the card"),
      h("p", { class: "small" }, "Repair, format or test the card. Copy your files off first."),
      h("div", { class: "grid" }, acts.filter((a) => a.danger).map(tile))),
    h("div", { class: "actions-row" }, h("button", { type: "button", onclick: () => doCmd("back") }, "Back")),
  ];
}

function stepChoice() {
  let picked = view.req.backup || "standard";
  const rows = view.choices || [];
  return [
    h("h1", {}, "What should be copied?"),
    h("p", { class: "muted" }, "Files left out are still listed in the report, with the reason."),
    busyNote(),
    rows.length ? h("ul", { class: "choice-list", role: "radiogroup", "aria-label": "What to copy" }, rows.map((c) =>
      h("li", {}, h("label", { class: "check" },
        h("input", { type: "radio", name: "choice", value: c.id, checked: c.id === picked, onchange: () => (picked = c.id) }),
        h("span", {}, h("strong", {}, c.title), h("br"), h("span", { class: "small" }, c.desc), h("br"),
          h("span", { class: "counts" }, `${commas(c.files)} files · ${gb(c.bytes)}`)))))) : null,
    h("div", { class: "actions-row" },
      h("button", { type: "button", onclick: () => doCmd("back") }, "Back"),
      h("button", { type: "button", class: "primary", disabled: !rows.length, onclick: () => doCmd("set-choice", { id: picked }) }, "Continue")),
  ];
}

function folderBrowser(onPick) {
  const box = h("div", { class: "folder", role: "group", "aria-label": "Choose a folder" });
  async function show(path) {
    const r = await api("/api/dirs?path=" + encodeURIComponent(path));
    const d = await r.json();
    if (d.error) { flash(d.error); return; }
    const list = h("ul", {}, (d.Dirs || []).map((name) => {
      const full = d.Path ? (d.Path.endsWith("\\") || d.Path.endsWith("/") ? d.Path + name : d.Path + (d.Path.includes("\\") ? "\\" : "/") + name) : name;
      return h("li", {}, h("button", { type: "button", onclick: () => show(full) }, name));
    }));
    box.replaceChildren(h("div", { class: "head" },
      h("button", { type: "button", disabled: !d.Path, onclick: () => show(d.Parent || "") }, "Up"),
      h("span", { class: "mono" }, d.Path || "This computer"),
      d.Path ? h("button", { type: "button", class: "primary", onclick: () => onPick(d.Path) }, "Use this folder") : null), list);
  }
  show("");
  return box;
}

function stepDest() {
  const isImage = view.req.action === "image";
  let path = drafts.dest ?? (view.req.dest || "");
  const input = h("input", { type: "text", id: "dest", value: path, autocomplete: "off",
    oninput: (e) => (drafts.dest = path = e.target.value) });
  return [
    h("h1", {}, isImage ? "Where should the image file go?" : "Where should the files go?"),
    h("p", { class: "muted" }, isImage ? "Choose a file name on another disk, e.g. D:\\card.img." :
      "Choose a folder on another disk — never on the card itself."),
    h("label", { for: "dest" }, isImage ? "Image file" : "Folder"),
    input,
    !isImage ? h("details", {}, h("summary", {}, "Browse folders on this computer"),
      folderBrowser((p) => { drafts.dest = path = p; input.value = p; })) : null,
    h("div", { class: "actions-row" },
      h("button", { type: "button", onclick: () => doCmd("back") }, "Back"),
      h("button", { type: "button", class: "primary", onclick: () => doCmd("set-dest", { path }) }, "Continue")),
  ];
}

function tick(name, label, hint, on, locked) {
  const id = "opt-" + name;
  return h("label", { class: "check", for: id },
    h("input", { type: "checkbox", id, checked: on, disabled: !!locked, "aria-describedby": locked ? "undelete-why" : false,
      onchange: (e) => doCmd("set-option", { name, value: e.target.checked ? "1" : "0" }) }),
    h("span", {}, label, hint ? [h("br"), h("span", { class: "small muted" }, hint)] : null));
}

function stepOptions() {
  const r = view.req;
  const body = [];
  if (r.action === "rescue") {
    body.push(tick("compress", "Compress (max) into ZIP files", "Saves little on photos and videos; helps documents and RAW", r.compress),
      tick("skip-empty", "Leave out empty (0-byte) files", "They are still listed in the report", r.skipEmpty));
  } else if (r.action === "image") {
    body.push(tick("compress", "Compress the image (card.img.xz) when finished", "Needs room for both until it's checked", r.compress));
  } else if (r.action === "recover") {
    const u = view.undelete;
    if (u) body.push(h("div", { class: "note", id: "undelete-why" }, h("span", { class: "kind" }, "Undelete isn't available on this card: "), u.Reason,
      h("br"), h("span", { class: "small" }, "Deep scan is on — it's the only way to find deleted files here.")));
    body.push(tick("deep", "Deep scan", "Slower: also finds files whose names are gone", r.deep, !!u),
      tick("thorough", "Thorough", "Much slower: looks at every byte, not only sector starts", r.thorough),
      tick("compress", "Compress into ZIP files", "", r.compress));
  } else if (r.action === "verify") {
    let m = r.manifest || "", t = r.target || "";
    body.push(h("label", { for: "manifest" }, "The rescue to check (its Reports\\manifest.json)"),
      h("input", { type: "text", id: "manifest", value: m, oninput: (e) => (m = e.target.value),
        onchange: () => doCmd("set-option", { name: "manifest", value: m }) }),
      h("label", { for: "target" }, "Where the files are now (leave empty for the rescue folder)"),
      h("input", { type: "text", id: "target", value: t, oninput: (e) => (t = e.target.value),
        onchange: () => doCmd("set-option", { name: "target", value: t }) }));
  } else if (r.action === "fix") {
    // unavailable tools stay listed, greyed, with why (never hidden)
    body.push(h("ul", { class: "choice-list", role: "radiogroup", "aria-label": "Card tool" }, (view.tools || []).map((t) =>
      h("li", {}, h("label", { class: "check", for: "fixop-" + t.name },
        h("input", { type: "radio", name: "fixop", id: "fixop-" + t.name, value: t.name, checked: r.fixOp === t.name, disabled: !t.enabled,
          "aria-describedby": t.enabled ? false : "fixop-why-" + t.name,
          onchange: () => doCmd("set-option", { name: "fix-op", value: t.name }) }),
        h("span", {}, h("strong", {}, t.label), h("br"), h("span", { class: "small " + (t.danger ? "" : "muted") }, t.desc),
          t.enabled ? null : [h("br"), h("span", { class: "why", id: "fixop-why-" + t.name }, "Not available: " + t.reason)]))))));
  }
  return [
    h("h1", {}, "Options"),
    body.length ? body : h("p", {}, "Nothing to set for this one."),
    h("div", { class: "actions-row" },
      h("button", { type: "button", onclick: () => doCmd("back") }, "Back"),
      h("button", { type: "button", class: "primary", onclick: () => doCmd("next") }, "Continue")),
  ];
}

function stepReview() {
  if (view.busy || !view.plan) return [h("h1", {}, "Please check before it starts"), busyNote(), errorNote(),
    h("div", { class: "actions-row" }, h("button", { type: "button", onclick: () => doCmd("back") }, "Back"))];
  const p = view.plan;
  const issues = p.Issues || [];
  const blocks = issues.filter((i) => i.Severity === "blocked" && i.Kind !== "needs-admin");
  const warns = issues.filter((i) => i.Severity === "warning");
  const notes = issues.filter((i) => i.Severity !== "blocked" && i.Severity !== "warning");
  const hasDest = ["rescue", "image", "recover"].includes(view.req.action);
  // what was typed or ticked survives a refused Start (the page re-renders with the reason)
  const start = h("button", { type: "button", class: view.confirm ? "danger" : "primary", disabled: blocks.length > 0,
    onclick: () => doCmd("start", { accept: drafts.accept ? "1" : "", label: drafts.clabel || "", size: drafts.csize || "" }) },
    view.confirm ? "Change the card" : "Start");
  const issue = (i, kind, cls) => h("div", { class: "note " + cls, role: cls === "block" ? "alert" : false },
    h("span", { class: "kind" }, kind + ": "), i.Title, i.Detail ? [h("br"), h("span", { class: "small" }, i.Detail)] : null);
  return [
    h("h1", {}, "Please check before it starts"),
    h("ul", { class: "summary" }, (p.Summary || []).map((l) => h("li", {}, l))),
    p.Result && p.Result.length ? [h("h2", {}, "You'll get"), h("ul", {}, p.Result.map((l) => h("li", {}, l)))] : null,
    blocks.map((i) => issue(i, "Can't start", "block")),
    warns.map((i) => issue(i, "Warning", "warn")),
    notes.map((i) => issue(i, "Note", "")),
    view.canStartOver ? h("label", { class: "check", for: "fresh" },
      h("input", { type: "checkbox", id: "fresh", checked: !!view.req.fresh,
        onchange: (e) => doCmd("set-option", { name: "fresh", value: e.target.checked ? "1" : "0" }) }),
      h("span", {}, "Start over instead of continuing where it stopped")) : null,
    warns.length ? h("label", { class: "check", for: "accept" },
      h("input", { type: "checkbox", id: "accept", checked: !!drafts.accept, onchange: (e) => (drafts.accept = e.target.checked) }),
      h("span", {}, "I've read the warnings above and want to go ahead")) : null,
    view.confirm ? h("div", { class: "danger-box" },
      h("h2", {}, "Type the card's label and size to confirm"),
      h("p", {}, "This changes the card. Type ", h("strong", {}, view.confirm.label), " and ", h("strong", {}, view.confirm.size), " exactly as shown."),
      h("label", { for: "c-label" }, "Label"), h("input", { type: "text", id: "c-label", autocomplete: "off", value: drafts.clabel || "", oninput: (e) => (drafts.clabel = e.target.value) }),
      h("label", { for: "c-size" }, "Size"), h("input", { type: "text", id: "c-size", autocomplete: "off", value: drafts.csize || "", oninput: (e) => (drafts.csize = e.target.value) })) : null,
    errorNote(),
    h("div", { class: "actions-row" }, h("button", { type: "button", onclick: () => doCmd("back") }, "Back"),
      hasDest ? h("button", { type: "button", onclick: () => doCmd("edit-dest") }, "Change where the files go") : null, start),
  ];
}

function stepElevate() {
  return [
    h("h1", {}, "This needs administrator rights"),
    h("p", {}, "Reading the card directly needs them. Close this window and run:"),
    h("p", { class: "mono note" }, view.instruction),
    h("div", { class: "actions-row" }, h("button", { type: "button", onclick: () => doCmd("back") }, "Back")),
  ];
}

function trace() {
  const max = Math.max(1, ...speeds);
  const pts = speeds.map((v, i) => `${(i / Math.max(1, speeds.length - 1)) * 300},${60 - (v / max) * 56}`).join(" ");
  const last = speeds.length ? speeds[speeds.length - 1] : 0;
  return h("figure", {},
    svgEl(pts),
    h("figcaption", { class: "muted" }, `Read speed: ${(last / 1e6).toFixed(1)} MB/s now, ${(max / 1e6).toFixed(1)} MB/s best`));
}

function svgEl(points) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 300 64");
  svg.setAttribute("class", "trace");
  svg.setAttribute("aria-hidden", "true");
  const pl = document.createElementNS(ns, "polyline");
  pl.setAttribute("points", points);
  svg.append(pl);
  return svg;
}

// runLive is the part of the run screen that progress updates replace; nothing focusable is in it,
// so keyboard focus (on Stop) is never lost to a progress tick.
function runLive() {
  const p = view.progress || {};
  const pct = p.Total ? Math.floor((100 * p.Done) / p.Total) : 0;
  const c = p.Counters || {};
  const safe = (c.OK || 0) + (c["ALREADY-COPIED"] || 0) + (c["DELETED-RECOVERED"] || 0) + (c.CARVED || 0);
  const total = p.ItemsTotal || (view.plan && view.plan.Items) || 0;
  const hero = p.Counters ? `${commas(safe)}${total ? " of " + commas(total) : ""} files safe so far` : `${pct}%`;
  return [
    h("p", { class: "hero" }, hero),
    h("div", { class: "bar", role: "progressbar", "aria-valuemin": "0", "aria-valuemax": "100", "aria-valuenow": String(pct), "aria-label": "Progress" },
      h("span", { style: `width:${pct}%` })),
    h("p", { class: "stats" }, h("span", {}, h("b", {}, `${pct}%`), " done"),
      p.Total ? h("span", {}, h("b", {}, gb(p.Done)), ` of ${gb(p.Total)}`) : null,
      p.BPS ? h("span", {}, h("b", {}, `${(p.BPS / 1e6).toFixed(1)} MB/s`)) : null),
    p.Item ? h("p", { class: "current" }, p.Item) : null,
    p.Note ? h("p", { class: "note warn" }, p.Note) : null,
  ];
}

function stepRun() {
  return [
    h("h1", {}, view.runLabel || "Working"),
    h("div", { id: "run-live" }, runLive()),
    view.logs && view.logs.length ? [h("h2", {}, "Log"), h("div", { class: "log" }, view.logs.slice(-200).map((l) => h("div", {}, l)))] : null,
    h("div", { class: "actions-row" }, h("button", { type: "button", id: "stop", onclick: () => doCmd("cancel") }, "Stop")),
  ];
}

function stepRecoverList() {
  const rv = view.recover || { rows: [], total: 0, shown: 0, ticked: 0 };
  let filter = rv.filter || "";
  const states = { Excellent: "Excellent", Poor: "Poor", Unrecoverable: "Unrecoverable" };
  return [
    h("h1", {}, "Pick the files to bring back"),
    h("p", { class: "muted" }, `${commas(rv.total)} found · ${commas(rv.shown)} shown · ${commas(rv.ticked)} ticked. Unrecoverable files are listed with the reason and can't be ticked.`),
    h("label", { for: "filter" }, "Filter by name or folder"),
    h("input", { type: "text", id: "filter", value: filter, oninput: (e) => (filter = e.target.value),
      onchange: () => doCmd("recover-filter", { text: filter }) }),
    h("div", { class: "actions-row" }, h("button", { type: "button", onclick: () => doCmd("recover-tick-all") }, "Tick all Excellent and Poor")),
    h("div", { class: "table-wrap" }, h("table", { class: "recover-table" },
      h("caption", { class: "sr-only" }, "Files that can be recovered"),
      h("thead", {}, h("tr", {}, ["Pick", "State", "Name", "Folder", "Size", "Modified", "Comment"].map((t, i) =>
        h("th", { scope: "col", class: i === 4 ? "num" : false }, t)))),
      h("tbody", {}, rv.rows.map((r, i) => h("tr", {},
        h("td", {}, h("label", { class: "pick", for: `r${i}` }, h("input", { type: "checkbox", id: `r${i}`, checked: r.ticked, disabled: !r.tickable, "aria-label": `Pick ${r.name}`,
          onchange: () => doCmd("recover-tick", { id: r.id }) }))),
        h("td", {}, h("span", { class: "state " + r.state }, states[r.state] || r.state)),
        h("td", {}, r.name), h("td", { class: "mono" }, r.path), h("td", { class: "num" }, gb(r.size)),
        h("td", {}, r.modified || ""), h("td", { class: "small" }, r.comment)))))),
    h("div", { class: "actions-row" },
      h("button", { type: "button", onclick: () => doCmd("restart") }, "Start over"),
      h("button", { type: "button", class: "primary", disabled: !rv.ticked, onclick: () => doCmd("recover-start") }, `Recover ${commas(rv.ticked)} files`)),
  ];
}

function stepResults() {
  const res = view.result || { Counters: {} };
  const counters = Object.entries(res.Counters || {});
  return [
    h("h1", {}, view.error ? "Stopped" : res.Clean ? "Done" : "Done, with problems"),
    errorNote(),
    counters.length ? h("table", {}, h("caption", { class: "sr-only" }, "What happened to each file"),
      h("thead", {}, h("tr", {}, h("th", { scope: "col" }, "Result"), h("th", { scope: "col", class: "num" }, "Files"))),
      h("tbody", {}, counters.map(([k, v]) => h("tr", {}, h("th", { scope: "row" }, k), h("td", { class: "num" }, commas(v)))))) : null,
    res.Reports && res.Reports.length ? [h("h2", {}, "Reports"), h("ul", {}, res.Reports.filter(Boolean).map((r) => h("li", { class: "mono" }, r)))] : null,
    view.logs && view.logs.length ? [h("h2", {}, "Details"), h("div", { class: "log" }, view.logs.map((l) => h("div", {}, l)))] : null,
    view.next && view.next.length ? [h("h2", {}, "What next"), view.next.map((n) =>
      h("p", {}, h("button", { type: "button", class: "primary", onclick: () => doCmd("next-action", { action: n.action }) }, n.label), " ", h("span", { class: "muted" }, n.why)))] : null,
    h("div", { class: "actions-row" },
      view.req.dest ? h("button", { type: "button", onclick: () => api("/api/open-dest", { method: "POST", headers: { "X-CSRF": csrf } }) }, "Open the folder") : null,
      h("button", { type: "button", onclick: () => doCmd("restart") }, "Start over")),
  ];
}

const help = {
  drive: ["Which card?", "Cards and USB drives are listed first. The card is only read until you choose Card tools and confirm."],
  action: ["Where to start", "Copy files first. If the card is failing, a full backup image lets it rest while you work from the image."],
  choice: ["Choosing", "The counts are this card's files. Anything you leave out is still listed in the report."],
  dest: ["Where", "Choose a folder on another disk. A folder from an earlier run of the same card continues where it stopped."],
  options: ["Options", "These only change how the copy is stored, not what is read."],
  review: ["Check", "This is exactly what will happen. Nothing has started yet."],
  run: ["While it works", "You can stop any time; running the same thing again continues where it stopped."],
  "recover-list": ["States", "Excellent: nothing overwritten. Poor: may come back damaged. Unrecoverable: its space was reused."],
  results: ["Done", "Every file is in the report with its result."],
  elevate: ["Why", "Reading a card directly (images, deleted files, unreadable cards) needs administrator rights."],
};

function render() {
  if (!view) return;
  const sig = JSON.stringify(view);
  if (sig === lastSig) return; // nothing changed: leave the page (and what's being typed) alone
  lastSig = sig;
  renderFrame();
  const fn = { drive: stepDrive, action: stepAction, choice: stepChoice, dest: stepDest, options: stepOptions, review: stepReview,
    elevate: stepElevate, run: stepRun, "recover-list": stepRecoverList, results: stepResults }[view.step];
  const main = $("main");
  // focus survives a re-render: by id, or for a button by its words
  const ae = document.activeElement;
  const keepFocus = ae && ae !== document.body ? (ae.id ? "#" + ae.id : ae.tagName === "BUTTON" ? "btn:" + ae.textContent : "") : "";
  const stepChanged = main.dataset.step !== view.step;
  if (drafts.action !== view.req.action) { // a new flow: nothing typed for the last one carries over
    for (const k of Object.keys(drafts)) if (k !== "path") delete drafts[k];
    drafts.action = view.req.action;
  }
  main.dataset.step = view.step;
  main.replaceChildren(...[view.step !== "review" && view.step !== "results" ? errorNote() : null, ...(fn ? fn() : [])].flat(Infinity).filter(Boolean));
  const [ht, hp] = help[view.step] || ["", ""];
  $("side").replaceChildren(...[h("h2", {}, ht), h("p", {}, hp), view.step === "run" && speeds.length > 1 ? trace() : null].filter(Boolean));
  if (stepChanged) {
    const h1 = main.querySelector("h1");
    if (h1) { h1.tabIndex = -1; h1.focus(); }
  } else if (keepFocus.startsWith("#") && $(keepFocus.slice(1))) {
    $(keepFocus.slice(1)).focus();
  } else if (keepFocus.startsWith("btn:")) {
    const same = [...main.querySelectorAll("button")].find((b) => "btn:" + b.textContent === keepFocus);
    if (same) same.focus();
  }
  renderIssue();
}

function renderIssue() {
  const dlg = $("ask");
  const iv = view.issue;
  if (!iv) { if (dlg.open) dlg.close(); return; }
  if (dlg.open && dlg.dataset.id === iv.id) return;
  const is = iv.issue;
  const safe = (is.Choices || []).includes("stop") ? "stop" : is.Default;
  dlg.dataset.id = iv.id;
  dlg.replaceChildren(h("h2", { id: "ask-title" }, is.Title), is.Detail ? h("p", {}, is.Detail) : null,
    h("div", { class: "actions-row" }, (is.Choices || []).map((c) => h("button", { type: "button", class: c === is.Default ? "primary" : false,
      onclick: () => { dlg.close(); doCmd("answer", { id: iv.id, choice: c }); } }, choiceLabel(c)))));
  dlg.oncancel = (e) => { e.preventDefault(); dlg.close(); doCmd("answer", { id: iv.id, choice: safe }); }; // Esc = the safe choice
  dlg.showModal();
  $("live").textContent = is.Title;
}

const labels = { continue: "Continue", stop: "Stop", retry: "Try again", wait: "Wait for the card", "skip-rest": "Skip the rest (report them failed)",
  resume: "Continue where it stopped", new: "Start over", elevate: "Restart as administrator", "skip-videos": "Skip videos", "image-instead": "Make an image instead" };
const choiceLabel = (c) => labels[c] || c;

// ---- live updates -----------------------------------------------------------------------------

function onProgress(p) {
  if (!view) return;
  view.progress = p;
  if (p.BPS) { speeds.push(p.BPS); if (speeds.length > 60) speeds.shift(); }
  const live = $("run-live");
  if (view.step === "run" && live) { // patch the numbers in place: the Stop button and focus stay put
    live.replaceChildren(...runLive().flat(Infinity).filter(Boolean));
    lastSig = JSON.stringify(view);
    const side = $("side").querySelector("figure");
    if (speeds.length > 1) { const f = trace(); if (side) side.replaceWith(f); else $("side").append(f); }
    renderFrame();
  } else if (view.step === "run") render();
  const pct = p.Total ? Math.floor((10 * p.Done) / p.Total) * 10 : -1;
  if (pct >= 0 && pct !== lastAnnounced) { lastAnnounced = pct; $("live").textContent = `${pct}% done`; }
}

function connect() {
  const es = new EventSource("/api/events");
  es.onmessage = (m) => {
    const ev = JSON.parse(m.data);
    if (ev.kind === "progress") onProgress(ev.progress);
    else refresh();
  };
  // after any reconnect (the server drops a window that falls behind) reload the whole view, so a
  // question or a finish that was missed is shown
  es.addEventListener("hello", () => refresh());
  es.onerror = () => { /* the browser reconnects by itself */ };
  setInterval(() => { if (view && (view.step === "run" || view.issue)) refresh(); }, 5000); // safety net
}

function themeToggle() {
  const btn = $("theme");
  const order = ["auto", "light", "dark"];
  let t = "auto";
  try { t = localStorage.getItem("theme") || "auto"; } catch { /* private window */ }
  const apply = () => {
    if (t === "auto") document.documentElement.removeAttribute("data-theme");
    else document.documentElement.dataset.theme = t;
    btn.textContent = { auto: "Theme: automatic", light: "Theme: light", dark: "Theme: dark" }[t];
    btn.setAttribute("aria-label", btn.textContent);
  };
  btn.onclick = () => { t = order[(order.indexOf(t) + 1) % 3]; try { localStorage.setItem("theme", t); } catch { /* ignore */ } apply(); };
  apply();
}

async function main() {
  themeToggle();
  const r = await api("/api/csrf");
  csrf = (await r.json()).CSRF;
  await refresh();
  connect();
  setInterval(() => { if (view && view.step === "drive") doCmd("refresh"); }, 2000); // a card inserted shows up by itself
}
main();
