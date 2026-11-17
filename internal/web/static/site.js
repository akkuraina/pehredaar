// Pehredaar's site script. Everything here is an enhancement: pages and
// forms work without it. No libraries, no build step.
(() => {
  "use strict";
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const calm = matchMedia("(prefers-reduced-motion: reduce)").matches;

  // ---- the owl's eyes ------------------------------------------------------
  // Pupils lean toward the pointer (or wander on touch screens). It never
  // blinks: that's the whole point of Pehredaar.
  const owls = $$(".owl-hero");
  const look = (owl, x, y) => {
    for (const p of $$(".pupil", owl)) {
      const r = p.getBoundingClientRect();
      const cx = r.left + r.width / 2, cy = r.top + r.height / 2;
      const a = Math.atan2(y - cy, x - cx);
      const d = Math.min(9, Math.hypot(x - cx, y - cy) / 30);
      p.style.transform = `translate(${Math.cos(a) * d}px, ${Math.sin(a) * d}px)`;
    }
  };
  if (owls.length && !calm) {
    let pending = false;
    addEventListener("pointermove", (e) => {
      if (pending) return;
      pending = true;
      requestAnimationFrame(() => { owls.forEach((o) => look(o, e.clientX, e.clientY)); pending = false; });
    }, { passive: true });
    if (matchMedia("(hover: none)").matches) {
      setInterval(() => owls.forEach((o) => {
        const r = o.getBoundingClientRect();
        look(o, r.left + Math.random() * r.width * 2 - r.width / 2, r.top + Math.random() * r.height * 2 - r.height / 2);
      }), 2400);
    }
  }
  const startle = (target) => owls.forEach((o) => {
    o.classList.remove("startled"); void o.getBBox(); o.classList.add("startled");
    if (target) { const r = target.getBoundingClientRect(); look(o, r.left + r.width / 2, r.top + 20); }
  });

  const say = (bubble, text) => {
    if (!bubble || !text) return;
    bubble.textContent = text;
    bubble.classList.remove("say"); void bubble.offsetWidth; bubble.classList.add("say");
  };

  // ---- hero: the word that changed quietly --------------------------------
  const cycle = $(".cycle");
  if (cycle && !calm) {
    const words = $$("span", cycle);
    cycle.classList.add("js");
    let i = 0;
    words[0].classList.add("on");
    setInterval(() => {
      const prev = words[i];
      i = (i + 1) % words.length;
      prev.classList.replace("on", "off");
      words[i].classList.remove("off");
      words[i].classList.add("on");
      setTimeout(() => prev.classList.remove("off"), 500);
    }, 2200);
  }

  // The landing owl has a few things to say.
  const heroBubble = $("#owl-bubble");
  if (heroBubble && !calm) {
    const lines = ["👀 I don't blink.", "Dates move. I notice.", "Still watching…", "Psst. Corrigenda hide in PDFs.", "You study. I'll watch."];
    let n = 0;
    setInterval(() => say(heroBubble, lines[++n % lines.length]), 5200);
  }

  // ---- scroll reveal ---------------------------------------------------------
  if ("IntersectionObserver" in window && !calm) {
    document.documentElement.classList.add("js-reveal");
    const io = new IntersectionObserver((es) => es.forEach((e) => {
      if (e.isIntersecting) { e.target.classList.add("seen"); io.unobserve(e.target); }
    }), { rootMargin: "0px 0px -8% 0px" });
    $$(".reveal").forEach((el) => io.observe(el));
  }

  // ---- "4 min ago" that stays true -----------------------------------------
  const ago = (iso) => {
    const t = Date.parse(iso);
    if (!t) return "";
    const s = Math.max(0, (Date.now() - t) / 1000);
    if (s < 60) return s < 15 ? "just now" : `${Math.floor(s)}s ago`;
    if (s < 3600) return `${Math.floor(s / 60)} min ago`;
    if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
    const d = Math.floor(s / 86400);
    if (d === 1) return "yesterday";
    if (d < 7) return `${d} days ago`;
    return new Date(t).toLocaleDateString("en-IN", { day: "numeric", month: "short", year: "numeric" });
  };
  const tick = () => $$(".ago[datetime]").forEach((el) => { el.textContent = ago(el.getAttribute("datetime")); });
  setInterval(tick, 10000);

  // ---- counters --------------------------------------------------------------
  const countTo = (el, to) => {
    const from = parseInt(el.textContent, 10) || 0;
    if (from === to) return;
    el.classList.remove("bump"); void el.offsetWidth; el.classList.add("bump");
    if (calm) { el.textContent = to; return; }
    const start = performance.now(), dur = 700;
    const step = (now) => {
      const k = Math.min(1, (now - start) / dur);
      el.textContent = Math.round(from + (to - from) * (1 - Math.pow(1 - k, 3)));
      if (k < 1) requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  };
  // Count up from zero on first view, so the numbers feel alive.
  if (!calm) $$("[data-count]").forEach((el) => {
    const to = parseInt(el.textContent, 10) || 0;
    el.textContent = "0";
    setTimeout(() => countTo(el, to), 300);
  });

  // ---- toast ----------------------------------------------------------------
  const toastEl = $(".toast");
  let toastTimer;
  const toast = (text) => {
    if (!toastEl) return;
    toastEl.textContent = text;
    toastEl.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toastEl.hidden = true; }, 3500);
  };

  // ---- live updates (Server-Sent Events) ----------------------------------
  const events = document.body.dataset.events;
  const cards = $("#cards");
  if (events && "EventSource" in window) {
    const es = new EventSource(events);
    es.addEventListener("change", (e) => {
      const msg = JSON.parse(e.data);
      if (!cards || $(`[data-id="${msg.id}"]`, cards)) return;
      const tpl = document.createElement("template");
      tpl.innerHTML = msg.html.trim();
      const card = tpl.content.firstElementChild;
      if (!card) return;
      $(".empty", cards)?.remove();
      card.classList.add("enter");
      cards.prepend(card);
      tick();
      startle(card);
      toast("👀 Caught one!");
      if (document.hidden) document.title = "👀 " + document.title.replace(/^👀 /, "");
    });
    es.addEventListener("pulse", (e) => {
      const p = JSON.parse(e.data);
      for (const k of ["feeds", "sources", "caught"]) {
        const el = $(`[data-count="${k}"]`);
        if (el && typeof p[k] === "number") countTo(el, p[k]);
      }
      const last = $("[data-last-check]");
      if (last && p.lastCheck) {
        last.setAttribute("datetime", new Date(p.lastCheck * 1000).toISOString());
        tick();
      }
    });
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden) document.title = document.title.replace(/^👀 /, "");
    });
  }

  // ---- spotlight: strike the old value, type the new one --------------------
  const morphs = $$(".morph");
  if (morphs.length && !calm && "IntersectionObserver" in window) {
    const play = () => morphs.forEach((m, i) => setTimeout(() => {
      const nw = $(".morph-new", m), text = nw.dataset.type;
      m.classList.remove("play"); void m.offsetWidth; m.classList.add("play");
      nw.textContent = "";
      nw.classList.add("typing");
      let j = 0;
      setTimeout(function type() {
        nw.textContent = text.slice(0, ++j);
        if (j < text.length) setTimeout(type, 45); else nw.classList.remove("typing");
      }, 1000);
    }, i * 400));
    let loop;
    new IntersectionObserver(([e]) => {
      if (e.isIntersecting) { play(); loop = setInterval(play, 7000); } else clearInterval(loop);
    }, { threshold: 0.5 }).observe(morphs[0].parentElement);
  }

  // ---- "show older" ----------------------------------------------------------
  const more = $("[data-more]");
  more?.addEventListener("click", async () => {
    more.disabled = true;
    try {
      const r = await fetch(`/feed/more?before=${more.dataset.more}`, { credentials: "same-origin" });
      if (r.status === 204) { more.remove(); return; }
      const tpl = document.createElement("template");
      tpl.innerHTML = await r.text();
      cards.append(...tpl.content.children);
      tick();
      const next = r.headers.get("X-More-Before");
      if (next) { more.dataset.more = next; more.disabled = false; } else more.remove();
    } catch { more.disabled = false; toast("Couldn't load more. Try again?"); }
  });

  // ---- the survey: one question at a time ----------------------------------
  const form = $("[data-survey]");
  if (form) {
    const steps = $$(".step", form);
    const back = $("[data-back]", form), next = $("[data-next]", form), submit = $("[data-submit]", form);
    const bar = $(".progress span", form);
    const bubble = $("#join-bubble");
    const emailField = $(".email-field", form);
    let at = 0;
    form.classList.add("js");

    const liveExamsChosen = () => $$('input[name="exam"]:checked', form).filter((i) => i.closest(".is-live"));
    const syncFeeds = () => {
      const chosen = new Set($$('input[name="exam"]:checked', form).map((i) => i.value));
      let any = false;
      for (const g of $$(".feed-group", form)) {
        const on = chosen.has(g.dataset.exam);
        g.hidden = !on;
        any ||= on;
      }
      $(".feed-none", form).hidden = any;
    };
    const syncEmail = () => {
      const via = $('input[name="via"]:checked', form)?.value;
      emailField.hidden = !(via === "email" || via === "both");
    };
    const problem = (i) => {
      if (i === 0 && !$('input[name="exam"]:checked', form)) return "Pick at least one exam first. 🦉";
      if (i === 2) {
        const via = $('input[name="via"]:checked', form)?.value;
        if (!via) return "How should I reach you?";
        const email = $('input[name="email"]', form);
        if ((via === "email" || via === "both") && !email.checkValidity()) return "That email doesn't look right.";
        if ((via === "email" || via === "both") && !email.value.trim()) return "I need your email for that.";
      }
      return "";
    };
    const show = (i, dir = 1) => {
      // Skip the feeds step when no chosen exam is live yet.
      if (i === 1 && !liveExamsChosen().length) i += dir;
      at = Math.max(0, Math.min(steps.length - 1, i));
      steps.forEach((s, k) => { s.classList.toggle("on", k === at); s.classList.toggle("back", dir < 0); });
      back.hidden = at === 0;
      next.hidden = at === steps.length - 1;
      submit.hidden = at !== steps.length - 1;
      bar.style.width = `${((at + 1) / steps.length) * 100}%`;
      say(bubble, steps[at].dataset.say);
      const first = $("input:not([type=hidden]):not(:disabled)", steps[at]);
      if (first && matchMedia("(hover: hover)").matches) first.focus({ preventScroll: true });
    };

    next.addEventListener("click", () => {
      const p = problem(at);
      if (p) { say(bubble, p); toast(p); return; }
      if (at === 0) syncFeeds();
      show(at + 1, 1);
    });
    back.addEventListener("click", () => show(at - 1, -1));
    form.addEventListener("change", (e) => {
      if (e.target.name === "exam") {
        syncFeeds();
        const n = $$('input[name="exam"]:checked', form).length;
        const live = e.target.closest(".is-live");
        if (e.target.checked) say(bubble, live ? `${e.target.value.toUpperCase()}: on it. 👀` : "Noted! Not live yet; I'll tell you when.");
        else if (!n) say(bubble, "Pick at least one…");
      }
      if (e.target.name === "via") syncEmail();
    });
    // Enter moves forward instead of submitting half a survey.
    form.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && e.target.tagName === "INPUT" && at < steps.length - 1) { e.preventDefault(); next.click(); }
    });
    form.addEventListener("submit", (e) => {
      for (let i = 0; i < steps.length; i++) {
        const p = problem(i);
        if (p) { e.preventDefault(); show(i); say(bubble, p); toast(p); return; }
      }
      submit.disabled = true;
      say(bubble, "On it! 🦉");
    });

    syncFeeds();
    syncEmail();
    // After a server-side error, start on the first step with a problem.
    const firstBad = $(".alert", form) ? [0, 2].find((i) => problem(i)) : undefined;
    show(firstBad ?? 0);
  }
})();
