// The JavaScript of the interface (design §11.1): plain, no framework, no build. Pages
// come from the server and work without it; the script spares reloads (panel, filters,
// pages, rule choice), counts down, polls grants in transition and copies.
"use strict";

// Selection: [data-select-all] toggles the [data-select] boxes of its list; the
// header actions [data-action] are enabled only with a selection and show its size. A
// named action (data-action="revoke") also needs every selected row to allow it, as its
// data-actions lists.
function updateSelection(list) {
  const boxes = [...list.querySelectorAll("[data-select]")];
  const picked = boxes.filter((b) => b.checked);
  boxes.forEach((b) => b.closest("tr").classList.toggle("selected", b.checked));
  const all = list.querySelector("[data-select-all]");
  if (all) {
    all.checked = boxes.length > 0 && picked.length === boxes.length;
    all.indeterminate = picked.length > 0 && picked.length < boxes.length;
  }
  list.querySelectorAll("[data-action]").forEach((btn) => {
    const name = btn.dataset.action;
    const allowed = !name || picked.every((b) => (b.dataset.actions || "").split(" ").includes(name));
    const exact = Number(btn.dataset.count) || 0;
    btn.disabled = picked.length === 0 || !allowed || (exact > 0 && picked.length !== exact);
    btn.querySelector("[data-selected-count]").textContent = picked.length ? ` (${picked.length})` : "";
  });
}

document.addEventListener("change", (e) => {
  const list = e.target.closest("[data-list]");
  if (!list) return;
  if (e.target.matches("[data-select-all]")) {
    list.querySelectorAll("[data-select]").forEach((b) => { b.checked = e.target.checked; });
  }
  if (e.target.matches("[data-select-all], [data-select]")) updateSelection(list);
});

document.addEventListener("click", (e) => {
  const copy = e.target.closest("[data-copy]");
  if (copy) {
    navigator.clipboard.writeText(copy.dataset.copy).then(() => {
      copy.classList.add("copied");
      setTimeout(() => copy.classList.remove("copied"), 1500);
    });
  }
});

// Search narrows the rows of the current page.
document.addEventListener("input", (e) => {
  if (!e.target.matches("[data-search]")) return;
  const words = e.target.value.toLowerCase().trim().split(/\s+/);
  const list = e.target.closest("[data-list]");
  list.querySelectorAll("tbody tr[data-row]").forEach((tr) => {
    const text = tr.textContent.toLowerCase();
    tr.classList.toggle("hidden", !words.every((w) => text.includes(w)));
  });
});

// Countdowns: one timer for all <time data-until> of the page.
function remaining(until) {
  const s = Math.round((until - Date.now()) / 1000);
  if (s <= 0) return "0 min";
  if (s < 60) return s + " s";
  const m = Math.ceil(s / 60);
  return m < 60 ? m + " min" : Math.floor(m / 60) + " h " + (m % 60) + " min";
}

function tick() {
  document.querySelectorAll("time[data-until]").forEach((t) => {
    t.textContent = remaining(Date.parse(t.dateTime));
  });
}

// Polling: an element with data-poll is replaced by the HTML at that address after
// 2, 4, 8, 16 and then every 30 seconds, until the replacement has no data-poll. When its
// data-state changes the waits start again from 2 seconds: the next step may be quick.
const pollDelays = [2000, 4000, 8000, 16000, 30000];

// replace swaps el for the HTML at address, keeping its selection and the open-row mark.
// It returns the new element and whether its state moved.
// sync reads again the other elements showing the same grant (its row, its flash), so a
// change seen in one is not contradicted by the other.
function sync(next) {
  const id = next.dataset.grant;
  if (!id) return;
  document.querySelectorAll(`[data-grant="${CSS.escape(id)}"]`).forEach(async (other) => {
    if (other === next || other.dataset.state === next.dataset.state) return;
    const address = other.dataset.poll || other.querySelector("[data-refresh]")?.dataset.refresh;
    if (!address) return;
    try {
      const { next: fresh } = await replace(other, address, false);
      if (fresh.dataset.poll && !other.dataset.poll) poll(fresh, 0);
    } catch {
      other.title = "Status not refreshed: reload the page";
    }
  });
}

async function replace(el, address, follow = true) {
  const res = await fetch(address, { headers: { Accept: "text/html" } });
  if (!res.ok) throw new Error(res.status);
  const tpl = document.createElement("template");
  tpl.innerHTML = (await res.text()).trim();
  const next = tpl.content.firstElementChild;
  const moved = next.dataset.state !== el.dataset.state;
  const box = el.querySelector("[data-select]");
  const kept = next.querySelector("[data-select]");
  if (box && kept) kept.checked = box.checked;
  if (el.classList.contains("open")) next.classList.add("open");
  el.replaceWith(next);
  const list = next.closest("[data-list]");
  if (list) updateSelection(list);
  tick();
  if (moved && follow) sync(next);
  return { next, moved };
}

// A state that has not moved for 2 minutes is stuck or slow: the page stops asking, so a
// tab left open makes no calls, and offers the refresh button instead.
const pollLimit = 120000;

function poll(el, attempt, since = Date.now()) {
  const delay = pollDelays[Math.min(attempt, pollDelays.length - 1)];
  setTimeout(async () => {
    if (!el.isConnected) return; // the page around it changed
    try {
      const { next, moved } = await replace(el, el.dataset.poll);
      // The open panel follows its row: a new state may bring new actions or the console.
      if (moved && next.classList.contains("open")) {
        const link = next.querySelector("a[data-panel]");
        if (link) openPanel(link);
      }
      if (!next.dataset.poll) return;
      if (moved) return poll(next, 0);
      if (Date.now() - since > pollLimit) return stopPolling(next);
      poll(next, attempt + 1, since);
    } catch {
      el.title = "Status not refreshed: reload the page";
    }
  }, delay);
}

// stopPolling swaps the spinner of an element for a refresh button on the same address.
function stopPolling(el) {
  const address = el.dataset.poll;
  delete el.dataset.poll;
  el.classList.add("stalled"); // the state icon stops turning: nothing is asking
  const state = el.querySelector(".st") || el.querySelector(".flash-text");
  if (!state || el.querySelector("[data-refresh]")) return;
  const tpl = document.getElementById("refresh-button");
  const btn = tpl.content.firstElementChild.cloneNode(true);
  btn.dataset.refresh = address;
  state.after(" ", btn);
}

const refreshPause = 5000;

// Refresh: a [data-refresh] button reads its row or flash again once. A pending request
// waits for a person, so the page does not ask on its own; when the state moves on to one
// that changes by itself, the polling takes over.
document.addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-refresh]");
  if (!btn) return;
  const el = btn.closest("tr, .flash");
  btn.disabled = true;
  // "Checking…" takes the place of the state and the button, so nothing moves.
  const state = btn.parentElement.querySelector(".st");
  const hidden = [btn, state].filter(Boolean);
  const note = document.createElement("span");
  note.className = "checking";
  note.textContent = "Checking…";
  btn.after(note);
  hidden.forEach((h) => { h.hidden = true; });
  try {
    const { next } = await replace(el, btn.dataset.refresh);
    if (next.dataset.poll) return poll(next, 0);
    // A state waits for a person: asking again within seconds only costs calls.
    const again = next.querySelector("[data-refresh]");
    if (again) {
      again.disabled = true;
      setTimeout(() => { again.disabled = false; }, refreshPause);
    }
  } catch {
    note.remove();
    hidden.forEach((h) => { h.hidden = false; });
    btn.disabled = false;
    btn.title = "Not refreshed: try again, or reload the page";
  }
});

// Closing: a [data-dismiss] button removes the notice or flash it sits in. Only finished
// outcomes have one: a request still moving keeps its flash.
document.addEventListener("click", (e) => {
  const btn = e.target.closest("[data-dismiss]");
  if (btn) btn.closest(".flash")?.remove();
});

// fill puts the HTML at an address into box; it says so in the box when it fails.
async function fill(box, address) {
  try {
    const res = await fetch(address, { headers: { Accept: "text/html" } });
    if (!res.ok) throw new Error(res.status);
    box.innerHTML = await res.text();
    tick();
    return true;
  } catch {
    box.textContent = "Not loaded: try again, or reload the page.";
    return false;
  }
}

// Details read when opened: <details data-load="/address"> fills its
// [data-load-target] with the HTML at that address, once. With data-load-from="<input id>"
// the address ends with that input's value, and changing the input reads it again.
document.addEventListener("toggle", async (e) => {
  const d = e.target;
  if (!(d instanceof HTMLDetailsElement) || !d.open || !d.dataset.load || d.dataset.loaded) return;
  d.dataset.loaded = "1";
  const from = d.dataset.loadFrom && document.getElementById(d.dataset.loadFrom);
  const address = d.dataset.load + (from ? encodeURIComponent(from.value) : "");
  if (!(await fill(d.querySelector("[data-load-target]"), address))) delete d.dataset.loaded;
}, true); // toggle does not bubble

// Tabs: [data-tabs] shows the [data-tabpanel] of the chosen [data-tab]; a panel with
// data-load is read the first time it shows.
document.addEventListener("click", async (e) => {
  const tab = e.target.closest("[data-tabs] [data-tab]");
  if (!tab) return;
  const tabs = tab.closest("[data-tabs]");
  tabs.querySelectorAll("[data-tab]").forEach((t) => t.setAttribute("aria-selected", String(t === tab)));
  let shown;
  tabs.querySelectorAll("[data-tabpanel]").forEach((p) => {
    p.hidden = p.dataset.tabpanel !== tab.dataset.tab;
    if (!p.hidden) shown = p;
  });
  if (shown && shown.dataset.load && !shown.dataset.loaded) {
    shown.dataset.loaded = "1";
    if (!(await fill(shown.querySelector("[data-load-target]"), shown.dataset.load))) delete shown.dataset.loaded;
  }
});

// The bottom panel follows the chosen row without reloading the page: a link with
// data-panel, or a single selected row, fills [data-panel-slot] from that address. The
// page address changes too, so a reload or a shared link opens the same grant.
async function openPanel(link) {
  const slot = document.querySelector("[data-panel-slot]");
  if (!slot) return false;
  try {
    const res = await fetch(link.dataset.panel, { headers: { Accept: "text/html" } });
    if (!res.ok) throw new Error(res.status);
    const tpl = document.createElement("template");
    tpl.innerHTML = (await res.text()).trim();
    const next = tpl.content.firstElementChild;
    if (!next || !next.matches("[data-panel-slot]")) throw new Error("not a panel");
    slot.replaceWith(next);
    fillAll(next);
    history.replaceState(null, "", link.href);
    document.querySelectorAll("tr.open").forEach((tr) => tr.classList.remove("open"));
    link.closest("tr")?.classList.add("open");
    tick();
    return true;
  } catch {
    return false;
  }
}

document.addEventListener("click", async (e) => {
  const link = e.target.closest("a[data-panel]");
  if (!link || e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
  e.preventDefault();
  if (!(await openPanel(link))) location.href = link.href;
});

document.addEventListener("change", (e) => {
  const list = e.target.closest("[data-list]");
  if (!list || !e.target.matches("[data-select]")) return;
  const picked = [...list.querySelectorAll("[data-select]:checked")];
  if (picked.length === 1) {
    const link = picked[0].closest("tr").querySelector("a[data-panel]");
    if (link) openPanel(link);
  }
});

// Closing the panel hides it until a row is chosen again; deselecting a row keeps it, as
// in the console. The close link works without the script.
document.addEventListener("click", (e) => {
  const close = e.target.closest("[data-panel-close]");
  if (!close || e.button !== 0) return;
  e.preventDefault();
  const empty = document.createElement("section");
  empty.className = "split-panel";
  empty.hidden = true;
  empty.setAttribute("data-panel-slot", "");
  close.closest("[data-panel-slot]").replaceWith(empty);
  document.querySelectorAll("tr.open").forEach((tr) => tr.classList.remove("open"));
  history.replaceState(null, "", close.href);
});

// Filters and pages of a list change the page without reloading it: the new page's main
// area replaces this one, and the browser's back button returns to the previous.
async function go(address, push) {
  const main = document.querySelector("main.main");
  try {
    const res = await fetch(address, { headers: { Accept: "text/html" } });
    if (!res.ok || res.redirected) throw new Error(res.status);
    const next = new DOMParser().parseFromString(await res.text(), "text/html").querySelector("main.main");
    if (!next) throw new Error("no page");
    main.replaceChildren(...next.childNodes);
    if (push) history.pushState(null, "", address);
    start(main);
  } catch {
    location.href = address;
  }
}

document.addEventListener("click", (e) => {
  const link = e.target.closest(".segmented a, .pager a");
  if (!link || e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
  e.preventDefault();
  go(link.href, true);
});

window.addEventListener("popstate", () => go(location.href, false));

// Resize: dragging the handle sets the panel height, remembered by this browser only.
function setPanelHeight(px) {
  const h = Math.max(120, Math.min(px, Math.round(window.innerHeight * 0.8)));
  document.documentElement.style.setProperty("--panel-h", h + "px");
  return h;
}

try {
  const saved = Number(localStorage.getItem("panelHeight"));
  if (saved > 0) setPanelHeight(saved);
} catch { /* storage may be blocked: the default height stays */ }

document.addEventListener("pointerdown", (e) => {
  const handle = e.target.closest("[data-panel-resize]");
  if (!handle) return;
  e.preventDefault();
  handle.setPointerCapture(e.pointerId);
  let h;
  const move = (ev) => { h = setPanelHeight(window.innerHeight - ev.clientY); };
  const up = () => {
    handle.removeEventListener("pointermove", move);
    handle.removeEventListener("pointerup", up);
    try { if (h) localStorage.setItem("panelHeight", String(h)); } catch { /* not kept */ }
  };
  handle.addEventListener("pointermove", move);
  handle.addEventListener("pointerup", up);
});

document.addEventListener("keydown", (e) => {
  if (!e.target.matches("[data-panel-resize]") || !["ArrowUp", "ArrowDown"].includes(e.key)) return;
  e.preventDefault();
  const now = e.target.closest("[data-panel-slot]").getBoundingClientRect().height;
  const h = setPanelHeight(now + (e.key === "ArrowUp" ? 40 : -40));
  try { localStorage.setItem("panelHeight", String(h)); } catch { /* not kept */ }
});

// A choice that changes part of the page: <input data-swap="/address?x=" data-swap-into="id"
// data-swap-url="/page?x="> fills that element with the HTML for the chosen value and
// updates the page address. Without the script, the form's own button sends it.
document.addEventListener("change", async (e) => {
  const input = e.target;
  if (!input.matches("[data-swap]")) return;
  const value = encodeURIComponent(input.value);
  const into = document.getElementById(input.dataset.swapInto);
  input.closest("tbody")?.querySelectorAll("tr").forEach((tr) => tr.classList.toggle("selected", tr.contains(input)));
  if (into && (await fill(into, input.dataset.swap + value))) {
    history.replaceState(null, "", input.dataset.swapUrl + value);
  }
});

document.addEventListener("change", (e) => {
  document.querySelectorAll(`details[data-load-from="${e.target.id}"]`).forEach((d) => {
    delete d.dataset.loaded;
    d.querySelector("[data-load-target]").innerHTML = '<p class="muted">Loading…</p>';
    d.open = false;
  });
});

// A table to choose from, searched and shown a page at a time: <div data-paginate="8">
// with an [data-filter] input, a [data-pager] and the rows in its tbody. The page with the
// chosen row shows first. Without the script every row shows.
function paginate(box) {
  const size = Number(box.dataset.paginate);
  const rows = [...box.querySelectorAll("tbody tr")];
  const filter = box.querySelector("[data-filter]");
  const pager = box.querySelector("[data-pager]");
  const none = box.querySelector("[data-no-match]");
  let page = 0;
  const matching = () => {
    const words = filter.value.toLowerCase().trim().split(/\s+/);
    return rows.filter((tr) => words.every((w) => tr.textContent.toLowerCase().includes(w)));
  };
  const show = () => {
    const shown = matching();
    const pages = Math.max(1, Math.ceil(shown.length / size));
    page = Math.min(page, pages - 1);
    rows.forEach((tr) => { tr.hidden = true; tr.classList.remove("last-shown"); });
    const now = shown.slice(page * size, page * size + size);
    now.forEach((tr) => { tr.hidden = false; });
    // Rows hidden after it would leave the last one shown without its bottom line.
    now.at(-1)?.classList.add("last-shown");
    none.hidden = shown.length > 0;
    pager.replaceChildren();
    if (pages < 2) return;
    const button = (label, to, name) => {
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = label;
      if (name) b.setAttribute("aria-label", name);
      if (to === page) b.setAttribute("aria-current", "true");
      b.disabled = to < 0 || to >= pages;
      b.addEventListener("click", () => { page = to; show(); });
      pager.append(b);
    };
    button("‹", page - 1, "Previous page");
    for (let i = 0; i < pages; i++) button(String(i + 1), i);
    button("›", page + 1, "Next page");
  };
  const chosen = rows.findIndex((tr) => tr.querySelector("input:checked"));
  if (chosen > 0) page = Math.floor(chosen / size);
  filter.addEventListener("input", () => { page = 0; show(); });
  show();
}

// A form in steps: <form data-steps> shows one [data-step] section at a time, and Next
// moves on only when the fields of the current one are valid. Without this script every
// step shows at once and the form still works.
function steps(form) {
  const all = [...form.querySelectorAll("[data-step]")];
  const back = form.querySelector("[data-step-back]");
  const next = form.querySelector("[data-step-next]");
  const done = form.querySelector("[data-step-done]");
  let at = 0;
  const show = () => {
    all.forEach((s, i) => { s.hidden = i !== at; });
    back.hidden = at === 0;
    next.hidden = at === all.length - 1;
    done.hidden = at !== all.length - 1;
  };
  back.addEventListener("click", () => { at--; show(); });
  next.addEventListener("click", () => {
    const bad = [...all[at].querySelectorAll("input, select")].find((f) => !f.checkValidity());
    if (bad) { bad.reportValidity(); return; }
    at++;
    show();
  });
  show();
}

// start wires the parts of a page, or of the part of it just replaced.
function start(root) {
  root.querySelectorAll("form[data-steps]").forEach(steps);
  root.querySelectorAll("[data-paginate]").forEach(paginate);
  root.querySelectorAll("[data-list]").forEach(updateSelection);
  root.querySelectorAll("[data-poll]").forEach((el) => poll(el, 0));
  tick();
}

start(document);
setInterval(tick, 1000);

// Summary of a form: each [data-mirror="name ..."] shows what the named fields hold now,
// as their labels read: the chosen option, the checked boxes, the text typed.
function fieldText(form, name) {
  const els = [...form.elements].filter((el) => el.name === name);
  const parts = [];
  for (const el of els) {
    if (el instanceof HTMLSelectElement) {
      if (el.value) parts.push(el.selectedOptions[0].textContent.trim());
    } else if (el.type === "checkbox" || el.type === "radio") {
      if (el.checked) parts.push(el.type === "checkbox" && els.length === 1 ? "Yes" : el.closest("label").textContent.trim());
    } else if (el.value.trim()) {
      parts.push(el.value.trim());
    }
  }
  return parts.join(", ");
}

// Fields shown only with one choice: [data-show-when="name=value"].
function showWhen(form) {
  form.querySelectorAll("[data-show-when]").forEach((el) => {
    const [name, value] = el.dataset.showWhen.split("=");
    const picked = [...form.elements].find((f) => f.name === name && f.checked);
    el.hidden = !picked || picked.value !== value;
    // A hidden field is not sent: the service sees only what the form shows.
    el.querySelectorAll("input, select, textarea").forEach((f) => { f.disabled = el.hidden; });
  });
}

function mirror(form) {
  showWhen(form);
  form.querySelectorAll("[data-mirror]").forEach((dd) => {
    const names = dd.dataset.mirror.split(" ").filter((n) => !form.querySelector(`[data-show-when] [name="${n}"]`)?.closest("[data-show-when]").hidden);
    dd.textContent = names.map((n) => fieldText(form, n)).filter(Boolean).join(" · ") || "-";
  });
}

document.querySelectorAll("[data-mirror-form], form:has([data-show-when])").forEach((form) => {
  mirror(form);
  form.addEventListener("input", () => mirror(form));
  form.addEventListener("change", () => mirror(form));
});

// Parts read after the page shows: [data-fill] is replaced by the HTML at its address,
// once. The panel uses it for what takes AWS a few seconds, such as the CLI profile.
function fillAll(root) {
  root.querySelectorAll("[data-fill]").forEach(async (box) => {
    const address = box.dataset.fill;
    delete box.dataset.fill;
    await fill(box, address);
  });
}
fillAll(document);
