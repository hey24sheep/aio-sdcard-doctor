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
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid).replace(reorder, " ")));
  }
  return el;
}

// Invisible marks that reorder or hide text (bidi overrides and isolates, LRM/RLM, zero-width
// space, line/paragraph separators, the format marks U+2060-206F, tag characters): a crafted
// card name could show "photo.exe" as "photo.jpg". Same set the terminal UI removes.
const reorder = /[\u200b\u200e\u200f\u2028\u2029\u202a-\u202e\u2060-\u206f\u061c\ufeff\u{e0000}-\u{e007f}]/gu;

// ---- icons: line drawings on a 24 px grid, drawn with the text colour; always beside words ----

const SVG = "http://www.w3.org/2000/svg";
const ICONS = {
  sd: ["M8 3h7l4 4v13a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V6z", "M9 7v3M12 7v3M15 8v2"],
  drive: ["M3 14h18v5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1z", "M3 14l2.5-8h13l2.5 8", "M7 17h.01"],
  copy: ["M7 3h7l5 5v12a1 1 0 0 1-1 1H7a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z", "M14 3v5h5", "M12.5 11v6M9.5 14l3 3 3-3"],
  undo: ["M4 12a8 8 0 1 0 2.3-5.7", "M4 4v4h4", "M12 8v4l3 2"],
  disk: ["M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3z", "M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6", "M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"],
  shield: ["M12 3l7 3v5.5c0 4.4-3 7.8-7 9.5-4-1.7-7-5.1-7-9.5V6z", "M9 12l2 2 4-4.5"],
  eye: ["M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z", "M12 9.2a2.8 2.8 0 1 0 0 5.6 2.8 2.8 0 0 0 0-5.6z"],
  tool: ["M14.7 6.3a4 4 0 0 0-5.3 5.2L3.5 17.4a1.5 1.5 0 0 0 2.1 2.1l5.9-5.9a4 4 0 0 0 5.2-5.3l-2.4 2.4-2.1-.4-.4-2.1z"],
  folder: ["M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"],
  file: ["M7 3h7l5 5v12a1 1 0 0 1-1 1H7a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z", "M14 3v5h5", "M9 13h6M9 17h4"],
  back: ["M15 18l-6-6 6-6"],
  up: ["M12 19V6M6 11l6-6 6 6"],
  play: ["M8 5.5v13l10.5-6.5z"],
  stop: ["M7 7h10v10H7z"],
  restart: ["M4 12a8 8 0 1 0 2.4-5.7L4 8.5", "M4 4v4.5h4.5"],
  search: ["M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14z", "M20 20l-4-4"],
  check: ["M5 12.5l4.5 4.5L19 7.5"],
  warn: ["M12 3.5l9.5 16.5h-19z", "M12 10v4.5", "M12 17.3v.2"],
  info: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z", "M12 11v5.5", "M12 7.8v.2"],
  x: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z", "M9 9l6 6M15 9l-6 6"],
  ok: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z", "M8 12.5l3 3 5-6"],
  sun: ["M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8z", "M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M5.3 18.7l1.4-1.4M17.3 6.7l1.4-1.4"],
  moon: ["M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z"],
  auto: ["M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18z", { solid: "M12 3v18a9 9 0 0 0 0-18z" }],
};
function icon(name) {
  const svg = document.createElementNS(SVG, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("class", "icon");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  for (const d of ICONS[name] || []) {
    const p = document.createElementNS(SVG, "path");
    if (typeof d === "string") p.setAttribute("d", d);
    else { p.setAttribute("d", d.solid); p.setAttribute("class", "solid"); }
    svg.append(p);
  }
  return svg;
}
const actionIcon = { rescue: "copy", recover: "undo", image: "disk", verify: "shield", scan: "eye", fix: "tool" };

function gb(n) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GB";
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  return Math.ceil(n / 1024) + " KB";
}
const commas = (n) => Number(n || 0).toLocaleString("en");
const files = (n) => (n === 1 ? "1 file" : commas(n) + " files");

let gone = false;
const stopOnGone = []; // the event stream and timers: a closed program isn't asked again every 2 s

async function api(path, opts = {}) {
  if (gone) throw new Error("gone");
  let r;
  try {
    r = await fetch(path, { credentials: "same-origin", ...opts });
  } catch (e) { // the program was closed: say so instead of a page that silently stops working
    showGone(true);
    throw e;
  }
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
  main.prepend(h("div", { class: "note error flash", role: "alert" }, icon("x"), h("div", {}, h("span", { class: "kind" }, "Not done: "), msg)));
}

function showGone(closed) {
  if (gone) return;
  gone = true;
  stopOnGone.forEach((stop) => stop());
  document.body.replaceChildren(h("main", { id: "main", class: "layout" },
    h("div", {}, h("h1", {}, closed ? "SD card doctor was closed" : "This window is no longer connected"),
      h("p", {}, closed
        ? "Run aio-sdcard-doctor gui to start it again. A rescue or image that was interrupted picks up where it stopped when you run it again."
        : "Run aio-sdcard-doctor gui again: it reopens the running copy, and anything that was working carries on."))));
}

// One /api/view at a time, and at most one more after it: an older view can never be drawn over
// a newer one (a command's answer and an event's arriving out of order), and a burst of log
// lines costs two fetches, not one each.
let viewing = null;
let viewAgain = false;
function refresh() {
  if (viewing) { viewAgain = true; return viewing; }
  viewing = (async () => {
    try {
      do {
        viewAgain = false;
        const r = await api("/api/view");
        const d = await r.json();
        view = d.View;
        render();
      } while (viewAgain);
    } finally { viewing = null; }
  })();
  return viewing;
}

// ---- frame ------------------------------------------------------------------------------------

const stepNames = {
  drive: "Card", action: "What to do", choice: "What to copy", dest: "Where", options: "Options",
  review: "Check", elevate: "Administrator", run: "Working", "recover-list": "Pick files", results: "Done",
};

function renderFrame() {
  const c = view.card;
  const id = $("card-id");
  if (c) { // the card being read, always in sight: a chip with its facts
    const facts = [c.FS, c.Total ? gb(c.Total) : "", c.Removable ? "removable" : "", c.Root].filter(Boolean);
    const writes = view.req.action === "fix" && view.req.fixOp !== "check"; // repair, format, health test
    id.replaceChildren(h("span", { class: "chip" }, icon(c.Removable ? "sd" : "drive"), h("strong", {}, c.Label || c.Root),
      h("span", { class: "facts" }, facts.map((f) => h("span", {}, h("span", { class: "sr-only" }, ", "), f))),
      writes ? h("span", { class: "badge warn" }, icon("warn"), "Card tools write to the card")
        : h("span", { class: "badge good" }, icon("eye"), "Only read")));
  } else id.textContent = "No card chosen";

  let steps = view.steps && view.steps.length > 2 ? view.steps : ["drive", "action"];
  if (view.req.action === "recover" && steps.includes("run")) { // the pick list: after the listing, before the files come back
    steps = [...steps];
    steps.splice(view.req.list ? steps.indexOf("run") + 1 : steps.indexOf("review"), 0, "recover-list");
  }
  const at = steps.indexOf(view.step);
  $("steps").replaceChildren(...steps.map((s, i) =>
    h("li", { "aria-current": s === view.step ? "step" : false, class: i < at ? "done" : false },
      h("span", { class: "dot", "aria-hidden": "true" }, i < at ? icon("check") : String(i + 1)),
      h("span", { class: "name" }, stepNames[s] || s),
      i < at ? h("span", { class: "sr-only" }, " (done)") : null)));
  // phones: one line instead of the row of circles (the list above is still what is read out)
  $("step-compact").replaceChildren(
    h("div", { class: "where" }, h("b", {}, stepNames[view.step] || view.step), at >= 0 ? h("span", {}, `Step ${at + 1} of ${steps.length}`) : null),
    h("div", { class: "line" }, steps.map((s, i) => h("span", { class: i <= at ? "on" : false }))));

  const p = view.progress;
  if (view.step === "run" && p && p.Total) {
    document.title = `${Math.floor((100 * p.Done) / p.Total)}% · ${view.runLabel || "Working"}`;
  } else if (view.step === "results") {
    document.title = "Done — SD card doctor";
  } else document.title = "SD card doctor";
}

// ---- shared pieces ------------------------------------------------------------------------------

function errorNote() {
  return view.error ? h("div", { class: "note error", role: "alert" }, icon("x"), h("div", {}, h("span", { class: "kind" }, "Problem: "), view.error)) : null;
}

function busyNote() {
  return view.busy ? h("p", { class: "note busy", role: "status" }, h("span", { class: "spin", "aria-hidden": "true" }), h("span", {}, view.busy + "…")) : null;
}

const backBtn = () => h("button", { type: "button", onclick: () => doCmd("back") }, icon("back"), "Back");
const spacer = () => h("span", { class: "spacer" });

// ---- steps ------------------------------------------------------------------------------------

function driveTile(d) {
  const badges = [];
  if (d.Removable) badges.push(h("span", { class: "badge good" }, "Removable"));
  if (d.System) badges.push(h("span", { class: "badge warn" }, "System"));
  if (d.Locked) badges.push(h("span", { class: "badge bad" }, "Locked"));
  if (d.WriteProtected) badges.push(h("span", { class: "badge" }, "Write-protected"));
  if (d.FS === "not responding") badges.push(h("span", { class: "badge bad" }, "Not responding"));
  else if (d.RAW) badges.push(h("span", { class: "badge warn" }, "File system not readable by the computer"));
  const used = d.Total ? Math.round((100 * (d.Total - d.Free)) / d.Total) : 0;
  return h("button", { class: "tile", type: "button", onclick: () => doCmd("select-drive", { root: d.Root }) },
    h("span", { class: "ico" }, icon(d.Removable ? "sd" : "drive")),
    h("span", { class: "title" }, d.Label || d.Root),
    h("span", { class: "desc" }, [d.Root, d.FS, d.Total ? gb(d.Total) : ""].filter(Boolean).join(", ")),
    d.Total ? h("span", { class: "meter", role: "img", "aria-label": `${used}% used` }, h("span", { style: `width:${used}%` })) : null,
    badges.length ? h("span", { class: "badges" }, badges) : null);
}

function stepDrive() {
  const cards = view.drives.filter((d) => d.Removable);
  const others = view.drives.filter((d) => !d.Removable);
  let path = drafts.path || "";
  const input = h("input", { type: "text", id: "path", autocomplete: "off", value: path, placeholder: "e.g. D:\\card.img or D:\\copy of the card",
    oninput: (e) => (drafts.path = path = e.target.value),
    onkeydown: (e) => { if (e.key === "Enter" && !e.isComposing) doCmd("select-path", { path }); } }); // Enter = Use this
  return [
    h("h1", {}, "Choose the card"),
    h("p", { class: "lead" }, "Nothing on the card is changed unless you choose Card tools later and confirm."),
    cards.length ? h("div", { class: "grid" }, cards.map(driveTile))
      : h("p", { class: "note busy", role: "status" }, h("span", { class: "spin", "aria-hidden": "true" }), h("span", {}, "Insert the card — it will appear here automatically.")),
    others.length ? [h("h2", { class: "section-label" }, "Other drives"), h("div", { class: "grid" }, others.map(driveTile))] : null,
    view.hiddenSystem ? h("button", { type: "button", class: "link", onclick: () => doCmd("show-system") },
      `Show system drives (${view.hiddenSystem} hidden)`) : null,
    h("h2", {}, "Or use a folder or a card image file"),
    h("label", { for: "path" }, "Folder or image file"),
    h("div", { class: "field" }, icon("folder"), input),
    h("div", { class: "actions-row plain" }, h("button", { type: "button", onclick: () => doCmd("select-path", { path }) }, "Use this")),
  ];
}

function stepAction() {
  const acts = view.actions || [];
  const tile = (a) => h("button", { type: "button", class: "tile" + (a.danger ? " danger-tile" : ""), disabled: !a.enabled,
    "aria-describedby": a.reason ? `why-${a.name}` : false, onclick: () => doCmd("choose-action", { action: a.name }) },
    h("span", { class: "ico" }, icon(actionIcon[a.name] || "file")),
    h("span", { class: "title" }, a.label),
    h("span", { class: "desc" }, a.desc + (a.admin ? " (asks for administrator rights)" : "")),
    a.reason ? h("span", { class: "why", id: `why-${a.name}` }, a.reason) : null);
  const rescue = acts.find((a) => a.name === "rescue");
  return [
    h("h1", {}, "What do you want to do?"),
    rescue && rescue.enabled ? h("button", { type: "button", class: "primary quick", onclick: () => doCmd("quick-rescue") },
      h("span", { class: "ico" }, icon("copy")),
      h("span", { class: "title" }, "Copy everything off this card"),
      h("span", { class: "desc" }, "Every file, nothing skipped — you'll check the details before it starts")) : null,
    h("h2", { class: "section-label" }, "Or choose"),
    h("div", { class: "grid two" }, acts.filter((a) => !a.danger).map(tile)),
    h("section", { class: "tools", "aria-labelledby": "tools-h" },
      h("h2", { id: "tools-h" }, icon("warn"), "Card tools — these change the card"),
      h("p", { class: "small" }, "Repair, format or test the card. Copy your files off first."),
      h("div", { class: "grid two" }, acts.filter((a) => a.danger).map(tile))),
    h("div", { class: "actions-row" }, backBtn()),
  ];
}

function stepChoice() {
  let picked = view.req.backup || "standard";
  const rows = view.choices || [];
  return [
    h("h1", {}, "What should be copied?"),
    h("p", { class: "lead" }, "Files left out are still listed in the report, with the reason."),
    busyNote(),
    rows.length ? h("ul", { class: "choice-list", role: "radiogroup", "aria-label": "What to copy" }, rows.map((c) =>
      h("li", {}, h("label", { class: "check" },
        h("input", { type: "radio", name: "choice", value: c.id, checked: c.id === picked, onchange: () => (picked = c.id) }),
        h("span", {}, h("strong", {}, c.title), h("span", { class: "hint" }, c.desc)),
        h("span", { class: "counts" }, h("b", {}, files(c.files)), gb(c.bytes)))))) : null,
    h("div", { class: "actions-row" }, backBtn(), spacer(),
      h("button", { type: "button", class: "primary", disabled: !rows.length, onclick: () => doCmd("set-choice", { id: picked }) }, "Continue")),
  ];
}

function folderBrowser(onPick) {
  const box = h("div", { class: "folder", role: "group", "aria-label": "Choose a folder" });
  async function show(path) {
    const r = await api("/api/dirs?path=" + encodeURIComponent(path));
    const d = await r.json();
    if (d.error) { flash(d.error); return; }
    const dirs = d.Dirs || [];
    const list = h("ul", {}, dirs.map((name) => {
      const full = d.Path ? (d.Path.endsWith("\\") || d.Path.endsWith("/") ? d.Path + name : d.Path + (d.Path.includes("\\") ? "\\" : "/") + name) : name;
      return h("li", {}, h("button", { type: "button", onclick: () => show(full) }, icon(d.Path ? "folder" : "drive"), name));
    }), dirs.length ? null : h("li", { class: "empty" }, "No folders inside"));
    box.replaceChildren(h("div", { class: "head" },
      h("button", { type: "button", disabled: !d.Path, onclick: () => show(d.Parent || "") }, icon("up"), "Up"),
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
    oninput: (e) => (drafts.dest = path = e.target.value),
    onkeydown: (e) => { if (e.key === "Enter" && !e.isComposing) doCmd("set-dest", { path }); } }); // Enter = Continue
  return [
    h("h1", {}, isImage ? "Where should the image file go?" : "Where should the files go?"),
    h("p", { class: "lead" }, isImage ? "Choose a file name on another disk, e.g. D:\\card.img." :
      "Choose a folder on another disk — never on the card itself."),
    h("label", { for: "dest" }, isImage ? "Image file" : "Folder"),
    h("div", { class: "field" }, icon(isImage ? "disk" : "folder"), input),
    !isImage ? h("details", { open: !!drafts.browse, ontoggle: (e) => (drafts.browse = e.target.open) },
      h("summary", {}, "Browse folders on this computer"),
      folderBrowser((p) => { drafts.dest = path = p; input.value = p; })) : null,
    h("div", { class: "actions-row" }, backBtn(), spacer(),
      h("button", { type: "button", class: "primary", onclick: () => doCmd("set-dest", { path }) }, "Continue")),
  ];
}

function tick(name, label, hint, on, locked) {
  const id = "opt-" + name;
  return h("label", { class: "check", for: id },
    h("input", { type: "checkbox", id, checked: on, disabled: !!locked, "aria-describedby": locked ? "undelete-why" : false,
      onchange: (e) => doCmd("set-option", { name, value: e.target.checked ? "1" : "0" }) }),
    h("span", {}, h("span", { class: "what" }, label), hint ? h("span", { class: "hint" }, hint) : null));
}

function stepOptions() {
  const r = view.req;
  const body = [];
  if (r.action === "rescue") {
    body.push(h("div", { class: "opts" },
      tick("compress", "Compress (max) into ZIP files", "Saves little on photos and videos; helps documents and RAW", r.compress),
      tick("skip-empty", "Leave out empty (0-byte) files", "They are still listed in the report", r.skipEmpty)));
  } else if (r.action === "image") {
    body.push(h("div", { class: "opts" }, tick("compress", "Compress the image (card.img.xz) when finished", "Needs room for both until it's checked", r.compress)));
  } else if (r.action === "recover") {
    const u = view.undelete;
    if (u) body.push(h("div", { class: "note", id: "undelete-why" }, icon("info"), h("div", {}, h("span", { class: "kind" }, "Undelete isn't available on this card: "), u.Reason,
      h("span", { class: "detail" }, "Deep scan is on — it's the only way to find deleted files here."))));
    body.push(h("div", { class: "opts" },
      tick("deep", "Deep scan", "Slower: also finds files whose names are gone", r.deep, !!u),
      tick("thorough", "Thorough", "Much slower: looks at every byte, not only sector starts", r.thorough),
      tick("compress", "Compress into ZIP files", "", r.compress)));
  } else if (r.action === "verify") {
    let m = r.manifest || "", t = r.target || "";
    body.push(h("label", { for: "manifest" }, "The rescue to check (its Reports\\manifest.json)"),
      h("div", { class: "field" }, icon("file"), h("input", { type: "text", id: "manifest", value: m, oninput: (e) => (m = e.target.value),
        onchange: () => doCmd("set-option", { name: "manifest", value: m }) })),
      h("label", { for: "target" }, "Where the files are now (leave empty for the rescue folder)"),
      h("div", { class: "field" }, icon("folder"), h("input", { type: "text", id: "target", value: t, oninput: (e) => (t = e.target.value),
        onchange: () => doCmd("set-option", { name: "target", value: t }) })));
  } else if (r.action === "fix") {
    // unavailable tools stay listed, greyed, with why (never hidden)
    body.push(busyNote(), h("ul", { class: "choice-list", role: "radiogroup", "aria-label": "Card tool" }, (view.tools || []).map((t) =>
      h("li", {}, h("label", { class: "check", for: "fixop-" + t.name },
        h("input", { type: "radio", name: "fixop", id: "fixop-" + t.name, value: t.name, checked: r.fixOp === t.name, disabled: !t.enabled,
          "aria-describedby": t.enabled ? false : "fixop-why-" + t.name,
          onchange: () => doCmd("set-option", { name: "fix-op", value: t.name }) }),
        h("span", {}, h("strong", {}, t.label), h("span", { class: "hint" + (t.danger ? " danger-hint" : "") }, t.desc),
          t.enabled ? null : h("span", { class: "why", id: "fixop-why-" + t.name }, "Not available: " + t.reason)))))));
  }
  return [
    h("h1", {}, "Options"),
    body.length ? body : h("p", {}, "Nothing to set for this one."),
    h("div", { class: "actions-row" }, backBtn(), spacer(),
      h("button", { type: "button", class: "primary", onclick: () => doCmd("next") }, "Continue")),
  ];
}

// planRows reads the plan's summary lines ("From: …", "To:   …", an indented line continuing the
// one above) as label/value rows; a line without a short label is shown whole.
function planRows(lines) {
  const rows = [];
  for (const l of lines) {
    const m = /^([A-Z][A-Za-z ]{0,13}):\s+(\S.*)$/.exec(l);
    if (/^\s/.test(l) && rows.length) rows[rows.length - 1].more.push(l.trim());
    else if (m) rows.push({ k: m[1], v: m[2], more: [] });
    else rows.push({ k: "", v: l, more: [] });
  }
  return rows;
}

function planBlock(lines) {
  const rows = planRows(lines);
  return h("div", { class: "plan" }, h("dl", {}, rows.map((r) => [
    h("dt", {}, r.k || h("span", { class: "sr-only" }, "Also")),
    h("dd", {}, r.v, r.more.map((m) => h("span", { class: "more" }, m)))])));
}

function stepReview() {
  if (view.busy || !view.plan) return [h("h1", {}, "Please check before it starts"), busyNote(), errorNote(),
    h("div", { class: "actions-row" }, backBtn())];
  const p = view.plan;
  const issues = p.Issues || [];
  const blocks = issues.filter((i) => i.Severity === "blocked" && i.Kind !== "needs-admin");
  const warns = issues.filter((i) => i.Severity === "warning");
  const notes = issues.filter((i) => i.Severity !== "blocked" && i.Severity !== "warning");
  const hasDest = ["rescue", "image", "recover"].includes(view.req.action);
  // what was typed or ticked survives a refused Start (the page re-renders with the reason)
  const start = h("button", { type: "button", class: view.confirm ? "danger" : "primary", disabled: blocks.length > 0,
    onclick: () => doCmd("start", { accept: drafts.accept ? "1" : "", label: drafts.clabel || "", size: drafts.csize || "" }) },
    icon(view.confirm ? "tool" : "play"), view.confirm ? "Change the card" : "Start");
  const issue = (i, kind, cls) => h("div", { class: "note " + cls, role: cls === "block" ? "alert" : false },
    icon(cls === "block" ? "x" : cls === "warn" ? "warn" : "info"),
    h("div", {}, h("span", { class: "kind" }, kind + ": "), i.Title, i.Detail ? h("span", { class: "detail" }, i.Detail) : null));
  return [
    h("h1", {}, "Please check before it starts"),
    planBlock(p.Summary || []),
    p.Result && p.Result.length ? [h("h2", {}, "You'll get"), h("ul", { class: "gets" }, p.Result.map((l) => h("li", {}, icon("check"), h("span", {}, l))))] : null,
    blocks.map((i) => issue(i, "Can't start", "block")),
    warns.map((i) => issue(i, "Warning", "warn")),
    notes.map((i) => issue(i, "Note", "")),
    view.canStartOver || warns.length ? h("div", { class: "opts" },
      view.canStartOver ? h("label", { class: "check", for: "fresh" },
        h("input", { type: "checkbox", id: "fresh", checked: !!view.req.fresh,
          onchange: (e) => doCmd("set-option", { name: "fresh", value: e.target.checked ? "1" : "0" }) }),
        h("span", {}, "Start over instead of continuing where it stopped")) : null,
      warns.length ? h("label", { class: "check", for: "accept" },
        h("input", { type: "checkbox", id: "accept", checked: !!drafts.accept, onchange: (e) => (drafts.accept = e.target.checked) }),
        h("span", {}, "I've read the warnings above and want to go ahead")) : null) : null,
    view.confirm ? h("div", { class: "danger-box" },
      h("h2", {}, "Type the card's label and size to confirm"),
      h("p", {}, "This changes the card. Type ", h("strong", {}, view.confirm.label), " and ", h("strong", {}, view.confirm.size), " exactly as shown."),
      h("div", { class: "pair" },
        h("div", {}, h("label", { for: "c-label" }, "Label"), h("input", { type: "text", id: "c-label", autocomplete: "off", value: drafts.clabel || "", oninput: (e) => (drafts.clabel = e.target.value) })),
        h("div", {}, h("label", { for: "c-size" }, "Size"), h("input", { type: "text", id: "c-size", autocomplete: "off", value: drafts.csize || "", oninput: (e) => (drafts.csize = e.target.value) })))) : null,
    errorNote(),
    h("div", { class: "actions-row" }, backBtn(), spacer(),
      hasDest ? h("button", { type: "button", onclick: () => doCmd("edit-dest") }, "Change where the files go") : null, start),
  ];
}

function stepElevate() {
  return [
    h("h1", {}, "This needs administrator rights"),
    h("p", { class: "lead" }, "Reading the card directly needs them. Close this window and run:"),
    h("p", { class: "note" }, icon("shield"), h("span", { class: "mono" }, view.instruction)),
    h("div", { class: "actions-row" }, backBtn()),
  ];
}

// trace is the read speed over the last minute as an area: dips (a damaged area) stand out.
function trace() {
  const max = Math.max(1, ...speeds);
  const top = max * 1.15;
  const n = speeds.length;
  const x = (i) => ((i / Math.max(1, n - 1)) * 300).toFixed(1);
  const y = (v) => (72 - (v / top) * 72).toFixed(1);
  const line = speeds.map((v, i) => `${i ? "L" : "M"}${x(i)},${y(v)}`).join("");
  const last = n ? speeds[n - 1] : 0;
  const svg = document.createElementNS(SVG, "svg");
  svg.setAttribute("viewBox", "0 0 300 72");
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("class", "trace");
  svg.setAttribute("aria-hidden", "true");
  for (const [cls, d] of [["area", line + "L300,72L0,72Z"], ["line", line], ["base", "M0,72L300,72"]]) {
    const p = document.createElementNS(SVG, "path");
    p.setAttribute("class", cls);
    p.setAttribute("d", d);
    svg.append(p);
  }
  return h("figure", { class: "speed" }, svg,
    h("figcaption", {}, `Read speed: ${(last / 1e6).toFixed(1)} MB/s now, ${(max / 1e6).toFixed(1)} MB/s best`));
}

// timeLeft is a rough estimate from the recent speed (the last ~10 updates), never a promise.
function timeLeft(p) {
  if (!p.Total || p.Done >= p.Total || speeds.length < 5) return "";
  const recent = speeds.slice(-10);
  const bps = recent.reduce((a, b) => a + b, 0) / recent.length;
  if (bps <= 0) return "";
  const min = Math.round((p.Total - p.Done) / bps / 60);
  if (min < 1) return "less than a minute left";
  if (min < 90) return `about ${min} min left`;
  return `about ${Math.floor(min / 60)} h ${min % 60} min left`;
}

const goodStates = ["OK", "ALREADY-COPIED", "DELETED-RECOVERED", "CARVED", "KEPT-FROM-EARLIER-RUN"];
const warnStates = ["PARTIAL", "CARVED-DAMAGED", "STALLED", "LOCKED"];
const badStates = ["FAILED", "MISSING", "WRONG-SIZE", "CORRUPT", "UNREADABLE", "DELETED-UNRECOVERABLE"];
const rank = (k) => ({ good: 0, warn: 1, bad: 2 })[tone(k)] ?? 3; // what was saved first, then what needs a look
const tone = (k) => (goodStates.includes(k) ? "good" : warnStates.includes(k) ? "warn" : badStates.includes(k) ? "bad" : "");

// runLive is the part of the run screen that progress updates replace; nothing focusable is in it,
// so keyboard focus (on Stop) is never lost to a progress tick.
function runLive() {
  const p = view.progress || {};
  const pct = p.Total ? Math.floor((100 * p.Done) / p.Total) : 0;
  const c = p.Counters || {};
  const safe = (c.OK || 0) + (c["ALREADY-COPIED"] || 0) + (c["DELETED-RECOVERED"] || 0) + (c.CARVED || 0);
  const total = p.ItemsTotal || (view.plan && view.plan.Items) || 0;
  const hero = p.Counters ? `${commas(safe)}${total ? " of " + commas(total) : ""} files safe so far` : "done";
  const problems = Object.entries(c).filter(([k, v]) => v > 0 && (warnStates.includes(k) || badStates.includes(k)));
  const left = timeLeft(p);
  return [
    h("div", { class: "instrument" },
      h("div", { class: "readout" },
        h("span", { class: "big" }, String(pct), h("small", {}, "%")),
        h("p", { class: "hero" }, hero),
        left ? h("p", { class: "left" }, left) : null),
      h("div", { class: "bar", role: "progressbar", "aria-valuemin": "0", "aria-valuemax": "100", "aria-valuenow": String(pct), "aria-label": "Progress" },
        h("span", { style: `width:${pct}%` })),
      h("dl", { class: "stats" },
        p.Total ? h("div", {}, h("dt", {}, "Done"), h("dd", {}, gb(p.Done), h("span", { class: "of" }, ` of ${gb(p.Total)}`))) : null,
        p.BPS ? h("div", {}, h("dt", {}, "Speed"), h("dd", {}, `${(p.BPS / 1e6).toFixed(1)} MB/s`)) : null,
        p.Counters ? h("div", {}, h("dt", {}, "Problems"), h("dd", { class: problems.some(([k]) => badStates.includes(k)) ? "bad" : problems.length ? "warn" : false },
          problems.length ? problems.map(([k, v]) => `${commas(v)} ${k}`).join(", ") : "none")) : null),
      speeds.length > 1 ? trace() : null,
      p.Item ? h("p", { class: "current" }, h("span", {}, "Now"), h("span", { class: "mono" }, p.Item)) : null),
    p.Note ? h("p", { class: "note warn" }, icon("warn"), h("span", {}, p.Note)) : null,
  ];
}

function stepRun() {
  return [
    // Stop sits beside the title: always in sight, whatever the log below grows to
    h("div", { class: "runhead" }, h("h1", {}, view.runLabel || "Working"),
      h("button", { type: "button", id: "stop", onclick: () => doCmd("cancel") }, icon("stop"), "Stop")),
    h("div", { id: "run-live" }, runLive()),
    view.logs && view.logs.length ? [h("h2", { class: "minor" }, "Log"), h("div", { class: "log" }, view.logs.slice(-200).map((l) => h("div", {}, l)))] : null,
  ];
}

function stepRecoverList() {
  const rv = view.recover || { rows: [], total: 0, shown: 0, ticked: 0 };
  let filter = rv.filter || "";
  const states = { Excellent: "Excellent", Poor: "Poor", Unrecoverable: "Unrecoverable" };
  return [
    h("h1", {}, "Pick the files to bring back"),
    h("p", { class: "tally-line" }, h("span", {}, h("b", {}, commas(rv.total)), " found"), h("span", {}, h("b", {}, commas(rv.shown)), " match"),
      h("span", {}, h("b", {}, commas(rv.ticked)), " ticked")),
    rv.listed < rv.shown ? h("p", { class: "small muted" }, `The first ${commas(rv.listed)} are listed: filter to narrow, or tick all ${commas(rv.shown)}.`) : null,
    h("p", { class: "small muted" }, "Unrecoverable files are listed with the reason and can't be ticked."),
    h("div", { class: "filterbar" },
      h("div", { class: "grow" }, h("label", { for: "filter" }, "Filter by name or folder"),
        h("div", { class: "field" }, icon("search"), h("input", { type: "text", id: "filter", value: filter, oninput: (e) => (filter = e.target.value),
          onchange: () => doCmd("recover-filter", { text: filter }) }))),
      h("button", { type: "button", onclick: () => doCmd("recover-tick-all") }, icon("check"), "Tick all Excellent and Poor")),
    h("div", { class: "table-wrap" }, h("table", { class: "recover-table" },
      h("caption", { class: "sr-only" }, "Files that can be recovered"),
      h("thead", {}, h("tr", {}, ["Pick", "State", "Name", "Folder", "Size", "Modified", "Comment"].map((t, i) =>
        h("th", { scope: "col", class: i === 4 ? "num" : false }, t)))),
      h("tbody", {}, rv.rows.map((r, i) => h("tr", {},
        h("td", {}, h("label", { class: "pick", for: `r${i}` }, h("input", { type: "checkbox", id: `r${i}`, checked: r.ticked, disabled: !r.tickable, "aria-label": `Pick ${r.name}`,
          onchange: () => doCmd("recover-tick", { id: r.id }) }))),
        h("td", {}, h("span", { class: "state " + r.state }, states[r.state] || r.state)),
        h("td", {}, h("strong", {}, r.name)), h("td", { class: "mono" }, r.path), h("td", { class: "num" }, gb(r.size)),
        h("td", {}, r.modified || ""), h("td", { class: "small" }, r.comment)))))),
    h("div", { class: "actions-row" },
      h("button", { type: "button", onclick: () => doCmd("restart") }, icon("restart"), "Start over"), spacer(),
      h("button", { type: "button", class: "primary", disabled: !rv.ticked, onclick: () => doCmd("recover-start") }, icon("undo"), `Recover ${files(rv.ticked)}`)),
  ];
}

function stepResults() {
  const res = view.result || { Counters: {} };
  const counters = Object.entries(res.Counters || {});
  const title = view.imageInstead ? "Stopped to make a full backup image instead"
    : view.error ? "Stopped" : res.Clean ? "Done" : "Done, with problems";
  const mood = view.imageInstead ? "info" : view.error ? "bad" : res.Clean ? "good" : "warn";
  const c = res.Counters || {};
  const safe = goodStates.reduce((n, k) => n + (c[k] || 0), 0);
  const all = counters.reduce((n, [, v]) => n + v, 0);
  // a stopped run's counts cover only the files it got to: no "of" total then
  const saved = ["rescue", "recover"].includes(view.req.action) && all && !view.error && !view.imageInstead ? `${commas(safe)} of ${files(all)} safe` : "";
  return [
    h("div", { class: "verdict " + mood },
      h("span", { class: "ico" }, icon({ info: "disk", bad: "x", good: "check", warn: "warn" }[mood])),
      h("h1", {}, title), saved ? h("p", {}, saved) : null,
      view.error ? h("p", { role: "alert" }, h("span", { class: "kind" }, "Problem: "), view.error) : null),
    counters.length ? h("ul", { class: "tally", "aria-label": "What happened to each file" },
      counters.sort(([a], [b]) => rank(a) - rank(b)).map(([k, v]) => h("li", { class: tone(k) || false }, h("b", {}, commas(v)), h("span", {}, k)))) : null,
    view.next && view.next.length ? [h("h2", {}, "What next"), h("div", { class: "grid two" }, view.next.map((n) =>
      h("button", { type: "button", class: "tile", onclick: () => doCmd("next-action", { action: n.action }) },
        h("span", { class: "ico" }, icon(actionIcon[n.action] || "file")), h("span", { class: "title" }, n.label), h("span", { class: "desc" }, n.why))))] : null,
    res.Reports && res.Reports.length ? [h("h2", {}, "Reports"), h("ul", { class: "reports" }, res.Reports.filter(Boolean).map((r) => h("li", {}, icon("file"), h("span", { class: "mono" }, r))))] : null,
    view.logs && view.logs.length ? [h("h2", { class: "minor" }, "Details"), h("div", { class: "log" }, view.logs.map((l) => h("div", {}, l)))] : null,
    h("div", { class: "actions-row" },
      view.req.dest ? h("button", { type: "button", onclick: () => api("/api/open-dest", { method: "POST", headers: { "X-CSRF": csrf } }) }, icon("folder"), "Open the folder") : null,
      h("button", { type: "button", onclick: () => doCmd("restart") }, icon("restart"), "Start over")),
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
  // and so does the caret in a text box: the card list refreshes every 2 s while someone types
  const caret = ae && ae.tagName === "INPUT" && ae.type === "text"
    ? { value: ae.value, start: ae.selectionStart, end: ae.selectionEnd, dir: ae.selectionDirection, scroll: ae.scrollLeft } : null;
  const stepChanged = main.dataset.step !== view.step;
  if (drafts.action !== view.req.action) { // a new flow: nothing typed for the last one carries over
    for (const k of Object.keys(drafts)) if (k !== "path") delete drafts[k];
    drafts.action = view.req.action;
  }
  if (stepChanged) { // a confirmation is for the check it was typed on, never the next one
    delete drafts.accept; delete drafts.clabel; delete drafts.csize;
  }
  if (view.plan) { // checked again on the same page (Start over): new warnings need a new tick
    const w = JSON.stringify((view.plan.Issues || []).filter((i) => i.Severity === "warning"));
    if (w !== drafts.warns) { delete drafts.accept; drafts.warns = w; }
  }
  // the log keeps its place: newest lines in view, unless the person scrolled up to read
  const oldLog = main.querySelector(".log");
  const logAtEnd = !oldLog || oldLog.scrollHeight - oldLog.scrollTop - oldLog.clientHeight < 8;
  const logTop = oldLog ? oldLog.scrollTop : 0;
  main.dataset.step = view.step;
  document.body.dataset.step = view.step; // the pick list takes the whole width
  main.replaceChildren(...[view.step !== "review" && view.step !== "results" ? errorNote() : null, ...(fn ? fn() : [])].flat(Infinity).filter(Boolean));
  const [ht, hp] = help[view.step] || ["", ""];
  $("side").replaceChildren(h("div", { class: "help" }, h("h2", {}, ht), h("p", {}, hp)));
  const log = main.querySelector(".log");
  if (log) log.scrollTop = stepChanged || logAtEnd ? log.scrollHeight : logTop;
  if (stepChanged) {
    const h1 = main.querySelector("h1");
    if (h1) { h1.tabIndex = -1; h1.focus(); }
  } else if (keepFocus.startsWith("#") && $(keepFocus.slice(1))) {
    const el = $(keepFocus.slice(1));
    if (caret && el.tagName === "INPUT") {
      if (el.value !== caret.value) { // what was typed, even if the page was drawn from an older copy
        el.value = caret.value;
        el.dispatchEvent(new Event("input")); // and the box's own handler keeps the same text
      }
      el.setSelectionRange(caret.start, caret.end, caret.dir || "none");
    }
    el.focus();
    if (caret && el.tagName === "INPUT") el.scrollLeft = caret.scroll;
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
  dlg.replaceChildren(h("div", { class: "ask-head" }, h("span", { class: "ico" }, icon(is.Severity === "info" ? "info" : "warn")),
    h("div", {}, h("h2", { id: "ask-title" }, is.Title), is.Detail ? h("p", {}, is.Detail) : null)),
    h("div", { class: "actions-row" }, (is.Choices || []).map((c) => h("button", { type: "button", class: c === is.Default ? "primary" : false,
      onclick: () => { dlg.close(); doCmd("answer", { id: iv.id, choice: c }); } }, choiceLabel(c)))));
  dlg.oncancel = (e) => { e.preventDefault(); dlg.close(); doCmd("answer", { id: iv.id, choice: safe }); }; // Esc = the safe choice
  dlg.showModal();
  const def = [...dlg.querySelectorAll("button")].find((b) => b.classList.contains("primary"));
  if (def) def.focus(); // Enter answers with the default (the highlighted one), not the first button
  $("live").textContent = is.Title;
}

const labels = { continue: "Continue", stop: "Stop", retry: "Try again", wait: "Wait for the card", "skip-rest": "Skip the rest (report them failed)", skip: "Skip this file",
  resume: "Continue where it stopped", new: "Start over", elevate: "Restart as administrator", "skip-videos": "Skip videos", image: "Make a full backup image instead" };
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
  const t = setInterval(() => { if (view && (view.step === "run" || view.issue)) refresh(); }, 5000); // safety net
  stopOnGone.push(() => es.close(), () => clearInterval(t));
}

function themeToggle() {
  const btn = $("theme");
  const order = ["auto", "light", "dark"];
  let t = "auto";
  try { t = localStorage.getItem("theme") || "auto"; } catch { /* private window */ }
  const apply = () => {
    if (t === "auto") document.documentElement.removeAttribute("data-theme");
    else document.documentElement.dataset.theme = t;
    const word = { auto: "automatic", light: "light", dark: "dark" }[t];
    btn.replaceChildren(icon({ auto: "auto", light: "sun", dark: "moon" }[t]), h("span", { class: "word" }, "Theme: " + word));
    btn.setAttribute("aria-label", "Colour theme: " + word);
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
  // a card inserted shows up by itself; one listing at a time (a slow one never piles up requests)
  let listing = false;
  const t = setInterval(async () => {
    if (!view || view.step !== "drive" || listing) return;
    listing = true;
    try { await doCmd("refresh"); } catch { /* the page says when the program is gone */ } finally { listing = false; }
  }, 2000);
  stopOnGone.push(() => clearInterval(t));
}
main();
