(function () {
  const consoleEl = document.getElementById("console");
  const consoleFilterInput = document.getElementById("console-filter");
  const runIndicator = document.getElementById("run-indicator");
  const updateButtons = () => document.querySelectorAll(".update-btn, #update-selected");

  // All log lines seen this page load, kept client-side so the filter box
  // can re-slice them by stack name without another round trip - capped
  // so a long-running instance left open in a tab doesn't grow forever.
  const MAX_CONSOLE_ENTRIES = 2000;
  let consoleEntries = [];
  let consoleFilter = "";

  function matchesFilter(stack, line) {
    return !consoleFilter ||
      stack.toLowerCase().includes(consoleFilter) ||
      line.toLowerCase().includes(consoleFilter);
  }

  function renderConsole() {
    consoleEl.textContent = consoleEntries
      .filter((e) => matchesFilter(e.stack, e.line))
      .map((e) => "[" + e.stack + "] " + e.line)
      .join("\n");
    consoleEl.scrollTop = consoleEl.scrollHeight;
  }

  function log(stack, line) {
    consoleEntries.push({ stack, line });
    if (consoleEntries.length > MAX_CONSOLE_ENTRIES) {
      consoleEntries = consoleEntries.slice(-MAX_CONSOLE_ENTRIES);
    }
    if (matchesFilter(stack, line)) {
      consoleEl.textContent += (consoleEl.textContent ? "\n" : "") + "[" + stack + "] " + line;
      consoleEl.scrollTop = consoleEl.scrollHeight;
    }
  }

  if (consoleFilterInput) {
    consoleFilterInput.addEventListener("input", () => {
      consoleFilter = consoleFilterInput.value.trim().toLowerCase();
      renderConsole();
    });
  }

  function setActive(active) {
    runIndicator.hidden = !active;
    updateButtons().forEach((b) => (b.disabled = active));
  }

  function formatBytes(n) {
    if (!n) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB"];
    let v = n,
      i = 0;
    while (v >= 1024 && i < units.length - 1) {
      v /= 1024;
      i++;
    }
    return (i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)) + " " + units[i];
  }

  function escapeAttr(s) {
    return String(s).replace(/"/g, "&quot;");
  }

  const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

  function ordinal(n) {
    if (n % 100 >= 11 && n % 100 <= 13) return n + "th";
    switch (n % 10) {
      case 1: return n + "st";
      case 2: return n + "nd";
      case 3: return n + "rd";
      default: return n + "th";
    }
  }

  // "Aug 11th, 15:19" - one consistent format everywhere a timestamp is
  // shown, whether it came from the initial page render or a live SSE
  // update, instead of mixing a server-side format with the browser's
  // locale-dependent toLocaleString().
  function formatTimestamp(iso) {
    if (!iso) return "never";
    const d = new Date(iso);
    if (isNaN(d)) return "never";
    const hh = String(d.getHours()).padStart(2, "0");
    const mm = String(d.getMinutes()).padStart(2, "0");
    return MONTHS[d.getMonth()] + " " + ordinal(d.getDate()) + ", " + hh + ":" + mm;
  }

  function renderLastRun(s) {
    let html = '<time class="ts" datetime="' + escapeAttr(s.lastRun || "") + '">' + formatTimestamp(s.lastRun) + "</time>";
    if (s.lastError) {
      html += ' <span class="err" title="' + escapeAttr(s.lastError) + '">⚠</span>';
    }
    return html;
  }

  function formatAllTimestamps() {
    document.querySelectorAll("time.ts[datetime]").forEach((el) => {
      const iso = el.getAttribute("datetime");
      if (iso) el.textContent = formatTimestamp(iso);
    });
  }

  // Chips for stacks currently updating, shown in the strip above the table
  // in addition to (not instead of) each stack's own row, which always stays
  // put in alphabetical order. Map preserves insertion order, so chips line
  // up in the order their updates started.
  const updatingStrip = document.getElementById("updating-strip");
  const stripChips = new Map();

  // Both .updating-strip and the table head are sticky, stacked one above
  // the other - the table head's CSS top offset (--strip-h) has to track the
  // strip's real height so it sits just below it instead of overlapping,
  // including when the strip grows to a second line as more chips wrap in.
  const stacksPanel = document.querySelector(".stacks-panel");
  if (stacksPanel && "ResizeObserver" in window) {
    const syncStripHeight = () => {
      stacksPanel.style.setProperty("--strip-h", updatingStrip.offsetHeight + "px");
    };
    new ResizeObserver(syncStripHeight).observe(updatingStrip);
  }

  function ensureChip(name) {
    let chip = stripChips.get(name);
    if (chip) return chip;
    chip = document.createElement("div");
    chip.className = "updating-chip";
    const nameEl = document.createElement("span");
    nameEl.className = "chip-name";
    nameEl.textContent = name;
    chip.appendChild(nameEl);
    chip.appendChild(document.createElement("div")).className = "chip-progress";
    updatingStrip.appendChild(chip);
    stripChips.set(name, chip);
    return chip;
  }

  function removeChip(name) {
    const chip = stripChips.get(name);
    if (!chip) return;
    chip.remove();
    stripChips.delete(name);
  }

  function applyStackStatus(s) {
    const pill = document.getElementById("pill-" + s.name);
    if (pill) {
      pill.textContent = s.state;
      pill.className = "pill pill-" + s.state;
    }
    const cell = document.getElementById("lastrun-" + s.name);
    if (cell && s.state !== "updating") {
      cell.innerHTML = renderLastRun(s);
    }
    if (s.state === "updating") {
      ensureChip(s.name);
    } else {
      removeChip(s.name);
    }
  }

  // Shared by both the table row's "Updated" cell and the stack's strip
  // chip (if it has one), so the two surfaces never drift out of sync.
  function progressHTML(p) {
    const phaseLabel = p.phase === "pull" ? "Pulling" : "Starting";
    let detail;
    if (p.phase === "pull" && p.total > 0) {
      detail = formatBytes(p.current) + " / " + formatBytes(p.total);
    } else if (p.phase === "up" && p.total > 0) {
      detail = p.current + "/" + p.total + " containers";
    } else {
      detail = p.text || phaseLabel;
    }
    return (
      '<div class="bar" title="' + escapeAttr(detail) + '">' +
      '<div class="bar-fill" style="width:' + p.percent + '%"></div>' +
      "</div>" +
      '<div class="progress-label">' + phaseLabel + " " + p.percent + "% · " + detail + "</div>"
    );
  }

  function applyProgress(p) {
    const html = progressHTML(p);
    const cell = document.getElementById("lastrun-" + p.stack);
    if (cell) cell.innerHTML = html;
    const chip = stripChips.get(p.stack);
    if (chip) chip.querySelector(".chip-progress").innerHTML = html;
  }

  function connect() {
    const es = new EventSource("/events");

    es.addEventListener("snapshot", (e) => {
      const snap = JSON.parse(e.data);
      setActive(snap.active);
      Object.values(snap.statuses || {}).forEach(applyStackStatus);
      Object.entries(snap.progress || {}).forEach(([name, p]) => {
        const s = snap.statuses[name];
        if (s && s.state === "updating") applyProgress(p);
      });
    });

    es.addEventListener("stack", (e) => {
      applyStackStatus(JSON.parse(e.data));
    });
    es.addEventListener("progress", (e) => applyProgress(JSON.parse(e.data)));

    es.addEventListener("log", (e) => {
      const d = JSON.parse(e.data);
      log(d.stack, d.line);
    });

    es.addEventListener("done", () => setActive(false));

    es.onopen = () => setActive(false);
    es.onerror = () => {
      es.close();
      setTimeout(connect, 2000);
    };
  }

  document.body.addEventListener("htmx:beforeRequest", (e) => {
    const el = e.target;
    if (el && (el.id === "update-selected" || el.id === "final-step-run-btn" || el.classList.contains("update-btn"))) {
      setActive(true);
    }
    if (el && el.id === "update-selected") {
      // Updating stacks get a chip in the strip at the top of the panel -
      // scroll there so it's visible as the run kicks off, rather than
      // wherever the list happened to be scrolled to.
      const panel = document.querySelector(".stacks-panel");
      if (panel) panel.scrollTo({ top: 0, behavior: "smooth" });
    }
  });

  // Post-update-steps editor: a single shared <dialog>, repointed at
  // whichever stack's "post-btn" was clicked.
  const postDialog = document.getElementById("post-dialog");
  const postForm = document.getElementById("post-form");
  const postInput = document.getElementById("post-script-input");
  const postContainerInput = document.getElementById("post-container-input");
  const postDialogName = document.getElementById("post-dialog-name");

  document.body.addEventListener("click", (e) => {
    // .post-btn is reused (for its on/off styling) by the toolbar's
    // final-step button too, which has no data-name - only per-row
    // buttons do, so that's the reliable way to tell them apart here.
    const btn = e.target.closest(".post-btn");
    if (!btn || !btn.dataset.name) return;
    const name = btn.dataset.name;
    postForm.setAttribute("hx-post", "/api/stacks/" + name + "/post-script");
    postForm.setAttribute("hx-target", "#stack-" + name);
    htmx.process(postForm); // pick up the hx-post/hx-target values just set
    postInput.value = btn.dataset.script || "";
    postContainerInput.value = btn.dataset.container || "";
    postDialogName.textContent = name;
    postDialog.showModal();
  });

  document.getElementById("post-cancel-btn").addEventListener("click", () => postDialog.close());

  // Final-step editor: global (not per-stack) settings dialog.
  const finalStepDialog = document.getElementById("final-step-dialog");
  const finalStepForm = document.getElementById("final-step-form");
  const finalStepInput = document.getElementById("final-step-input");
  const finalStepEnabled = document.getElementById("final-step-enabled");

  document.getElementById("final-step-btn").addEventListener("click", (e) => {
    const btn = e.currentTarget;
    finalStepInput.value = btn.dataset.script || "";
    finalStepEnabled.checked = !!btn.dataset.enabled;
    finalStepDialog.showModal();
  });

  document.getElementById("final-step-cancel-btn").addEventListener("click", () => finalStepDialog.close());

  const finalStepRunBtn = document.getElementById("final-step-run-btn");

  document.body.addEventListener("htmx:afterRequest", (e) => {
    if (e.target === postForm && e.detail.successful) {
      postDialog.close();
    }
    if (e.target === finalStepForm && e.detail.successful) {
      finalStepDialog.close();
    }
    if (e.target === finalStepRunBtn) {
      if (e.detail.successful) {
        finalStepDialog.close();
      } else {
        setActive(false);
        alert(e.detail.xhr.responseText || "Could not run final step.");
      }
    }
  });

  formatAllTimestamps();
  // Rescan/toggle responses swap in fresh rows via htmx, which come from
  // the server with a raw datetime attribute, no formatted text yet.
  document.body.addEventListener("htmx:afterSwap", () => {
    formatAllTimestamps();
  });

  connect();
})();
