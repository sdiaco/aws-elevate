// The little JavaScript of the interface (design §11.1): selection, countdowns,
// polling of grants in transition, details read when opened and copyable ids.
// Everything else is server pages.
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
    btn.disabled = picked.length === 0 || !allowed;
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

function poll(el, attempt) {
  const delay = pollDelays[Math.min(attempt, pollDelays.length - 1)];
  setTimeout(async () => {
    try {
      const res = await fetch(el.dataset.poll, { headers: { Accept: "text/html" } });
      if (!res.ok) throw new Error(res.status);
      const tpl = document.createElement("template");
      tpl.innerHTML = (await res.text()).trim();
      const next = tpl.content.firstElementChild;
      const moved = next.dataset.state !== el.dataset.state;
      const box = el.querySelector("[data-select]");
      const kept = next.querySelector("[data-select]");
      if (box && kept) kept.checked = box.checked;
      el.replaceWith(next);
      const list = next.closest("[data-list]");
      if (list) updateSelection(list);
      tick();
      if (next.dataset.poll) poll(next, moved ? 0 : attempt + 1);
    } catch {
      el.title = "Status not refreshed: reload the page";
    }
  }, delay);
}

// Details read when opened: <details data-load="/address"> fills its
// [data-load-target] with the HTML at that address, once. With data-load-from="<input id>"
// the address ends with that input's value, and changing the input reads it again.
document.addEventListener("toggle", async (e) => {
  const d = e.target;
  if (!(d instanceof HTMLDetailsElement) || !d.open || !d.dataset.load || d.dataset.loaded) return;
  d.dataset.loaded = "1";
  const box = d.querySelector("[data-load-target]");
  const from = d.dataset.loadFrom && document.getElementById(d.dataset.loadFrom);
  const address = d.dataset.load + (from ? encodeURIComponent(from.value) : "");
  try {
    const res = await fetch(address, { headers: { Accept: "text/html" } });
    if (!res.ok) throw new Error(res.status);
    box.innerHTML = await res.text();
    tick();
  } catch {
    box.textContent = "Not loaded: close and open again, or reload the page.";
    delete d.dataset.loaded;
  }
}, true); // toggle does not bubble

document.addEventListener("change", (e) => {
  document.querySelectorAll(`details[data-load-from="${e.target.id}"]`).forEach((d) => {
    delete d.dataset.loaded;
    d.querySelector("[data-load-target]").innerHTML = '<p class="muted">Loading…</p>';
    d.open = false;
  });
});

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

// A choice that is a navigation: <input data-autosubmit> sends its form when changed.
document.addEventListener("change", (e) => {
  if (e.target.matches("[data-autosubmit]")) e.target.form.requestSubmit();
});

document.querySelectorAll("form[data-steps]").forEach(steps);
document.querySelectorAll("[data-list]").forEach(updateSelection);
document.querySelectorAll("[data-poll]").forEach((el) => poll(el, 0));
tick();
setInterval(tick, 1000);
