// The JavaScript of the interface: plain, no framework, no build. Pages
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

// A click on a row, outside its links and controls, selects it as its box or radio would
// (Cloudscape): the 16-pixel control alone is hard to hit, above all on a phone. Only the
// id link opens the panel.
document.addEventListener("click", (e) => {
  const tr = e.target.closest("tbody tr");
  if (!tr || e.target.closest("a, button, input, label, select, textarea, summary")) return;
  const controls = tr.querySelectorAll("input[type=checkbox], input[type=radio]");
  if (controls.length !== 1 || controls[0].disabled) return;
  const control = controls[0];
  if (control.type === "radio" && control.checked) return;
  control.checked = control.type === "radio" ? true : !control.checked;
  control.dispatchEvent(new Event("change", { bubbles: true }));
  control.dispatchEvent(new Event("input", { bubbles: true }));
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
    const until = Date.parse(t.dateTime);
    const row = t.closest("[data-expire]");
    const ended = until <= Date.now();
    // A row past its end says so until the service closes it, not "0 min".
    t.textContent = row && ended ? "Ending…" : remaining(until);
    if (row && ended && !row.dataset.expiring) {
      row.dataset.expiring = "1";
      expire(row, 0);
    }
  });
}

// The open panel follows its row: a new state may bring new actions or the console. The
// tab shown stays the one the person chose.
async function followPanel(next, moved) {
  if (!moved || !next.classList.contains("open")) return;
  const link = next.querySelector("a[data-panel]");
  if (!link) return;
  const tab = document.querySelector("[data-panel-slot] [data-tab][aria-selected=true]")?.dataset.tab;
  if (!(await openPanel(link)) || !tab) return;
  document.querySelector(`[data-panel-slot] [data-tab="${CSS.escape(tab)}"]`)?.click();
}

// Expiry: an active row changes only when the grant ends, so the page asks nothing while
// it runs. Once the countdown ends it reads the row 20 seconds later, time for the service
// to close the grant, then at most 3 more times; a row still active after that keeps
// saying "Ending…" until the page is reloaded.
const expireDelays = [20000, 30000, 30000, 30000];

function expire(el, attempt) {
  if (attempt >= expireDelays.length) return;
  setTimeout(async () => {
    if (!el.isConnected) return;
    try {
      const { next, moved } = await replace(el, el.dataset.expire);
      followPanel(next, moved); // the panel loses the Access box with the grant
      if (next.dataset.poll) return poll(next, 0); // revoking: the polling takes over
      if (next.dataset.expire) {
        next.dataset.expiring = "1";
        expire(next, attempt + 1);
      }
    } catch {
      el.title = "Status not refreshed: reload the page";
    }
  }, expireDelays[attempt]);
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
      const { next: fresh, moved } = await replace(other, address, false);
      followPanel(fresh, moved);
      // The loop of the replaced element stops (it is no longer in the page): start one.
      if (fresh.dataset.poll) poll(fresh, 0);
    } catch {
      other.title = "Status not refreshed: reload the page";
    }
  });
}

// With hold, an unchanged element is swapped only once hold resolves, so a quick answer
// does not make "Checking…" flash.
async function replace(el, address, follow = true, hold = null) {
  const res = await fetch(address, { headers: { Accept: "text/html" } });
  if (!res.ok) throw new Error(res.status);
  const tpl = document.createElement("template");
  tpl.innerHTML = (await res.text()).trim();
  const next = tpl.content.firstElementChild;
  const moved = next.dataset.state !== el.dataset.state;
  if (hold && !moved) await hold;
  const box = el.querySelector("[data-select]");
  const kept = next.querySelector("[data-select]");
  if (box && kept) kept.checked = box.checked;
  if (el.classList.contains("open")) next.classList.add("open");
  el.replaceWith(next);
  const list = next.closest("[data-list]");
  if (list) updateSelection(list);
  tick();
  if (moved && follow) sync(next);
  if (moved) sumUp();
  return { next, moved };
}

// sumUp rewrites a notice that started revocations once none of its grants is still
// revoking, from the states their rows show: "2 grants revoked.", "1 revoked, 1 failed."
// It asks the service nothing: the rows are already being polled.
function sumUp() {
  document.querySelectorAll(".flash[data-follow]").forEach((flash) => {
    const ids = flash.dataset.follow.split(" ");
    const rows = ids.map((id) => document.querySelector(`tr[data-grant="${CSS.escape(id)}"]`));
    if (rows.some((r) => !r || r.dataset.state === "revoking")) return;
    const failedIds = ids.filter((id, i) => rows[i].dataset.state === "revoke_failed");
    const revoked = ids.length - failedIds.length;
    const failed = failedIds.length + Number(flash.dataset.followFailed || 0);
    let kind, text;
    if (failed === 0) {
      kind = "success";
      text = ids.length === 1 ? `Grant ${ids[0]} revoked.` : `${ids.length} grants revoked.`;
    } else if (revoked === 0) {
      kind = "error";
      text = failed === 1 ? "The revocation failed." : `${failed} revocations failed.`;
    } else {
      kind = "warning";
      text = `${revoked} revoked, ${failed} failed.`;
    }
    if (failedIds.length) {
      let list = flash.querySelector(".flash-list");
      if (!list) {
        list = document.createElement("ul");
        list.className = "flash-body flash-list";
        flash.append(list);
      }
      failedIds.forEach((id) => {
        const li = document.createElement("li");
        li.textContent = `${id}: revocation failed, the access may still work. Open it for the error and Retry.`;
        list.append(li);
      });
    }
    flash.classList.remove("success", "warning", "error");
    flash.classList.add(kind);
    const icon = document.getElementById(`flash-icon-${kind}`).content.firstElementChild.cloneNode(true);
    flash.querySelector(".flash-icon")?.replaceWith(icon);
    flash.querySelector(".flash-text").textContent = text;
    delete flash.dataset.follow;
  });
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
      followPanel(next, moved);
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
    const hold = new Promise((done) => setTimeout(done, 800));
    const { next, moved } = await replace(el, btn.dataset.refresh, true, hold);
    followPanel(next, moved);
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
// updates the page address. Fields marked [data-keep] keep what was typed in them.
// Without the script, the form's own button sends it.
document.addEventListener("change", async (e) => {
  const input = e.target;
  if (!input.matches("[data-swap]")) return;
  const value = encodeURIComponent(input.value);
  const into = document.getElementById(input.dataset.swapInto);
  input.closest("tbody")?.querySelectorAll("tr").forEach((tr) => tr.classList.toggle("selected", tr.contains(input)));
  if (!into) return;
  const kept = new Map([...into.querySelectorAll("[data-keep]")].map((f) => [f.name, f.value]));
  if (await fill(into, input.dataset.swap + value)) {
    history.replaceState(null, "", input.dataset.swapUrl + value);
    into.querySelectorAll("[data-keep]").forEach((f) => {
      if (!kept.get(f.name)) return;
      f.value = kept.get(f.name);
      f.dispatchEvent(new Event("input", { bubbles: true }));
    });
  }
});

document.addEventListener("change", (e) => {
  document.querySelectorAll(`details[data-load-from="${e.target.id}"]`).forEach((d) => {
    delete d.dataset.loaded;
    d.querySelector("[data-load-target]").innerHTML = '<p class="muted">Loading…</p>';
    d.open = false;
  });
});

// A table or cards to choose from, searched and shown a page at a time:
// <div data-paginate="8"> with an [data-filter] input, a [data-pager] and the rows in its
// tbody, or cards marked [data-item]. The page with the chosen one shows first. Without
// the script everything shows.
function paginate(box) {
  const size = Number(box.dataset.paginate);
  const rows = [...box.querySelectorAll("tbody tr, [data-item]")];
  const filter = box.querySelector("[data-filter]");
  const pager = box.querySelector("[data-pager]");
  const none = box.querySelector("[data-no-match]");
  let page = 0;
  const matching = () => {
    const words = (filter ? filter.value : "").toLowerCase().trim().split(/\s+/);
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
  filter?.addEventListener("input", () => { page = 0; show(); });
  show();
}

// Cards too many to show at once: <div data-limit="10"> shows at most that many [data-item]
// cards, the chosen ones first, and the others only when the [data-filter] search finds
// them; [data-more] says how many stay hidden. Hidden cards cost no layout, so a large
// organization keeps the page fast. Without the script every card shows.
function limit(box) {
  const size = Number(box.dataset.limit);
  const cards = [...box.querySelectorAll("[data-item]")];
  const filter = box.querySelector("[data-filter]");
  const none = box.querySelector("[data-no-match]");
  const more = box.querySelector("[data-more]");
  const chosen = (c) => c.querySelector("input:checked") !== null;
  const show = () => {
    const words = (filter ? filter.value : "").toLowerCase().trim().split(/\s+/);
    const found = cards.filter((c) => words.every((w) => c.textContent.toLowerCase().includes(w)));
    const shown = [...found.filter(chosen), ...found.filter((c) => !chosen(c))].slice(0, size);
    cards.forEach((c) => { c.hidden = !shown.includes(c); });
    none.hidden = found.length > 0;
    const hidden = found.length - shown.length;
    more.hidden = hidden === 0;
    more.textContent = `${hidden} more ${hidden === 1 ? "account" : "accounts"}: search by name or id to find ${hidden === 1 ? "it" : "them"}.`;
  };
  filter?.addEventListener("input", show);
  show();
}

// A form in steps: <form data-steps> shows one [data-step] section at a time, and Next
// moves on only when the fields of the current one are valid. [data-step-current] is the
// one to show first; [data-step-mark] items mark where the form is. A [data-step-review]
// step, made by the server from what was sent, ends with its own buttons; Edit
// ([data-step-goto="n"]) drops it, since the steps may change, and goes back to step n.
// A mark of a step already passed goes back to it; marks never jump forward, since
// only Next checks the fields.
// Without this script every step shows at once and the form still works.
function steps(form) {
  let all = [...form.querySelectorAll("[data-step]")];
  const marks = [...form.querySelectorAll("[data-step-mark]")];
  const back = form.querySelector("[data-step-back]");
  const next = form.querySelector("[data-step-next]");
  const done = form.querySelector("[data-step-done]");
  let at = Math.max(0, all.findIndex((s) => s.hasAttribute("data-step-current")));
  const show = () => {
    const review = all[at].hasAttribute("data-step-review");
    all.forEach((s, i) => { s.hidden = i !== at; });
    back.hidden = at === 0 || review;
    next.hidden = review || at === all.length - 1;
    done.hidden = review || at !== all.length - 1;
    marks.forEach((m, i) => {
      m.classList.toggle("current", i === at);
      m.classList.toggle("done", i < at);
      if (i === at) m.setAttribute("aria-current", "step"); else m.removeAttribute("aria-current");
      if (i < at) {
        m.setAttribute("role", "button");
        m.tabIndex = 0;
      } else {
        m.removeAttribute("role");
        m.removeAttribute("tabindex");
      }
    });
    all[at].querySelector("h2")?.scrollIntoView({ block: "nearest" });
  };
  back.addEventListener("click", () => { at--; show(); });
  next.addEventListener("click", () => {
    if (!showErrors(all[at], [...all[at].querySelectorAll("input, select, textarea")])) return;
    at++;
    show();
  });
  const goTo = (n) => {
    const review = form.querySelector("[data-step-review]");
    if (review) { review.remove(); all = all.filter((s) => s !== review); }
    at = n;
    show();
  };
  form.addEventListener("click", (e) => {
    const go = e.target.closest("[data-step-goto]");
    if (go) goTo(Number(go.dataset.stepGoto));
  });
  marks.forEach((m, i) => {
    const back = () => { if (i < at) goTo(i); };
    m.addEventListener("click", back);
    m.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); back(); }
    });
  });
  show();
}

// start wires the parts of a page, or of the part of it just replaced.
function start(root) {
  root.querySelectorAll("form[data-steps]").forEach(steps);
  root.querySelectorAll("[data-paginate]").forEach(paginate);
  root.querySelectorAll("[data-limit]").forEach(limit);
  root.querySelectorAll("[data-list]").forEach(updateSelection);
  root.querySelectorAll("[data-poll]").forEach((el) => poll(el, 0));
  tick();
}

start(document);
// Reload: a list with data-reload has rows the service has not caught up with yet, as
// rules for about 30 seconds after a publish. It reads its rows again 30 seconds after the
// page opened, then every 10 seconds, for at most 2 minutes, keeping the selection.
const reloadFirst = 30000;
const reloadEvery = 10000;
const reloadLimit = 120000;

function reloadList(list, since) {
  setTimeout(async () => {
    if (!list.isConnected) return;
    try {
      const res = await fetch(location.href, { headers: { Accept: "text/html" } });
      if (!res.ok) throw new Error(res.status);
      const doc = new DOMParser().parseFromString(await res.text(), "text/html");
      const fresh = doc.querySelector("form[data-reload], form[data-list]");
      // The service now uses what was published: roles come from it, so the menu changes too.
      if (!fresh.hasAttribute("data-reload")) return location.reload();
      const checked = new Set([...list.querySelectorAll("[data-select]:checked")].map((b) => b.value));
      const open = list.querySelector("tr.open")?.dataset.grant;
      list.querySelector("tbody").replaceWith(fresh.querySelector("tbody"));
      list.querySelector(".counter").replaceWith(fresh.querySelector(".counter"));
      list.querySelectorAll("[data-select]").forEach((b) => { b.checked = checked.has(b.value); });
      if (open) list.querySelector(`tr[data-grant="${CSS.escape(open)}"]`)?.classList.add("open");
      updateSelection(list);
      list.querySelector("[data-search]")?.dispatchEvent(new Event("input", { bubbles: true })); // keep the filter
      if (Date.now() - since < reloadLimit) reloadList(list, since);
    } catch {
      list.title = "List not refreshed: reload the page";
    }
  }, Date.now() - since < reloadFirst ? reloadFirst : reloadEvery);
}

document.querySelectorAll("form[data-reload]").forEach((list) => reloadList(list, Date.now()));

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
      // A single checkbox is a yes or no; checked is the only way a group says something.
      if (el.type === "checkbox" && els.length === 1) parts.push(el.checked ? "Yes" : "No");
      else if (el.checked) parts.push(el.closest("label").textContent.replace(/\s+/g, " ").trim());
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

// A summary follows the form as it is filled: [data-mirror="name …"] inside the form, or
// anywhere with data-of="<form id>", shows those fields, or data-empty when they are.
function mirror(form) {
  showWhen(form);
  const dds = [...form.querySelectorAll("[data-mirror]")];
  if (form.id) dds.push(...document.querySelectorAll(`[data-mirror][data-of="${form.id}"]`));
  dds.forEach((dd) => {
    const names = dd.dataset.mirror.split(" ").filter((n) => !form.querySelector(`[data-show-when] [name="${n}"]`)?.closest("[data-show-when]").hidden);
    dd.textContent = names.map((n) => fieldText(form, n)).filter(Boolean).join(" · ") || dd.dataset.empty || "-";
  });
}

const mirrored = (form) => form?.matches?.("[data-mirror-form], form:has([data-show-when])") ||
  (form?.id && document.querySelector(`[data-of="${form.id}"]`));
document.querySelectorAll("form").forEach((form) => { if (mirrored(form)) mirror(form); });
// Delegated: the New request form arrives after the page, when a rule is chosen.
["input", "change"].forEach((type) => document.addEventListener(type, (e) => {
  if (mirrored(e.target.form)) mirror(e.target.form);
}));

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

// Confirmation dialogs: the page and its form stay as they are behind them, and Esc or
// "Go back" closes them. The opener gets the focus back.
document.addEventListener("click", (e) => {
  const open = e.target.closest("[data-dialog]");
  if (open) {
    const dialog = document.getElementById(open.dataset.dialog);
    dialog.addEventListener("close", () => open.focus(), { once: true });
    dialog.showModal();
  }
  const close = e.target.closest("dialog [data-close]");
  if (close) {
    e.preventDefault(); // a link back to the list when the page opened without a dialog
    close.closest("dialog").close();
  }
});

// What cannot be undone asks over the page: a [data-confirm] link or list
// action opens its confirmation page's form in a dialog, the chosen grants shown as a
// small table taken from their rows. Without the script, or if the page cannot be read,
// the confirmation opens as a page.
async function confirmDialog(address, opener) {
  let doc;
  try {
    const res = await fetch(address, { headers: { Accept: "text/html" } });
    if (!res.ok) throw new Error(res.status);
    doc = new DOMParser().parseFromString(await res.text(), "text/html");
  } catch {
    location.href = address;
    return;
  }
  const form = doc.querySelector(".confirm form");
  if (!form) {
    location.href = address;
    return;
  }
  const dialog = document.createElement("dialog");
  dialog.className = "modal";
  dialog.setAttribute("aria-labelledby", "confirm-title");
  const title = document.createElement("h2");
  title.id = "confirm-title";
  title.textContent = doc.querySelector("h1").textContent;
  const ids = form.querySelector("ul.ids");
  if (ids) {
    ids.querySelectorAll("input").forEach((input) => form.prepend(input));
    ids.replaceWith(grantTable([...form.querySelectorAll("input[name=id]")].map((i) => i.value)));
  }
  dialog.append(title, form);
  document.body.append(dialog);
  dialog.addEventListener("close", () => {
    dialog.remove();
    opener.focus();
  });
  dialog.showModal();
}

// grantTable lists the grants with the permission set and account their rows show.
function grantTable(ids) {
  const table = document.createElement("table");
  table.className = "compact";
  table.innerHTML = "<thead><tr><th>Id</th><th>Permission set</th><th>Account</th></tr></thead><tbody></tbody>";
  ids.forEach((id) => {
    const tr = document.createElement("tr");
    const row = document.querySelector(`tr[data-grant="${CSS.escape(id)}"]`);
    const heads = row ? [...row.closest("table").querySelectorAll("thead th")].map((th) => th.textContent.trim()) : [];
    const cellOf = (name) => row?.cells[heads.indexOf(name)];
    const idCell = document.createElement("td");
    idCell.textContent = id;
    const ps = document.createElement("td");
    ps.textContent = cellOf("Permission set")?.textContent.trim() ?? "";
    // The account is copied as it is, so its name and its id stay apart.
    const account = document.createElement("td");
    account.append(...[...(cellOf("Account")?.childNodes ?? [])].map((node) => node.cloneNode(true)));
    tr.append(idCell, ps, account);
    table.tBodies[0].append(tr);
  });
  return table;
}

document.addEventListener("click", (e) => {
  const link = e.target.closest("a[data-confirm]");
  if (!link || e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
  e.preventDefault();
  confirmDialog(link.href, link);
});

document.addEventListener("submit", (e) => {
  const button = e.submitter;
  if (!button || !button.matches("[data-confirm]")) return;
  e.preventDefault();
  const ids = new FormData(e.target).getAll("id");
  const query = new URLSearchParams(ids.map((id) => ["id", id]));
  confirmDialog(`${button.getAttribute("formaction")}?${query}`, button);
});

// Validation in the console's style: the browser's own bubbles speak its
// language and show one field at a time, so forms check themselves on submit and show
// every error in red under its field, in English. The submit button stays enabled; an
// error goes away when its field changes. Without the script the browser checks.
document.querySelectorAll("form").forEach((f) => { f.noValidate = true; });
new MutationObserver((records) => records.forEach((rec) => rec.addedNodes.forEach((n) => {
  if (n.nodeType !== 1) return;
  if (n.matches("form")) n.noValidate = true;
  n.querySelectorAll("form").forEach((f) => { f.noValidate = true; });
}))).observe(document.body, { childList: true, subtree: true });

function fieldLabel(el) {
  const label = el.id && document.querySelector(`label[for="${CSS.escape(el.id)}"]`);
  const text = label ? label.textContent : el.closest("fieldset")?.querySelector("legend")?.textContent;
  return (text || "This field").trim();
}

function errorText(el) {
  if (el.dataset.error) return el.dataset.error;
  if (el.validity.valueMissing) return `${fieldLabel(el)} is required.`;
  return el.title || `Enter a valid ${fieldLabel(el).toLowerCase()}.`;
}

function clearError(el) {
  const field = el.closest(".field") || el.parentElement;
  field.querySelector(".field-error")?.remove();
  el.removeAttribute("aria-invalid");
}

document.addEventListener("submit", (e) => {
  const form = e.target;
  if (!form.noValidate || (e.submitter && e.submitter.formNoValidate)) return;
  if (showErrors(form, [...form.elements])) return;
  e.preventDefault();
  e.stopImmediatePropagation(); // nothing else acts on a form that is not ready
}, true); // before the handlers that open dialogs or send the form

// showErrors puts a console-style message under each invalid field among fields, in
// container, and focuses the first. It answers whether all of them were valid.
function showErrors(container, fields) {
  const invalid = fields.filter((el) => el.willValidate && !el.checkValidity());
  container.querySelectorAll(".field-error").forEach((p) => p.remove());
  container.querySelectorAll("[aria-invalid]").forEach((el) => el.removeAttribute("aria-invalid"));
  if (!invalid.length) return true;
  const shown = new Set();
  invalid.forEach((el) => {
    const field = el.closest(".field") || el.parentElement;
    if (shown.has(field)) return; // one message per field, as for a group of radios
    shown.add(field);
    const p = document.createElement("p");
    p.className = "field-error";
    p.id = `${el.name || "field"}-error`;
    p.append(document.getElementById("flash-icon-error").content.firstElementChild.cloneNode(true), errorText(el));
    field.append(p);
    el.setAttribute("aria-invalid", "true");
    el.setAttribute("aria-describedby", p.id);
  });
  (invalid[0].filterTrigger || invalid[0]).focus();
  return false;
}

["input", "change"].forEach((type) => document.addEventListener(type, (e) => {
  if (e.target.getAttribute?.("aria-invalid") && e.target.checkValidity()) clearError(e.target);
}));

// A file input with data-file-name shows the chosen file's name in that element.
document.addEventListener("change", (e) => {
  const input = e.target;
  if (!input.matches?.("input[type=file][data-file-name]")) return;
  document.getElementById(input.dataset.fileName).textContent = input.files[0]?.name || "No file chosen";
});

// A select with data-filter becomes a Cloudscape "select with filtering": a button shows
// the choice, and a popover lists the options with a search field. The native select
// stays in the form, hidden, so sending, validation and change listeners work unchanged.
function filterSelect(select) {
  const trigger = document.createElement("button");
  trigger.type = "button";
  trigger.className = "select-trigger";
  trigger.setAttribute("aria-haspopup", "listbox");
  trigger.setAttribute("aria-expanded", "false");
  const label = select.id && document.querySelector(`label[for="${select.id}"]`);
  if (label) {
    label.htmlFor = `${select.id}-trigger`;
    trigger.id = `${select.id}-trigger`;
  }
  const shown = () => {
    const o = select.selectedOptions[0];
    trigger.textContent = o ? o.textContent : "";
    trigger.classList.toggle("placeholder", !o || o.value === "");
  };
  shown();
  select.hidden = true;
  select.filterTrigger = trigger;
  select.after(trigger);
  select.addEventListener("change", shown);

  let pop = null;
  let active = -1;
  const close = (refocus) => {
    if (!pop) return;
    pop.remove();
    pop = null;
    trigger.setAttribute("aria-expanded", "false");
    document.removeEventListener("pointerdown", outside, true);
    if (refocus) trigger.focus();
  };
  const outside = (e) => { if (pop && !pop.contains(e.target) && e.target !== trigger) close(false); };
  const items = () => [...pop.querySelectorAll("[role=option]:not([hidden])")];
  const mark = (i) => {
    const list = items();
    list.forEach((li) => li.classList.remove("active"));
    active = Math.max(0, Math.min(i, list.length - 1));
    const li = list[active];
    if (!li) return;
    li.classList.add("active");
    li.scrollIntoView({ block: "nearest" });
    pop.querySelector("input").setAttribute("aria-activedescendant", li.id);
  };
  const pick = (li) => {
    select.value = li.dataset.value;
    select.dispatchEvent(new Event("change", { bubbles: true }));
    close(true);
  };
  const open = () => {
    pop = document.createElement("div");
    pop.className = "select-popover";
    const search = document.createElement("input");
    search.type = "search";
    search.placeholder = "Find";
    search.setAttribute("aria-label", "Find");
    search.setAttribute("role", "combobox");
    search.setAttribute("aria-expanded", "true");
    const list = document.createElement("ul");
    list.setAttribute("role", "listbox");
    list.id = `${select.id}-options`;
    search.setAttribute("aria-controls", list.id);
    [...select.options].filter((o) => o.value !== "").forEach((o, i) => {
      const li = document.createElement("li");
      li.setAttribute("role", "option");
      li.id = `${select.id}-option-${i}`;
      li.dataset.value = o.value;
      li.textContent = o.textContent;
      li.setAttribute("aria-selected", String(o.selected));
      li.addEventListener("click", () => pick(li));
      list.append(li);
    });
    const none = document.createElement("p");
    none.className = "select-none";
    none.textContent = "No matches";
    none.hidden = true;
    search.addEventListener("input", () => {
      const q = search.value.trim().toLowerCase();
      list.querySelectorAll("li").forEach((li) => { li.hidden = !li.textContent.toLowerCase().includes(q); });
      none.hidden = items().length > 0;
      mark(0);
    });
    search.addEventListener("keydown", (e) => {
      if (e.key === "ArrowDown") { e.preventDefault(); mark(active + 1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); mark(active - 1); }
      else if (e.key === "Enter") { e.preventDefault(); const li = items()[active]; if (li) pick(li); }
      else if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); close(true); }
      else if (e.key === "Tab") close(false);
    });
    pop.append(search, list, none);
    trigger.after(pop);
    trigger.setAttribute("aria-expanded", "true");
    document.addEventListener("pointerdown", outside, true);
    search.focus();
    const chosen = items().findIndex((li) => li.dataset.value === select.value);
    mark(chosen < 0 ? 0 : chosen);
  };
  trigger.addEventListener("click", () => (pop ? close(true) : open()));
  trigger.addEventListener("keydown", (e) => {
    if (!pop && (e.key === "ArrowDown" || e.key === "ArrowUp")) { e.preventDefault(); open(); }
  });
}

document.querySelectorAll("select[data-filter]").forEach(filterSelect);

// A card shows whether it is chosen, and a list of several counts the chosen ones.
document.addEventListener("change", (e) => {
  const input = e.target;
  const box = input.closest?.(".cards");
  if (!box) return;
  box.querySelectorAll(".card").forEach((c) => c.classList.toggle("selected", c.querySelector("input").checked));
  const count = box.querySelector("[data-selected-count]");
  if (count) count.textContent = `Selected (${box.querySelectorAll(".card input:checked").length})`;
});

// An "Actions" menu (EC2's Instance state): [data-menu] opens the [role=menu] after it.
// Its items are the list's actions, enabled by the selection as the buttons are.
document.addEventListener("click", (e) => {
  const opener = e.target.closest("[data-menu]");
  document.querySelectorAll("[data-menu][aria-expanded=true]").forEach((b) => {
    if (b === opener) return;
    b.setAttribute("aria-expanded", "false");
    b.nextElementSibling.hidden = true;
  });
  if (!opener) return;
  const menu = opener.nextElementSibling;
  menu.hidden = !menu.hidden;
  opener.setAttribute("aria-expanded", String(!menu.hidden));
  if (!menu.hidden) menu.querySelector("[role=menuitem]:not(:disabled)")?.focus();
});

document.addEventListener("keydown", (e) => {
  const menu = e.target.closest?.("[role=menu]");
  if (!menu) return;
  const items = [...menu.querySelectorAll("[role=menuitem]:not(:disabled)")];
  const at = items.indexOf(e.target);
  if (e.key === "ArrowDown") { e.preventDefault(); items[(at + 1) % items.length]?.focus(); }
  else if (e.key === "ArrowUp") { e.preventDefault(); items[(at - 1 + items.length) % items.length]?.focus(); }
  else if (e.key === "Escape") {
    e.preventDefault();
    e.stopPropagation();
    menu.hidden = true;
    menu.previousElementSibling.setAttribute("aria-expanded", "false");
    menu.previousElementSibling.focus();
  }
});

// The local server closes after 30 minutes without requests. A page left open and visible
// says it is still there every 5 minutes (a call to this computer only, nothing to AWS);
// when the server no longer answers, the page says so and how to open it again.
let closedShown = false;
async function alive() {
  if (closedShown || document.visibilityState !== "visible") return;
  let ok = false;
  try { ok = (await fetch("/alive", { cache: "no-store" })).ok; } catch { /* closed */ }
  if (ok) return;
  closedShown = true;
  const notice = document.getElementById("closed-alert")?.content.cloneNode(true);
  if (notice) document.querySelector("main")?.prepend(notice);
}
setInterval(alive, 5 * 60 * 1000);
document.addEventListener("visibilitychange", alive);

// A reason field with data-count shows how much of its limit is used, as "12 / 500" in the
// [data-counter] under it. The server gives the limit on the field; nothing is hard-coded.
function updateCounter(field) {
  const counter = field.parentElement.querySelector("[data-counter]");
  if (counter) counter.textContent = `${field.value.length} / ${field.maxLength}`;
}
document.addEventListener("input", (e) => { if (e.target.matches("[data-count]")) updateCounter(e.target); });
document.querySelectorAll("[data-count]").forEach(updateCounter);

// A form being sent: its submit button says what it waits for (data-busy) and cannot be
// pressed twice. It is disabled after the browser has built the form data, which leaves
// out a disabled button's name and value. A form the script or the dialogs stopped
// never gets here, and pageshow brings every button back, also after Back from the next
// page (the browser may keep this page as it was).
const busyButtons = new Map();

document.addEventListener("submit", (e) => {
  const button = e.submitter;
  if (e.defaultPrevented || !button || !button.dataset.busy) return;
  setTimeout(() => {
    busyButtons.set(button, button.innerHTML);
    button.textContent = button.dataset.busy;
    button.disabled = true;
  }, 0);
});

window.addEventListener("pageshow", () => {
  busyButtons.forEach((html, button) => {
    button.innerHTML = html;
    button.disabled = false;
  });
  busyButtons.clear();
});

// An outcome notice ([data-autoclose]) goes away after 5 seconds. Pointing at it or
// focusing inside it holds the time; leaving starts it again. A notice with the grant just
// requested, or an error, never has the attribute.
document.querySelectorAll("[data-autoclose]").forEach((notice) => {
  let timer;
  const start = () => { clearTimeout(timer); timer = setTimeout(() => notice.remove(), 5000); };
  const stop = () => clearTimeout(timer);
  notice.addEventListener("mouseenter", stop);
  notice.addEventListener("focusin", stop);
  notice.addEventListener("mouseleave", start);
  notice.addEventListener("focusout", start);
  start();
});
