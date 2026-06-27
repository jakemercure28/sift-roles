'use strict';

// ── Header popovers (nav menu, filter panel) ─────────────────────────
const PANEL_IDS = ['nav-menu', 'loc-panel'];

function togglePanel(id) {
  const panel = document.getElementById(id);
  if (!panel) return;
  const isOpen = panel.classList.toggle('open');
  document.getElementById(id + '-btn')?.classList.toggle('active', isOpen);
  if (isOpen) {
    PANEL_IDS.filter((p) => p !== id).forEach((p) => {
      document.getElementById(p)?.classList.remove('open');
      document.getElementById(p + '-btn')?.classList.remove('active');
    });
  }
}

function toggleNavMenu() {
  togglePanel('nav-menu');
}
function toggleLocPanel() {
  togglePanel('loc-panel');
}

// ── Mobile sidebar drawer ────────────────────────────────────────────
// On narrow viewports the sidebar is off-canvas; the content-header menu button,
// the backdrop, and the in-drawer close button all toggle the shell class.
function toggleSidebar(force) {
  const shell = document.getElementById('app-shell');
  if (!shell) return;
  const open =
    typeof force === 'boolean'
      ? force
      : !shell.classList.contains('sidebar-open');
  shell.classList.toggle('sidebar-open', open);
}

// Apply a location preference in a single clean navigation: the server reads
// these params, persists them to location.json, and renders the filtered list.
// metroKey '' means "All locations". includeUnknown defaults to whatever the
// current toggle state is (read off the rendered switch).
function setLocationPref(metroKey, includeUnknown) {
  if (typeof includeUnknown === 'undefined') {
    const toggle = document.querySelector('.loc-unlisted-row');
    includeUnknown = toggle
      ? toggle.getAttribute('aria-checked') !== 'false'
      : true;
  }
  // Remember the chosen metro client-side so a hard refresh shows it immediately,
  // before the async auth hydration re-fetches the saved server prefs. The server
  // (location.json) stays the source of truth for the actual job filtering; this
  // only restores the dropdown label across the unauthenticated initial render.
  try {
    localStorage.setItem('dashboard-metro', metroKey || '');
  } catch {
    /* storage may be unavailable */
  }
  const url = new URL(window.location.href);
  url.searchParams.set('setMetro', metroKey || '');
  url.searchParams.set('setUnlisted', includeUnknown ? '1' : '0');
  url.searchParams.delete('page');
  navigateDashboardUrl(url.toString());
}

// Remote-only is an independent toggle: send just setRemote so the server merge
// preserves any selected metro / unlisted preference.
function setRemotePref(remoteOnly) {
  const url = new URL(window.location.href);
  url.searchParams.set('setRemote', remoteOnly ? '1' : '0');
  url.searchParams.delete('page');
  navigateDashboardUrl(url.toString());
}

// On a hard refresh the document loads unauthenticated, so the server renders the
// location dropdown as "All locations" (the empty tenant has no saved prefs) until
// the async auth hydration swaps the filters back in. Bridge that gap optimistically
// from the last selection saved in setLocationPref, matching it against the rendered
// menu so the trigger shows the right metro right away. If hydration later re-renders
// the filters from server prefs, that authoritative label simply replaces this one.
function applySavedLocation() {
  let saved;
  try {
    saved = localStorage.getItem('dashboard-metro');
  } catch {
    return;
  }
  if (saved === null) return; // never chosen here; leave the server-rendered default
  const panel = document.getElementById('loc-panel');
  const trigger = document.getElementById('loc-panel-btn');
  if (!panel || !trigger) return;
  const items = panel.querySelectorAll('.menu-item:not(.loc-toggle-row)');
  let matched = null;
  items.forEach((it) => {
    if (it.getAttribute('onclick') === "setLocationPref('" + saved + "')")
      matched = it;
  });
  if (!matched) return;
  items.forEach((it) => it.classList.toggle('active', it === matched));
  const span = trigger.querySelector('span');
  if (span) span.textContent = matched.textContent.trim();
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', applySavedLocation);
} else {
  applySavedLocation();
}

// ── Light/dark theme toggle ──────────────────────────────────────────
// The effective theme is the data-theme attribute if set, else the OS
// preference. toggleTheme() flips it, persists the choice, and swaps the icon.
// A pre-paint script in <head> applies the saved choice before this loads.
function currentTheme() {
  const t = document.documentElement.getAttribute('data-theme');
  if (t === 'light' || t === 'dark') return t;
  return window.matchMedia &&
    window.matchMedia('(prefers-color-scheme: dark)').matches
    ? 'dark'
    : 'light';
}

function syncThemeIcon() {
  const theme = currentTheme();
  const moon = document.getElementById('theme-icon-moon');
  const sun = document.getElementById('theme-icon-sun');
  // Show the icon for the mode you'd switch *to*: moon while in light, sun while in dark.
  if (moon) moon.style.display = theme === 'dark' ? 'none' : '';
  if (sun) sun.style.display = theme === 'dark' ? '' : 'none';
}

function toggleTheme() {
  const next = currentTheme() === 'dark' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  try {
    localStorage.setItem('dashboard-theme', next);
  } catch {
    /* storage may be unavailable */
  }
  syncThemeIcon();
}

document.addEventListener('click', (e) => {
  // Header popovers
  PANEL_IDS.forEach((id) => {
    const panel = document.getElementById(id);
    const btn = document.getElementById(id + '-btn');
    if (
      panel?.classList.contains('open') &&
      !panel.contains(e.target) &&
      btn &&
      !btn.contains(e.target)
    ) {
      panel.classList.remove('open');
      btn.classList.remove('active');
    }
  });
  // Choosing a destination in the mobile drawer closes it.
  if (e.target.closest && e.target.closest('.sidebar .nav-item[href]'))
    toggleSidebar(false);
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') toggleSidebar(false);
});

let _toastTimer = null;
function showToast(msg, color, undoFn) {
  const t = document.getElementById('toast');
  t.style.background = color || 'var(--green)';
  if (undoFn) {
    t.textContent = '';
    const span = document.createElement('span');
    span.textContent = msg;
    const btn = document.createElement('button');
    btn.className = 'toast-undo';
    btn.textContent = 'Undo';
    btn.onclick = () => {
      t.classList.remove('show');
      undoFn();
    };
    t.appendChild(span);
    t.appendChild(btn);
  } else {
    t.textContent = msg;
  }
  t.classList.add('show');
  if (_toastTimer) clearTimeout(_toastTimer);
  _toastTimer = setTimeout(
    () => t.classList.remove('show'),
    undoFn ? 5000 : 2000
  );
}

// ── Single-level undo for accidental archive/reject/ghost/close ──────────────
// We remember just the last destructive change and restore it via /pipeline,
// then reload so a row that was faded out of the list comes back.
let _pendingUndo = null; // { id, prevValue }

function registerUndo(id, prevValue) {
  _pendingUndo = { id, prevValue: prevValue || '' };
}

async function performUndo() {
  if (!_pendingUndo) return;
  const { id, prevValue } = _pendingUndo;
  _pendingUndo = null;
  try {
    await fetch('/pipeline', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, value: prevValue }),
    });
  } finally {
    location.reload();
  }
}

document.addEventListener('keydown', (e) => {
  if (
    (e.metaKey || e.ctrlKey) &&
    !e.shiftKey &&
    (e.key === 'z' || e.key === 'Z')
  ) {
    const el = e.target;
    const tag = ((el && el.tagName) || '').toLowerCase();
    if (
      tag === 'input' ||
      tag === 'textarea' ||
      tag === 'select' ||
      (el && el.isContentEditable)
    )
      return;
    if (_pendingUndo) {
      e.preventDefault();
      performUndo();
    }
  }
});

// Trigger-agnostic: both the row chevron and the score badge call this, so it
// finds the card from the reasoning panel rather than the clicked element. The
// chevron (if present) reflects the open state via its own class.
function toggleReasoning(id) {
  const panel = document.getElementById('reasoning-' + id);
  if (!panel) return;
  const card = panel.closest('.job-card');
  if (!card) return;
  const expanded = card.classList.toggle('expanded');
  card
    .querySelector('.btn-reasoning-toggle')
    ?.classList.toggle('open', expanded);
}

// ── "Scrape now" on the scraper-staleness banner ─────────────────────────────
// Kicks off a scrape via the Go engine (the only writer of the scrape heartbeat),
// then polls the heartbeat until the cycle finishes. On success we reload so the
// banner re-renders and clears; on failure we surface the engine's error. The
// banner button + #scrape-now-status span are rendered in lib/dashboard-html.js.
function _setScrapeStatus(msg) {
  const el = document.getElementById('scrape-now-status');
  if (el) el.textContent = msg || '';
}

function _resetScrapeBtn(btn) {
  if (!btn) return;
  btn.disabled = false;
  btn.style.opacity = '';
  btn.style.cursor = 'pointer';
}

function scrapeNow(btn) {
  if (btn) {
    btn.disabled = true;
    btn.style.opacity = '0.6';
    btn.style.cursor = 'default';
  }
  _setScrapeStatus('Starting scrape...');
  fetch('/api/scrape-now', { method: 'POST' })
    .then((r) => r.json().then((d) => ({ ok: r.ok, d })))
    .then(({ ok, d }) => {
      if (d && d.busy) {
        // A scrape was already running; still poll so we report when it lands.
        _setScrapeStatus('A scrape is already running...');
        _pollScrapeHeartbeat(btn);
        return;
      }
      if (!ok || !d || !d.ok) {
        const msg = (d && d.error) || 'Could not start scrape';
        _setScrapeStatus(msg);
        _resetScrapeBtn(btn);
        showToast(msg, 'var(--red)');
        return;
      }
      _setScrapeStatus('Scraping...');
      _pollScrapeHeartbeat(btn);
    })
    .catch(() => {
      _setScrapeStatus('Could not reach the server');
      _resetScrapeBtn(btn);
      showToast('Could not reach the server', 'var(--red)');
    });
}

// last_attempt_at is stamped on every completed cycle (success or failure), so a
// change to it means the scrape we kicked off has finished. We capture it as a
// baseline, then poll until it moves.
function _pollScrapeHeartbeat(btn) {
  const intervalMs = 5000;
  const capMs = 4 * 60 * 1000;
  let elapsed = 0;
  let baseline = null;

  const fetchHb = () =>
    fetch('/api/scraper-heartbeat')
      .then((r) => r.json())
      .then((d) => (d && d.heartbeat) || null);

  const tick = () => {
    setTimeout(() => {
      elapsed += intervalMs;
      fetchHb()
        .then((hb) => {
          const attempt = hb && hb.last_attempt_at;
          if (attempt && attempt !== baseline) {
            if (hb.status === 'error') {
              const msg = hb.error
                ? 'Scrape failed: ' + hb.error
                : 'Scrape failed';
              _setScrapeStatus(msg);
              _resetScrapeBtn(btn);
              showToast('Scrape failed', 'var(--red)');
              return;
            }
            _setScrapeStatus('Done. Refreshing...');
            showToast('Scrape complete', 'var(--green)');
            setTimeout(() => window.location.reload(), 700);
            return;
          }
          if (elapsed >= capMs) {
            _setScrapeStatus('Still scraping. Check back shortly.');
            _resetScrapeBtn(btn);
            return;
          }
          tick();
        })
        .catch(() => {
          if (elapsed >= capMs) {
            _resetScrapeBtn(btn);
            return;
          }
          tick();
        });
    }, intervalMs);
  };

  // Establish the baseline, then start polling for it to change.
  fetchHb()
    .then((hb) => {
      baseline = (hb && hb.last_attempt_at) || null;
      tick();
    })
    .catch(tick);
}

// Market Research re-run: the POST blocks on a long Gemini analysis, so swap
// the button into a busy state the moment the form submits. Returning true
// lets the native POST proceed; the disabled flag only blocks re-clicks.
function mrRunSubmit(form) {
  const btn = form.querySelector('button[type="submit"]');
  if (btn) {
    if (btn.disabled) return false;
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span>Analyzing, 30 to 60 seconds';
  }
  return true;
}
