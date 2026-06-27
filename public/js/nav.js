'use strict';

const ICON_EYE = `<svg xmlns="http://www.w3.org/2000/svg" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/></svg>`;

const _currentJobId = null;
let _dashboardNavController = null;

const DASHBOARD_PARTIAL_FILTERS = new Set([
  'all',
  'not-applied',
  'applied',
  'interviewing',
  'offers',
  'rejected',
  'closed',
  'archived',
  'ghosted',
  'analytics',
  'activity-log',
  'market-research',
]);

function getDashboardListUrl(urlLike) {
  let url;
  try {
    url = new URL(urlLike, window.location.href);
  } catch {
    return null;
  }
  if (url.origin !== window.location.origin || url.pathname !== '/')
    return null;
  const filter = url.searchParams.get('filter') || 'not-applied';
  return DASHBOARD_PARTIAL_FILTERS.has(filter) ? url : null;
}

function dashboardFragmentUrl(targetUrl) {
  const fragmentUrl = new URL('/api/dashboard-list', window.location.origin);
  fragmentUrl.search = targetUrl.search;
  return fragmentUrl;
}

function runInsertedScripts(root) {
  root.querySelectorAll('script').forEach((oldScript) => {
    const script = document.createElement('script');
    for (const attr of oldScript.attributes) {
      script.setAttribute(attr.name, attr.value);
    }
    script.textContent = oldScript.textContent;
    oldScript.replaceWith(script);
  });
}

// --- Sliding pill (seg-thumb) for the primary nav tabs ----------------------
// A single green pill sits behind the tabs and is moved under whichever tab is
// active. It's a persistent node so animating its transform/width slides it.
function ensureSegThumb(nav) {
  if (!nav) return null;
  let thumb = nav.querySelector('.seg-thumb');
  if (!thumb) {
    thumb = document.createElement('span');
    thumb.className = 'seg-thumb';
    nav.insertBefore(thumb, nav.firstChild);
  }
  return thumb;
}

function positionSegThumb(nav) {
  if (!nav) return;
  const thumb = nav.querySelector('.seg-thumb');
  if (!thumb) return;
  const active = nav.querySelector('.seg-opt.active');
  if (!active) {
    thumb.style.opacity = '0';
    return;
  }
  thumb.style.opacity = '1';
  thumb.style.width = active.offsetWidth + 'px';
  thumb.style.height = active.offsetHeight + 'px';
  thumb.style.transform =
    'translate(' + active.offsetLeft + 'px, ' + active.offsetTop + 'px)';
}

// Position the pill without a slide-in (first load, resize): place it, force a
// reflow, then re-enable transitions.
function placeSegThumbInstant(nav) {
  if (!nav) return;
  nav.classList.add('seg-no-anim');
  positionSegThumb(nav);
  void nav.offsetWidth;
  nav.classList.remove('seg-no-anim');
}

function initSegThumb() {
  const nav = document.getElementById('dashboard-primary-nav');
  if (!nav) return;
  ensureSegThumb(nav);
  placeSegThumbInstant(nav);
}

// The sidebar is rendered once and not part of the fragment swap, so after a
// partial navigation we reconcile which nav item reads as active from the
// destination filter (matched against each item's data-filter).
function syncSidebarActive(filter) {
  const items = document.querySelectorAll('.sidebar .nav-item[data-filter]');
  items.forEach((item) => {
    item.classList.toggle(
      'active',
      item.getAttribute('data-filter') === filter
    );
  });
}

// The sidebar links are rendered once with the search query (q/minScore) baked
// in and are not part of the fragment swap, so after the user changes or clears
// the search we must rewrite each link's carried params. Otherwise clicking a
// sidebar item re-applies a stale, already-cleared search (each link keeps its
// own filter/sort; we only sync the cross-view search params and drop page).
function syncSidebarSearch(url) {
  const carry = ['q', 'minScore'];
  document
    .querySelectorAll('.sidebar .nav-item[href][data-filter]')
    .forEach((item) => {
      let href;
      try {
        href = new URL(item.getAttribute('href'), window.location.href);
      } catch {
        return;
      }
      carry.forEach((key) => {
        const val = url.searchParams.get(key);
        if (val) href.searchParams.set(key, val);
        else href.searchParams.delete(key);
      });
      href.searchParams.delete('page');
      item.setAttribute('href', href.pathname + href.search);
    });
}

async function navigateDashboardUrl(urlLike, opts) {
  opts = opts || {};
  const targetUrl = getDashboardListUrl(urlLike);
  if (!targetUrl) {
    if (!opts.fromPop)
      window.location.assign(new URL(urlLike, window.location.href).toString());
    return false;
  }

  if (_dashboardNavController) _dashboardNavController.abort();
  const controller = new AbortController();
  _dashboardNavController = controller;

  try {
    document.body.classList.add('dashboard-loading');
    const res = await fetch(dashboardFragmentUrl(targetUrl).toString(), {
      headers: { Accept: 'application/json' },
      signal: controller.signal,
    });
    if (!res.ok) throw new Error('fragment request failed: ' + res.status);
    const data = await res.json();
    if (
      !data ||
      typeof data.filtersHtml !== 'string' ||
      typeof data.mainHtml !== 'string'
    ) {
      throw new Error('invalid dashboard fragment response');
    }

    // The status-tab bar was removed (the sidebar is the sole workflow nav), so
    // #dashboard-primary-nav no longer exists. Its swap is optional and only runs
    // if both the element and the fragment field are present.
    const nav = document.getElementById('dashboard-primary-nav');
    const filters = document.getElementById('dashboard-filters');
    const main = document.getElementById('dashboard-main');
    if (!filters || !main) throw new Error('dashboard targets missing');

    if (nav && typeof data.primaryNavHtml === 'string') {
      // Keep the sliding pill as one persistent node across the innerHTML swap so
      // it animates to the newly-active tab instead of snapping (see seg-thumb).
      const thumb = nav.querySelector('.seg-thumb');
      nav.innerHTML = data.primaryNavHtml;
      if (thumb) nav.appendChild(thumb);
      else ensureSegThumb(nav);
      positionSegThumb(nav);
    }
    filters.outerHTML = data.filtersHtml;
    main.innerHTML = data.mainHtml;
    runInsertedScripts(main);

    // The page title lives in its own swap target; the sidebar is persistent so
    // its active item is reconciled client-side from the destination filter.
    const titleEl = document.getElementById('dashboard-title');
    if (titleEl && typeof data.titleHtml === 'string')
      titleEl.innerHTML = data.titleHtml;
    syncSidebarActive(targetUrl.searchParams.get('filter') || 'not-applied');
    syncSidebarSearch(targetUrl);

    // The search box lives outside the swapped region, so keep it in sync on
    // programmatic navigation (back/forward) without clobbering active typing.
    // Market Research and Analytics are reports, not searchable lists, so the box
    // is hidden there and restored when navigating back to a list view.
    const destFilter = targetUrl.searchParams.get('filter') || 'not-applied';
    const searchBox = document.querySelector('.search-box');
    if (searchBox) {
      searchBox.hidden =
        destFilter === 'market-research' || destFilter === 'analytics';
      if (document.activeElement !== searchBox) {
        const qv =
          new URL(
            data.url || targetUrl.toString(),
            window.location.href
          ).searchParams.get('q') || '';
        if (searchBox.value !== qv) searchBox.value = qv;
      }
    }

    const nextUrl = new URL(
      data.url || targetUrl.toString(),
      window.location.href
    );
    if (opts.replace || opts.fromPop) {
      window.history.replaceState(null, '', nextUrl.toString());
    } else {
      window.history.pushState(null, '', nextUrl.toString());
      window.scrollTo(0, 0);
    }
    // The scoring banner lives outside the swapped region; reconcile its
    // visibility with the destination view (inbox views only).
    if (window.syncScoringBannerVisibility) {
      window.syncScoringBannerVisibility(
        nextUrl.searchParams.get('filter') || 'not-applied'
      );
    }
    return true;
  } catch (err) {
    if (err && err.name === 'AbortError') return false;
    console.warn(
      '[dashboard] partial navigation failed, falling back to full load:',
      err
    );
    if (!opts.fromPop) window.location.assign(targetUrl.toString());
    return false;
  } finally {
    if (_dashboardNavController === controller) {
      _dashboardNavController = null;
    }
    document.body.classList.remove('dashboard-loading');
  }
}

document.addEventListener('click', (e) => {
  const link = e.target.closest && e.target.closest('a[href]');
  if (!link) return;
  if (
    link.target ||
    link.download ||
    e.metaKey ||
    e.ctrlKey ||
    e.shiftKey ||
    e.altKey
  )
    return;
  if (!getDashboardListUrl(link.href)) return;
  e.preventDefault();
  navigateDashboardUrl(link.href);
});

window.addEventListener('popstate', () => {
  navigateDashboardUrl(window.location.href, { fromPop: true });
});

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', initSegThumb);
} else {
  initSegThumb();
}

window.addEventListener('resize', () => {
  placeSegThumbInstant(document.getElementById('dashboard-primary-nav'));
});
