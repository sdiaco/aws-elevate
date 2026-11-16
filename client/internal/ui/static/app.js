// The little JavaScript of the interface (design §11.1): selection, countdowns,
// polling of grants in transition and copyable ids. Everything else is server pages.
"use strict";

// Selection: [data-select-all] toggles the [data-select] boxes of its list; the
// .select-bar of the list shows how many are selected.
function updateSelection(list) {
  const boxes = [...list.querySelectorAll("[data-select]")];
  const picked = boxes.filter((b) => b.checked);
  boxes.forEach((b) => b.closest("tr").classList.toggle("selected", b.checked));
  const all = list.querySelector("[data-select-all]");
  if (all) {
    all.checked = boxes.length > 0 && picked.length === boxes.length;
    all.indeterminate = picked.length > 0 && picked.length < boxes.length;
  }
  const bar = list.querySelector(".select-bar");
  if (bar) {
    bar.classList.toggle("hidden", picked.length === 0);
    bar.querySelector("[data-selected-count]").textContent = picked.length;
  }
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
  const clear = e.target.closest("[data-select-clear]");
  if (clear) {
    const list = clear.closest("[data-list]");
    list.querySelectorAll("[data-select]").forEach((b) => { b.checked = false; });
    updateSelection(list);
  }
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
// 2, 4, 8 and then every 8 seconds, until the replacement has no data-poll.
const pollDelays = [2000, 4000, 8000];

function poll(el, attempt) {
  const delay = pollDelays[Math.min(attempt, pollDelays.length - 1)];
  setTimeout(async () => {
    try {
      const res = await fetch(el.dataset.poll, { headers: { Accept: "text/html" } });
      if (!res.ok) throw new Error(res.status);
      const tpl = document.createElement("template");
      tpl.innerHTML = (await res.text()).trim();
      const next = tpl.content.firstElementChild;
      el.replaceWith(next);
      tick();
      if (next.dataset.poll) poll(next, attempt + 1);
    } catch {
      el.title = "Status not refreshed: reload the page";
    }
  }, delay);
}

document.querySelectorAll("[data-list]").forEach(updateSelection);
document.querySelectorAll("[data-poll]").forEach((el) => poll(el, 0));
tick();
setInterval(tick, 1000);
