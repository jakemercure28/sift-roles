'use strict';

// ---------------------------------------------------------------------------
// Scoring progress banner — keeps users informed while the background pipeline
// imports, scores, and classifies. Polls /api/scoring-progress every 7s, hides
// itself when there's nothing left to score, and triggers a single page reload
// once new scores have landed (so the empty list fills in).
// ---------------------------------------------------------------------------

(function () {
  const banner = document.getElementById('scoring-progress-banner');
  if (!banner) return;
  const msgEl = document.getElementById('scoring-progress-msg');
  const barEl = document.getElementById('scoring-progress-bar');
  const scrambleEl = document.getElementById('scoring-progress-scramble');
  let initialScored = null;
  let reloadedOnce = false;
  let dismissed = false; // user closed the banner with the X
  let lastMsg = ''; // last message rendered, so we only re-scramble on change
  let lastPollData = null; // latest /api/scoring-progress payload

  // The scrape digest only belongs on the job inbox views; on Applications,
  // Interviews, Offers, and the report pages it is noise.
  function bannerAllowed(filter) {
    const f =
      typeof filter === 'string'
        ? filter
        : new URLSearchParams(location.search).get('filter') || 'not-applied';
    return f === 'not-applied' || f === 'all';
  }

  // Show a short headline only: drop any trailing detail after a dash and cap at
  // a word boundary. No ellipsis — the text is shortened, not visually clipped.
  function shortenStatus(text) {
    let head = String(text)
      .split(/\s[—–-]\s/)[0]
      .trim();
    const MAX = 120;
    if (head.length > MAX) head = head.slice(0, MAX).replace(/\s+\S*$/, '');
    return head;
  }

  // Polls fire every 7s with the same string, so skip identical updates.
  function setBannerMessage(text) {
    const head = shortenStatus(text);
    if (head === lastMsg) return;
    lastMsg = head;
    if (msgEl) msgEl.textContent = head;
  }

  // The X button calls this. Once dismissed, the poll never re-shows the banner.
  window.dismissScoringBanner = function () {
    dismissed = true;
    banner.style.display = 'none';
  };

  function timeAgoShort(iso) {
    const t = Date.parse(iso);
    if (isNaN(t)) return '';
    const mins = Math.floor((Date.now() - t) / 60000);
    if (mins < 1) return 'just now';
    if (mins < 60) return mins + 'm ago';
    const hrs = Math.floor(mins / 60);
    if (hrs < 24) return hrs + 'h ago';
    return Math.floor(hrs / 24) + 'd ago';
  }

  function countNoun(n, singular, pluralForm) {
    return n + ' ' + (n === 1 ? singular : pluralForm);
  }

  // Local wall-clock time (e.g. "6:00 PM") for an ISO timestamp, used to tell the
  // user when the next scrape runs. Empty for an unparseable input.
  function clockShort(iso) {
    const t = Date.parse(iso);
    if (isNaN(t)) return '';
    return new Date(t).toLocaleTimeString([], {
      hour: 'numeric',
      minute: '2-digit',
    });
  }

  // What the last pipeline run actually did (scrape result + any discovery
  // additions) plus when the next scrape runs. Empty when there is no fresh
  // successful scrape, so the caller can fall back to a generic line.
  function buildScrapeDigest(d) {
    if (!d || !d.lastScrapeAt || d.lastScrapeStatus !== 'ok') return '';
    const t = Date.parse(d.lastScrapeAt);
    if (isNaN(t) || Date.now() - t > 13 * 3600 * 1000) return '';
    const ago = timeAgoShort(d.lastScrapeAt);
    if (!ago) return '';
    let msg = 'Last scrape ' + ago + ': ';
    msg +=
      d.lastScrapeInserted > 0
        ? countNoun(d.lastScrapeInserted, 'new job', 'new jobs')
        : 'no new jobs';
    if (d.discoveryAdded > 0 && d.discoveryAt) {
      const dt = Date.parse(d.discoveryAt);
      if (!isNaN(dt) && Date.now() - dt < 24 * 3600 * 1000) {
        msg +=
          ' · discovery added ' +
          countNoun(d.discoveryAdded, 'company', 'companies');
      }
    }
    const next = clockShort(d.nextScrapeAt);
    if (next) msg += ' · next scrape ' + next;
    return msg;
  }

  function showComplete() {
    if (!bannerAllowed()) {
      banner.style.display = 'none';
      return;
    }
    if (scrambleEl) scrambleEl.style.display = 'none';
    const next = lastPollData && clockShort(lastPollData.nextScrapeAt);
    setBannerMessage(
      buildScrapeDigest(lastPollData) ||
        (next
          ? 'All jobs scored. Next scrape ' + next + '.'
          : 'All jobs scored.')
    );
    if (barEl) barEl.style.width = '100%';
    banner.style.display = 'flex';
  }

  // If scoring just finished on the previous load and triggered a reload, show
  // the completion message once on this fresh page.
  try {
    if (sessionStorage.getItem('scoringJustFinished')) {
      sessionStorage.removeItem('scoringJustFinished');
      sessionStorage.removeItem('scoringFinishedTotal');
      showComplete();
    }
  } catch {
    /* sessionStorage may be unavailable */
  }

  function formatEta(seconds) {
    function pad2(n) {
      return (n < 10 ? '0' : '') + n;
    }
    if (typeof seconds !== 'number' || !isFinite(seconds) || seconds <= 0)
      return '~00:01';
    const totalMinutes = Math.max(1, Math.ceil(seconds / 60));
    return (
      '~' + pad2(Math.floor(totalMinutes / 60)) + ':' + pad2(totalMinutes % 60)
    );
  }

  function poll() {
    fetch('/api/scoring-progress')
      .then(function (r) {
        return r.json();
      })
      .then(function (d) {
        if (!d || typeof d.unscored !== 'number') return;
        lastPollData = d;
        if (dismissed) return;
        if (!bannerAllowed()) {
          banner.style.display = 'none';
          return;
        }
        if (initialScored === null) initialScored = d.scored;
        // Caught up: some jobs scored, none left to score. Show the last-scrape
        // digest as a freshness strip; it re-renders each poll so the relative
        // time stays current.
        if (d.scored > 0 && d.unscored === 0) {
          if (!reloadedOnce && d.scored > initialScored) {
            // New scores landed this session. Reload to fill the list, then
            // show the digest after the reload.
            reloadedOnce = true;
            try {
              sessionStorage.setItem('scoringJustFinished', '1');
            } catch {
              /* ignore */
            }
            location.reload();
            return;
          }
          showComplete();
          return;
        }
        const pct = d.total > 0 ? Math.round((d.scored / d.total) * 100) : 0;
        let stateLabel;
        if (d.total === 0) {
          stateLabel =
            'Setting up your first job search — discovering companies and scraping job boards in the background. First jobs usually appear within 2–3 minutes.';
        } else if (d.scored === 0 && !d.active) {
          stateLabel = d.total + ' jobs imported. Scoring is about to start…';
        } else if (d.scored === 0) {
          stateLabel =
            'Scoring your first jobs against your resume… (ETA ' +
            formatEta(d.etaSeconds) +
            ')';
        } else if (d.newJobs24h > 0) {
          // Lead with what just landed (discovery + scrape), then progress, so the
          // user knows *why* the count jumped. Period, not a dash, so shortenStatus
          // keeps the whole line instead of truncating at the connector.
          const jobsPart =
            d.newJobs24h + ' new ' + (d.newJobs24h === 1 ? 'job' : 'jobs');
          const fromPart =
            d.newCompanies24h > 0
              ? ' from ' +
                d.newCompanies24h +
                ' new ' +
                (d.newCompanies24h === 1 ? 'company' : 'companies')
              : '';
          stateLabel =
            jobsPart + fromPart + '. ' + d.unscored + ' left to score.';
        } else {
          stateLabel =
            'Scoring ' +
            d.unscored +
            (d.unscored === 1 ? ' job' : ' jobs') +
            ' against your resume';
        }
        banner.style.display = 'flex';
        setBannerMessage(stateLabel);
        if (barEl) barEl.style.width = pct + '%';
        if (!reloadedOnce && d.scored >= 25 && initialScored < 25) {
          reloadedOnce = true;
          location.reload();
        }
      })
      .catch(function () {
        /* keep polling; transient failures are fine */
      });
  }

  // Partial navigation (nav.js) calls this with the destination filter so the
  // banner hides on non-inbox views and re-renders when returning to Jobs.
  window.syncScoringBannerVisibility = function (filter) {
    if (!bannerAllowed(filter)) {
      banner.style.display = 'none';
      return;
    }
    if (dismissed) return;
    lastMsg = ''; // force a re-render even if the message text is unchanged
    poll();
  };

  poll();
  // Poll every 25s rather than 7s. Each poll runs aggregate queries per open tab,
  // 24/7, so a tighter interval multiplied egress for no real benefit: scoring
  // progress is a slow background process and 25s is well within "feels live".
  setInterval(poll, 25000);
})();

// ---------------------------------------------------------------------------
// Scramble-to-text loader — animated indicator that churns random characters
// which resolve into a short phrase, holds, then re-scrambles into the next
// phrase and loops. Used by the scoring banner (replacing the old hourglass)
// and the onboarding wizard's pipeline-kickoff status.
//
// startScrambleLoader(el, phrases, opts) -> { stop }
//   opts.shouldPause()  optional; when true the loop idles a cycle instead of
//                       animating (e.g. while the host element is hidden).
//   opts.holdMs         pause after a phrase resolves before the next (def 4800).
//   opts.stepEvery      advance the churn every Nth animation frame; higher is
//                       slower and calmer (def 3).
// ---------------------------------------------------------------------------

// Shared phrases for both the scoring banner and the wizard kickoff status.
const SCRAMBLE_PHRASES = [
  'Scanning job boards',
  'Finding your matches',
  'Reading the market',
  'Ranking by fit',
  'Filtering the noise',
  'Matching your skills',
];

function startScrambleLoader(el, phrases, opts) {
  opts = opts || {};
  const holdMs = typeof opts.holdMs === 'number' ? opts.holdMs : 4800;
  const stepEvery = typeof opts.stepEvery === 'number' ? opts.stepEvery : 2;
  const shouldPause = opts.shouldPause;
  const reduce =
    window.matchMedia &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  let i = 0;
  let stopped = false;
  let holdTimer = null;
  let active = null; // in-flight scrambleTo handle

  // Reduced-motion: no character churn, just swap the plain phrase on a timer.
  if (reduce) {
    el.textContent = phrases[0];
    const rmTimer = setInterval(function () {
      i = (i + 1) % phrases.length;
      el.textContent = phrases[i];
    }, holdMs);
    return {
      stop: function () {
        clearInterval(rmTimer);
      },
    };
  }

  function next() {
    if (stopped) return;
    if (shouldPause && shouldPause()) {
      holdTimer = setTimeout(next, 1000);
      return;
    }
    active = scrambleTo(el, phrases[i], {
      stepEvery: stepEvery,
      onDone: function () {
        i = (i + 1) % phrases.length;
        holdTimer = setTimeout(next, holdMs);
      },
    });
  }

  next();
  return {
    stop: function () {
      stopped = true;
      if (active) active.cancel();
      clearTimeout(holdTimer);
    },
  };
}

// One-shot scramble: random characters churn and resolve into newText a single
// time, then onDone fires. Returns { cancel } to abort an in-flight resolve.
// Honors reduced motion by setting the text directly. Shared by
// startScrambleLoader (the looping label) and the scoring banner message.
function scrambleTo(el, newText, opts) {
  opts = opts || {};
  const stepEvery = typeof opts.stepEvery === 'number' ? opts.stepEvery : 2;
  const onDone = opts.onDone;
  const reduce =
    window.matchMedia &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  if (reduce) {
    el.textContent = newText;
    if (onDone) onDone();
    return { cancel: function () {} };
  }

  const chars = '!<>-_\\/[]{}=+*^?#';
  const oldText = el.textContent || '';
  const length = Math.max(oldText.length, newText.length);
  const queue = [];
  for (let n = 0; n < length; n++) {
    const start = Math.floor(Math.random() * 24);
    const end = start + Math.floor(Math.random() * 24);
    queue.push({
      from: oldText[n] || '',
      to: newText[n] || '',
      start: start,
      end: end,
      char: '',
    });
  }

  let frame = 0;
  let tick = 0;
  let cancelled = false;
  let frameRequest = null;

  function update() {
    if (cancelled) return;
    // Throttle: only advance the churn every Nth animation frame so the resolve
    // reads slow and calm rather than a frantic flicker.
    if (tick++ % stepEvery !== 0) {
      frameRequest = requestAnimationFrame(update);
      return;
    }
    let output = '';
    let complete = 0;
    for (let m = 0; m < queue.length; m++) {
      const q = queue[m];
      if (frame >= q.end) {
        complete++;
        output += q.to;
      } else if (frame >= q.start) {
        if (!q.char || Math.random() < 0.2) {
          q.char = chars[Math.floor(Math.random() * chars.length)];
        }
        output += '<span class="scramble-dim">' + q.char + '</span>';
      } else {
        output += q.from;
      }
    }
    el.innerHTML = output;
    if (complete === queue.length) {
      if (onDone) onDone();
    } else {
      frame++;
      frameRequest = requestAnimationFrame(update);
    }
  }

  update();
  return {
    cancel: function () {
      cancelled = true;
      cancelAnimationFrame(frameRequest);
    },
  };
}

(function () {
  const el = document.getElementById('scoring-progress-scramble');
  if (!el) return;
  const banner = document.getElementById('scoring-progress-banner');
  startScrambleLoader(el, SCRAMBLE_PHRASES, {
    // Idle while the banner is hidden — nothing to score, nothing to animate.
    shouldPause: function () {
      return banner && banner.style.display === 'none';
    },
  });
})();

// Set the toggle icon to match the current theme on load, and keep it in sync
// with the OS preference while the user is still on system default.
(function () {
  if (typeof syncThemeIcon === 'function') syncThemeIcon();
  if (window.matchMedia) {
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const onChange = function () {
      if (!document.documentElement.getAttribute('data-theme')) syncThemeIcon();
    };
    if (mq.addEventListener) mq.addEventListener('change', onChange);
    else if (mq.addListener) mq.addListener(onChange);
  }
})();

// ---------------------------------------------------------------------------
// Score comparison pagination (analytics page)
// ---------------------------------------------------------------------------

(function () {
  const table = document.getElementById('comparison-table');
  if (!table) return;
  const PAGE_SIZE = 25;
  let page = 0;
  const rows = table.querySelectorAll('tbody tr');
  const total = rows.length,
    pages = Math.ceil(total / PAGE_SIZE);
  function show() {
    rows.forEach(function (r, i) {
      r.style.display =
        i >= page * PAGE_SIZE && i < (page + 1) * PAGE_SIZE ? '' : 'none';
    });
    document.getElementById('comparison-prev').disabled = page === 0;
    document.getElementById('comparison-next').disabled = page >= pages - 1;
    document.getElementById('comparison-page-info').textContent =
      'Page ' + (page + 1) + ' of ' + pages + ' (' + total + ' total)';
  }
  window.pageComparison = function (d) {
    page = Math.max(0, Math.min(pages - 1, page + d));
    show();
  };
  show();
})();
