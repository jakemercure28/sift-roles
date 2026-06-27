// auth.js — hosted-mode authentication for the dashboard.
//
// Only loaded when the backend runs in hosted (Postgres + Supabase) mode; the
// page omits this script entirely on self-host, so the dashboard stays
// unauthenticated there. It runs supabase-js Google OAuth in the browser, keeps
// the current access token, attaches it as a Bearer credential to same-origin
// API requests, and shows a blocking login overlay until there is a session.
(function () {
  'use strict';

  const conf = window.__SUPABASE__;
  if (!conf || !conf.enabled || !window.supabase) {
    return;
  }

  const sb = window.supabase.createClient(conf.url, conf.anonKey, {
    auth: {
      persistSession: true,
      autoRefreshToken: true,
      detectSessionInUrl: true,
    },
  });
  window.sbClient = sb;

  let accessToken = null;

  // ready resolves once the initial session lookup completes, so the fetch
  // wrapper never fires a protected request before the token is restored (which
  // would 401 on a hard reload after login).
  let resolveReady;
  const ready = new Promise(function (r) {
    resolveReady = r;
  });

  let hydrated = false;

  function setSession(session) {
    accessToken = session && session.access_token ? session.access_token : null;
    // On sign-out reset the hydration latch so a later sign-in re-fetches the
    // tenant body. This lets dashboardSignOut skip window.location.reload (the
    // reload flashed the empty unauthenticated shell before the overlay drew).
    if (!accessToken) {
      hydrated = false;
    }
    renderGate();
    if (accessToken && !hydrated) {
      hydrated = true;
      hydrateDashboard();
      // The wizard's first-run check is tenant-scoped, so it can only run once the
      // bearer is restored. wizard.js skips its own load-time check in hosted mode
      // and waits for this call.
      if (typeof window.maybeShowOnboarding === 'function') {
        window.maybeShowOnboarding();
      }
    }
  }

  // The server renders GET / unauthenticated — browsers never attach a Bearer to
  // a top-level document load, so the initial HTML is the empty "" tenant (0
  // pending, "All locations", no jobs). nav.js only re-fetches the body on a
  // user click, so a plain load/refresh leaves that empty shell on screen. Once
  // the session is restored, re-fetch the dashboard body once with the token so
  // the real tenant's jobs and saved location filter appear without a click.
  function hydrateDashboard() {
    if (!accessToken) return;
    try {
      if (window.location.pathname !== '/') return;
    } catch {
      return;
    }
    const go = function () {
      if (typeof window.navigateDashboardUrl === 'function') {
        window.navigateDashboardUrl(window.location.href, { replace: true });
      }
    };
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', function () {
        setTimeout(go, 0);
      });
    } else {
      setTimeout(go, 0);
    }
  }

  sb.auth.onAuthStateChange(function (_event, session) {
    setSession(session);
  });

  sb.auth
    .getSession()
    .then(function (res) {
      setSession(res && res.data ? res.data.session : null);
      resolveReady();
    })
    .catch(function () {
      resolveReady();
    });

  // ---- Bearer injection -----------------------------------------------------
  // Wrap fetch so same-origin requests carry the access token. Cross-origin
  // calls (e.g. supabase-js -> Supabase) are left untouched.
  const nativeFetch = window.fetch.bind(window);
  window.fetch = function (input, init) {
    const url = typeof input === 'string' ? input : (input && input.url) || '';
    const sameOrigin =
      url.indexOf('http') !== 0 || url.indexOf(window.location.origin) === 0;
    if (!sameOrigin) {
      return nativeFetch(input, init);
    }
    return ready.then(function () {
      init = init || {};
      if (accessToken) {
        const headers = new Headers(
          init.headers || (typeof input !== 'string' && input.headers) || {}
        );
        if (!headers.has('Authorization')) {
          headers.set('Authorization', 'Bearer ' + accessToken);
        }
        init.headers = headers;
      }
      return nativeFetch(input, init).then(function (resp) {
        if (resp.status === 401) {
          accessToken = null;
          renderGate();
        }
        return resp;
      });
    });
  };

  // ---- Login overlay --------------------------------------------------------
  function signIn() {
    sb.auth.signInWithOAuth({
      provider: 'google',
      options: { redirectTo: window.location.origin },
    });
  }

  // Sign out without reloading: signOut() triggers onAuthStateChange(null),
  // which re-renders the gate and draws the login overlay over the current
  // page. Reloading instead repainted the empty unauthenticated shell first,
  // which is the flicker the user saw. Close the settings modal first so it is
  // not left mounted behind the overlay.
  window.dashboardSignOut = function () {
    if (typeof window.closeSettings === 'function') {
      window.closeSettings();
    }
    sb.auth.signOut();
  };

  let overlay = null;

  // Official multi-color Google "G", inlined so the button matches the
  // identity-guideline look without an extra network request.
  const googleLogoSVG =
    '<svg class="auth-btn-icon" width="18" height="18" viewBox="0 0 18 18" ' +
    'xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false">' +
    '<path fill="#4285F4" d="M17.64 9.205c0-.639-.057-1.252-.164-1.841H9v3.481h4.844a' +
    '4.14 4.14 0 0 1-1.796 2.716v2.259h2.908c1.702-1.567 2.684-3.875 2.684-6.615z"/>' +
    '<path fill="#34A853" d="M9 18c2.43 0 4.467-.806 5.956-2.18l-2.908-2.259c-.806.54-1.837.86-3.048.86-' +
    '2.344 0-4.328-1.584-5.036-3.711H.957v2.332A8.997 8.997 0 0 0 9 18z"/>' +
    '<path fill="#FBBC05" d="M3.964 10.71A5.41 5.41 0 0 1 3.682 9c0-.593.102-1.17.282-1.71V4.958H.957A' +
    '8.997 8.997 0 0 0 0 9c0 1.452.348 2.827.957 4.042l3.007-2.332z"/>' +
    '<path fill="#EA4335" d="M9 3.58c1.321 0 2.508.454 3.44 1.345l2.582-2.58C13.463.891 11.426 0 9 0A' +
    '8.997 8.997 0 0 0 .957 4.958L3.964 7.29C4.672 5.163 6.656 3.58 9 3.58z"/></svg>';

  function buildOverlay() {
    const el = document.createElement('div');
    el.id = 'auth-overlay';
    el.className = 'auth-overlay';
    el.innerHTML =
      '<div class="auth-card">' +
      '<h1 class="auth-title">' +
      (window.__BRAND__ || 'Sift') +
      '</h1>' +
      '<p class="auth-sub">Scored job matches, pipeline tracking, and interview ' +
      'prep, all in one place.</p>' +
      '<div class="auth-methods">' +
      '<button type="button" class="auth-btn auth-btn--google" id="auth-google">' +
      googleLogoSVG +
      '<span class="auth-btn-label">Continue with Google</span>' +
      '</button>' +
      '</div>' +
      '<p class="auth-fineprint">We only use your account to sign you in.</p>' +
      '</div>';
    const btn = el.querySelector('#auth-google');
    if (btn) btn.addEventListener('click', signIn);
    return el;
  }

  function renderGate() {
    const signedIn = !!accessToken;
    if (!signedIn) {
      if (!overlay) {
        overlay = buildOverlay();
      }
      if (document.body && !overlay.isConnected) {
        document.body.appendChild(overlay);
      }
    } else if (overlay && overlay.isConnected) {
      overlay.remove();
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', renderGate);
  } else {
    renderGate();
  }
})();
