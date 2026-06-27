'use strict';

// ---------------------------------------------------------------------------
// Onboarding wizard
// ---------------------------------------------------------------------------

// maybeShowOnboarding decides whether to open the setup wizard. Driven by the
// authenticated, tenant-scoped /api/setup/status response so it keys on the
// right tenant in hosted mode (GET / is public and always the base tenant).
function maybeShowOnboarding() {
  if (localStorage.getItem('jsa_setup_done')) return;

  fetch('/api/setup/status')
    .then(function (r) {
      return r.ok ? r.json() : null;
    })
    .then(function (data) {
      if (!data || !data.firstRun) return;
      // Don't force the wizard on a user who already has a resume (e.g. uploaded
      // via Settings). They have what they need; the wizard is only for the
      // empty-handed first run. firstRun can still be true here until discovery
      // populates companies, so resume presence is the real "stranded" gate.
      if (data.resumeContent && data.resumeContent.trim()) return;
      const overlay = document.getElementById('onboarding-wizard');
      if (overlay) overlay.classList.add('open');
    })
    .catch(function () {});
}
window.maybeShowOnboarding = maybeShowOnboarding;

// Self-host has no login gate, so run on load. In hosted mode auth.js fires it
// once the session is restored so the status call hits the real tenant.
if (!(window.__SUPABASE__ && window.__SUPABASE__.enabled)) {
  maybeShowOnboarding();
}

// Back/forward (bfcache) restores don't re-fire auth events, so the wizard's
// one-shot open in auth.js never runs again and the .open class is lost. Re-check
// on every pageshow (including persisted bfcache restores) so a not-yet-onboarded
// user is never stranded without the wizard. The status call is tenant-scoped and
// idempotent, so this is safe to call repeatedly in both hosted and self-host mode.
window.addEventListener('pageshow', function () {
  maybeShowOnboarding();
});

function wizardResumeFileChanged() {
  const input = document.getElementById('wizard-resume-file');
  const label = document.getElementById('wizard-resume-filename');
  const status = document.getElementById('wizard-resume-status');
  if (input && input.files && input.files[0]) {
    if (label) label.textContent = input.files[0].name;
    if (status) status.textContent = '';
  }
}

function wizardSaveResume() {
  const input = document.getElementById('wizard-resume-file');
  const status = document.getElementById('wizard-resume-status');
  const saveBtn = document.getElementById('wizard-resume-save-btn');
  const file = input && input.files && input.files[0];

  if (!file) {
    if (status) {
      status.textContent = 'Choose your resume to continue.';
      status.style.color = 'var(--red)';
    }
    return;
  }

  if (saveBtn) saveBtn.disabled = true;
  if (status) {
    status.textContent = 'Uploading...';
    status.style.color = 'var(--text-muted)';
  }

  function onUploaded(ok, errorMsg) {
    if (!ok) {
      if (status) {
        status.textContent = errorMsg || 'Upload failed.';
        status.style.color = 'var(--red)';
      }
      if (saveBtn) saveBtn.disabled = false;
      return;
    }
    _wizardAnalyzeAndFinish(status);
  }

  const isPdf = file.name.toLowerCase().endsWith('.pdf');

  if (isPdf) {
    const reader = new FileReader();
    reader.onload = function (e) {
      try {
        const bytes = new Uint8Array(e.target.result);
        let binary = '';
        const chunk = 0x8000;
        for (let i = 0; i < bytes.length; i += chunk) {
          binary += String.fromCharCode.apply(
            null,
            bytes.subarray(i, i + chunk)
          );
        }
        const base64 = btoa(binary);
        fetch('/api/setup/resume', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content: base64, format: 'pdf' }),
        })
          .then(function (r) {
            return r.json();
          })
          .then(function (data) {
            if (!data.ok && data.code === 'no_key') {
              onUploaded(
                false,
                'PDF extraction needs a Gemini key. Upload a .txt file instead, or add a key in Settings after setup.'
              );
            } else if (!data.ok) {
              onUploaded(
                false,
                data.error
                  ? 'Upload failed: ' + data.error
                  : 'Upload failed. Try a .txt file instead.'
              );
            } else {
              onUploaded(true);
            }
          })
          .catch(function (err) {
            onUploaded(
              false,
              'Upload failed: ' +
                (err && err.message ? err.message : 'network error')
            );
          });
      } catch (err) {
        onUploaded(false, 'Upload failed: ' + err.message);
      }
    };
    reader.onerror = function () {
      onUploaded(false, 'Could not read file.');
    };
    reader.readAsArrayBuffer(file);
  } else {
    const reader = new FileReader();
    reader.onload = function (e) {
      fetch('/api/setup/resume', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content: e.target.result }),
      })
        .then(function () {
          onUploaded(true);
        })
        .catch(function () {
          onUploaded(true);
        }); // best-effort: proceed even on hiccup
    };
    reader.readAsText(file);
  }
}

// After a successful resume upload: extract profile silently, save it, kick off
// the first scrape, then redirect to the dashboard. Every step is best-effort;
// the user can always update targets in Settings.
function _wizardAnalyzeAndFinish(status) {
  if (status) {
    status.textContent = 'Analyzing your resume...';
    status.style.color = 'var(--text-muted)';
  }
  fetch('/api/setup/extract-profile', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: '{}',
  })
    .then(function (r) {
      return r.json();
    })
    .then(function (data) {
      return fetch('/api/setup/profile', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          titles: (data.ok && data.titles) || '',
          stack: (data.ok && data.stack) || '',
          salary: (data.ok && data.salary) || '',
          location: '',
          industry: (data.ok && data.industry) || '',
          searchTerms: (data.ok && data.searchTerms) || '',
        }),
      });
    })
    .catch(function () {})
    .then(function () {
      return fetch('/api/setup/run-refresh', { method: 'POST' }).catch(
        function () {}
      );
    })
    .catch(function () {})
    .then(function () {
      localStorage.setItem('jsa_setup_done', '1');
      window.location.href = '/';
    });
}
