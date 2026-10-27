/* Drydock dashboard behaviour: confirmations, job polling, small niceties. */
(() => {
  "use strict";

  /* ---------- destructive action confirmation ---------- */

  document.addEventListener("submit", (event) => {
    const form = event.target;
    const submitter = event.submitter;
    const button = submitter && submitter.dataset && submitter.dataset.confirm
      ? submitter
      : form.querySelector("[data-confirm]");
    const message = button && button.dataset.confirm;
    if (message && !window.confirm(message)) {
      event.preventDefault();
      return;
    }
    if (button) {
      // Prevent accidental double submits. The page is replaced by a redirect
      // anyway, so there is nothing to restore.
      window.setTimeout(() => { button.disabled = true; }, 0);
    }
  });

  /* ---------- background job polling ---------- */

  const banner = document.getElementById("job-banner");
  if (banner) {
    let reloading = true;
    pollJob(banner.dataset.jobId, () => reloading, () => { reloading = false; });
  }

  async function pollJob(id, shouldReload, cancelReload) {
    const text = document.getElementById("job-text");
    const dismiss = document.getElementById("job-dismiss");
    let job;
    try {
      const res = await fetch("/jobs/" + encodeURIComponent(id), {
        headers: { Accept: "application/json" },
        credentials: "same-origin",
      });
      if (!res.ok) {
        throw new Error(res.status === 401 ? "session expired" : "HTTP " + res.status);
      }
      job = await res.json();
    } catch (err) {
      text.textContent = "Lost track of the job (" + err.message + "). Retrying…";
      window.setTimeout(() => pollJob(id, shouldReload, cancelReload), 2000);
      return;
    }

    if (job.status === "queued" || job.status === "running") {
      text.textContent = describeRunning(job);
      window.setTimeout(() => pollJob(id, shouldReload, cancelReload), 900);
      return;
    }

    banner.classList.add(job.status === "failed" ? "err" : "ok");
    const spinner = banner.querySelector(".spinner");
    if (spinner) spinner.remove();
    text.textContent = describeFinished(job);

    if (dismiss) {
      dismiss.hidden = false;
      dismiss.addEventListener("click", (event) => {
        event.preventDefault();
        cancelReload();
        banner.remove();
        stripJobParam();
      });
    }

    window.setTimeout(() => {
      if (shouldReload()) {
        stripJobParam(true);
      }
    }, 1800);
  }

  function describeRunning(job) {
    if (job.kind === "rollback") {
      return "Rolling back " + (job.container || "container") + "…";
    }
    if (job.kind === "check") {
      return "Checking registries for new images…";
    }
    return "Applying updates. Unhealthy replacements are rolled back automatically…";
  }

  function describeFinished(job) {
    if (job.kind === "rollback" && job.outcome) {
      return "Rollback " + job.status + ": " + job.outcome.container +
        (job.outcome.message ? " — " + job.outcome.message : "");
    }
    if (job.report) {
      const counts = countOutcomes(job.report.outcomes || []);
      const parts = [];
      if (counts.updated) parts.push(counts.updated + " updated");
      if (counts.failed) parts.push(counts.failed + " failed");
      if (counts.rolled_back) parts.push(counts.rolled_back + " rolled back");
      if (counts.pending) parts.push(counts.pending + " pending");
      if (counts.up_to_date) parts.push(counts.up_to_date + " up to date");
      const verb = job.kind === "check" ? "Check" : "Update";
      return verb + " finished: " + (parts.length ? parts.join(", ") : "nothing to do");
    }
    return job.kind + " " + job.status;
  }

  function countOutcomes(outcomes) {
    const counts = {};
    for (const o of outcomes) {
      counts[o.status] = (counts[o.status] || 0) + 1;
    }
    return counts;
  }

  function stripJobParam(replace) {
    const url = new URL(window.location.href);
    if (!url.searchParams.has("job")) return;
    url.searchParams.delete("job");
    if (replace) {
      window.location.replace(url.toString());
    } else if (window.history && window.history.replaceState) {
      window.history.replaceState({}, "", url.toString());
    }
  }

  /* ---------- keyboard shortcut: "/" focuses search ---------- */

  document.addEventListener("keydown", (event) => {
    if (event.key !== "/" || event.ctrlKey || event.metaKey || event.altKey) return;
    const tag = (event.target.tagName || "").toLowerCase();
    if (tag === "input" || tag === "textarea" || tag === "select" || event.target.isContentEditable) return;
    const search = document.querySelector('input[type="search"]');
    if (search) {
      event.preventDefault();
      search.focus();
    }
  });
})();
