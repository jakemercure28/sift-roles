'use strict';

// ---------------------------------------------------------------------------
// Settings panel — always-available counterpart to the first-run wizard. Reuses
// the same /api/setup/* endpoints plus the whitelisted /api/settings/env knobs.
// ---------------------------------------------------------------------------

function openSettings() {
  const overlay = document.getElementById('settings-modal');
  if (overlay) overlay.classList.add('open');
  document.body.classList.add('modal-open');
  settingsShowTab('resume');
  settingsLoadStatus();
  settingsLoadApp();
  settingsLoadRejectionStatus();
}

function closeSettings() {
  const overlay = document.getElementById('settings-modal');
  if (overlay) overlay.classList.remove('open');
  document.body.classList.remove('modal-open');
}

// Close when the click lands on the blurred backdrop itself, not inside the modal box.
function settingsOverlayClick(e) {
  if (e.target === e.currentTarget) closeSettings();
}

// Permanently delete the signed-in account: wipes all of this tenant's jobs and
// data server-side, then signs out. Hosted-only (the button is injected by
// renderModals when auth is enabled). Two confirms because it is irreversible.
function settingsDeleteAccount() {
  if (
    !window.confirm(
      'Permanently delete your account and all of your jobs and data? This cannot be undone.'
    )
  )
    return;
  fetch('/api/account', { method: 'DELETE' })
    .then(function (r) {
      return r.json().then(function (d) {
        return { ok: r.ok, data: d };
      });
    })
    .then(function (res) {
      if (res.ok && res.data && res.data.ok) {
        if (typeof window.dashboardSignOut === 'function') {
          window.dashboardSignOut();
        } else {
          location.reload();
        }
      } else {
        showToast(
          (res.data && res.data.error) || 'Could not delete account.',
          'var(--red)'
        );
      }
    })
    .catch(function () {
      showToast('Could not delete account.', 'var(--red)');
    });
}

function settingsShowTab(name) {
  document.querySelectorAll('#settings-tabs .modal-tab').forEach(function (t) {
    t.classList.toggle('active', t.getAttribute('data-tab') === name);
  });
  document
    .querySelectorAll('#settings-modal .settings-panel')
    .forEach(function (p) {
      p.style.display =
        p.getAttribute('data-panel') === name ? 'block' : 'none';
    });
}

// Parse the markdown context.md that buildContextMd() writes, so the Profile tab
// can prefill from the live profile. Sections are delimited by "## Heading".
function settingsParseContext(md) {
  const out = { location: '', industry: '', titles: '', stack: '', salary: '' };
  const sections = {};
  let current = null;
  (md || '').split('\n').forEach(function (line) {
    const h = line.match(/^##\s+(.+?)\s*$/);
    if (h) {
      current = h[1];
      sections[current] = [];
      return;
    }
    if (current) sections[current].push(line);
  });
  function firstText(name) {
    const arr = sections[name] || [];
    for (let i = 0; i < arr.length; i++) {
      const t = arr[i].trim();
      if (t && t !== '(not specified)') return t;
    }
    return '';
  }
  function bullets(name) {
    return (sections[name] || [])
      .map(function (l) {
        const m = l.match(/^-\s+(.*)$/);
        return m ? m[1].trim() : '';
      })
      .filter(function (t) {
        return t && t !== '(not specified)';
      })
      .join('\n');
  }
  out.location = firstText("What I'm looking for");
  out.industry = firstText('Industry');
  out.titles = bullets('Target titles');
  out.stack = bullets("Stack I'm most productive in");
  const comp = (sections['Compensation'] || []).join(' ');
  const sal = comp.match(/([0-9][0-9,]*)/);
  out.salary = sal ? sal[1].replace(/,/g, '') : '';
  return out;
}

// Pull the SEARCH_TERMS array out of the companies.json profile.
function settingsParseSearchTerms(jsonText) {
  try {
    const data = JSON.parse(jsonText || '{}');
    return Array.isArray(data.SEARCH_TERMS) ? data.SEARCH_TERMS.join('\n') : '';
  } catch (_) {
    return '';
  }
}

function settingsLoadStatus() {
  fetch('/api/setup/status')
    .then(function (r) {
      return r.json();
    })
    .then(function (data) {
      if (!data) return;
      const prof = settingsParseContext(data.contextContent || '');
      const set = function (id, val) {
        const el = document.getElementById(id);
        if (el) el.value = val || '';
      };
      set('settings-titles', prof.titles);
      set('settings-stack', prof.stack);
      set('settings-salary', prof.salary);
      set('settings-location', prof.location);
      window.__settingsIndustry = prof.industry || '';
      set(
        'settings-terms',
        settingsParseSearchTerms(data.companiesContent || '')
      );
      let maxAge = '';
      try {
        const parsed = JSON.parse(data.companiesContent || '{}');
        if (parsed.MAX_AGE_DAYS != null) maxAge = String(parsed.MAX_AGE_DAYS);
      } catch (_) {
        maxAge = '';
      }
      set('settings-max-age', maxAge);
    })
    .catch(function () {});
}

function settingsResumeFileChanged() {
  const input = document.getElementById('settings-resume-file');
  const label = document.getElementById('settings-resume-filename');
  const status = document.getElementById('settings-resume-status');
  if (input && input.files && input.files[0]) {
    if (label) label.textContent = input.files[0].name;
    if (status) status.textContent = '';
  }
}

function settingsSaveResume() {
  const input = document.getElementById('settings-resume-file');
  const status = document.getElementById('settings-resume-status');
  const btn = document.getElementById('settings-resume-save-btn');
  const file = input && input.files && input.files[0];
  if (!file) {
    if (status) {
      status.textContent = 'Choose a file first.';
      status.style.color = 'var(--red)';
    }
    return;
  }
  if (btn) btn.disabled = true;
  if (status) {
    status.textContent = 'Uploading...';
    status.style.color = 'var(--text-muted)';
  }
  const isPdf = file.name.toLowerCase().endsWith('.pdf');
  const reader = new FileReader();
  if (isPdf) {
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
            if (!data.ok) {
              const msg =
                data.code === 'no_key'
                  ? 'Add a Gemini API key first, or upload a .txt file.'
                  : 'Upload failed: ' + (data.error || 'unknown');
              if (status) {
                status.textContent = msg;
                status.style.color = 'var(--red)';
              }
            } else {
              if (status) {
                status.textContent = 'Resume updated.';
                status.style.color = 'var(--green)';
              }
              showToast('Resume updated', 'var(--green)');
            }
            if (btn) btn.disabled = false;
          })
          .catch(function (err) {
            if (status) {
              status.textContent =
                'Upload failed: ' +
                (err && err.message ? err.message : 'network error');
              status.style.color = 'var(--red)';
            }
            if (btn) btn.disabled = false;
          });
      } catch (err) {
        if (status) {
          status.textContent = 'Upload failed: ' + err.message;
          status.style.color = 'var(--red)';
        }
        if (btn) btn.disabled = false;
      }
    };
    reader.readAsArrayBuffer(file);
  } else {
    reader.onload = function (e) {
      fetch('/api/setup/resume', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content: e.target.result }),
      })
        .then(function (r) {
          return r.json();
        })
        .then(function (data) {
          if (status) {
            status.textContent = data.ok ? 'Resume updated.' : 'Upload failed.';
            status.style.color = data.ok ? 'var(--green)' : 'var(--red)';
          }
          if (data.ok) showToast('Resume updated', 'var(--green)');
          if (btn) btn.disabled = false;
        })
        .catch(function () {
          if (status) {
            status.textContent = 'Upload failed.';
            status.style.color = 'var(--red)';
          }
          if (btn) btn.disabled = false;
        });
    };
    reader.readAsText(file);
  }
}

function settingsExtractProfile() {
  const status = document.getElementById('settings-profile-status');
  const btn = document.getElementById('settings-extract-btn');
  if (btn) btn.disabled = true;
  if (status) {
    status.textContent = 'Analyzing resume...';
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
      if (data.ok && (data.titles || data.stack)) {
        const set = function (id, val) {
          if (val) {
            const el = document.getElementById(id);
            if (el) el.value = val;
          }
        };
        set('settings-titles', data.titles);
        set('settings-stack', data.stack);
        set('settings-salary', data.salary);
        if (data.industry) window.__settingsIndustry = data.industry;
        if (data.searchTerms) {
          const t = document.getElementById('settings-terms');
          if (t) t.value = data.searchTerms;
        }
        if (status) {
          status.textContent =
            'Auto-filled from your resume. Review, then Save.';
          status.style.color = 'var(--green)';
        }
      } else if (status) {
        status.textContent = 'Could not auto-fill from resume.';
        status.style.color = 'var(--text-muted)';
      }
      if (btn) btn.disabled = false;
    })
    .catch(function () {
      if (status) {
        status.textContent = 'Auto-fill failed.';
        status.style.color = 'var(--red)';
      }
      if (btn) btn.disabled = false;
    });
}

function settingsSaveProfile() {
  const status = document.getElementById('settings-profile-status');
  const btn = document.getElementById('settings-profile-save-btn');
  const val = function (id) {
    return (document.getElementById(id) || {}).value || '';
  };
  if (btn) btn.disabled = true;
  fetch('/api/setup/profile', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      titles: val('settings-titles'),
      stack: val('settings-stack'),
      salary: val('settings-salary'),
      location: val('settings-location'),
      industry: window.__settingsIndustry || '',
      // Send the current search terms so saving the profile doesn't narrow the scrape net.
      searchTerms: val('settings-terms'),
    }),
  })
    .then(function (r) {
      return r.json();
    })
    .then(function (data) {
      if (status) {
        status.textContent = data.ok ? 'Profile saved.' : 'Save failed.';
        status.style.color = data.ok ? 'var(--green)' : 'var(--red)';
      }
      if (data.ok) showToast('Profile saved', 'var(--green)');
      if (btn) btn.disabled = false;
    })
    .catch(function () {
      if (status) {
        status.textContent = 'Save failed.';
        status.style.color = 'var(--red)';
      }
      if (btn) btn.disabled = false;
    });
}

function settingsSaveCompanies() {
  const status = document.getElementById('settings-companies-status');
  const btn = document.getElementById('settings-companies-save-btn');
  const terms = (document.getElementById('settings-terms') || {}).value || '';
  const maxAge =
    (document.getElementById('settings-max-age') || {}).value || '';
  if (btn) btn.disabled = true;
  fetch('/api/setup/companies', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ searchTerms: terms, maxAgeDays: maxAge }),
  })
    .then(function (r) {
      return r.json().then(function (d) {
        return { ok: r.ok, data: d };
      });
    })
    .then(function (res) {
      if (res.ok && res.data && res.data.ok) {
        if (status) {
          status.textContent = 'Targets saved.';
          status.style.color = 'var(--green)';
        }
        showToast('Targets saved', 'var(--green)');
      } else if (status) {
        status.textContent = (res.data && res.data.error) || 'Save failed.';
        status.style.color = 'var(--red)';
      }
      if (btn) btn.disabled = false;
    })
    .catch(function () {
      if (status) {
        status.textContent = 'Save failed.';
        status.style.color = 'var(--red)';
      }
      if (btn) btn.disabled = false;
    });
}

function settingsLoadApp() {
  fetch('/api/settings/env')
    .then(function (r) {
      return r.json();
    })
    .then(function (data) {
      if (!data || !data.ok) return;
      settingsRenderAppFields(data.settings || []);
    })
    .catch(function () {});
}

function settingsRenderAppFields(settings) {
  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;')
      .replace(/"/g, '&quot;')
      .replace(/</g, '&lt;');
  }
  function fieldHtml(s) {
    const id = 'settings-env-' + s.key;
    if (s.type === 'bool') {
      const hintB = s.hint
        ? '<span class="wizard-field-hint" style="display:block;margin:2px 0 8px">' +
          esc(s.hint) +
          '</span>'
        : '';
      return (
        '<label for="' +
        id +
        '" style="display:flex;align-items:center;gap:8px;cursor:pointer"><input id="' +
        id +
        '" data-key="' +
        esc(s.key) +
        '" data-type="bool" type="checkbox" style="width:auto;margin:0"' +
        (s.checked ? ' checked' : '') +
        ' /> ' +
        esc(s.label) +
        '</label>' +
        hintB
      );
    }
    if (s.type === 'secret') {
      const hintS = s.hint
        ? '<div class="wizard-field-hint" style="margin:2px 0 8px">' +
          esc(s.hint) +
          '</div>'
        : '';
      const ph = s.set ? '•••••••• (saved — leave blank to keep)' : 'not set';
      return (
        '<label for="' +
        id +
        '">' +
        esc(s.label) +
        '</label><input id="' +
        id +
        '" data-key="' +
        esc(s.key) +
        '" data-type="secret" type="password" autocomplete="off" placeholder="' +
        esc(ph) +
        '" />' +
        hintS
      );
    }
    // Show the default as the placeholder (visible when the box is empty) and
    // spell it out in the hint so the baseline is clear even when a value is set.
    const hasDef = s.def !== undefined && s.def !== null && s.def !== '';
    const hintText =
      (s.hint || '') + (hasDef ? ' Default: ' + s.def + '.' : '');
    const hint = hintText.trim()
      ? '<div class="wizard-field-hint" style="margin:2px 0 8px">' +
        esc(hintText.trim()) +
        '</div>'
      : '';
    const placeholder = hasDef ? ' placeholder="' + esc(s.def) + '"' : '';
    return (
      '<label for="' +
      id +
      '">' +
      esc(s.label) +
      '</label><input id="' +
      id +
      '" data-key="' +
      esc(s.key) +
      '" data-type="' +
      esc(s.type) +
      '" type="text" value="' +
      esc(s.value) +
      '"' +
      placeholder +
      ' />' +
      hint
    );
  }
  // Only the Integrations tab exposes editable env settings now; the scrape /
  // scoring / discovery knobs are operator-only and never reach the browser.
  const intHost = document.getElementById('settings-integrations-fields');
  if (intHost) {
    intHost.innerHTML = settings
      .filter(function (s) {
        return s.group === 'integrations';
      })
      .map(fieldHtml)
      .join('');
  }
}

function settingsSaveApp() {
  // Both the Advanced and Integrations tabs save through here; surface the
  // result on whichever status lines and buttons exist.
  const statuses = [
    document.getElementById('settings-app-status'),
    document.getElementById('settings-integrations-status'),
  ].filter(Boolean);
  const btns = [
    document.getElementById('settings-app-save-btn'),
    document.getElementById('settings-integrations-save-btn'),
  ].filter(Boolean);
  function setStatus(msg, color) {
    statuses.forEach(function (s) {
      s.textContent = msg;
      s.style.color = color;
    });
  }
  function setDisabled(v) {
    btns.forEach(function (b) {
      b.disabled = v;
    });
  }
  const payload = {};
  document
    .querySelectorAll(
      '#settings-app-fields input[data-key], #settings-integrations-fields input[data-key]'
    )
    .forEach(function (el) {
      const key = el.getAttribute('data-key');
      const type = el.getAttribute('data-type');
      if (type === 'bool') {
        payload[key] = el.checked;
        return;
      }
      const v = el.value;
      // Write-only secrets: an empty box means "leave unchanged", so skip it.
      if (type === 'secret' && !String(v).trim()) return;
      payload[key] = v;
    });
  setDisabled(true);
  fetch('/api/settings/env', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ settings: payload }),
  })
    .then(function (r) {
      return r.json().then(function (d) {
        return { ok: r.ok, data: d };
      });
    })
    .then(function (res) {
      if (res.ok && res.data && res.data.ok) {
        setStatus('Saved. Applies on the next scheduled run.', 'var(--green)');
        showToast('Settings saved', 'var(--green)');
        settingsLoadApp();
        settingsLoadRejectionStatus();
      } else {
        setStatus((res.data && res.data.error) || 'Save failed.', 'var(--red)');
      }
      setDisabled(false);
    })
    .catch(function () {
      setStatus('Save failed.', 'var(--red)');
      setDisabled(false);
    });
}

// --- Integrations tab: rejection sync status + manual sync -----------------

function settingsTimeAgo(iso) {
  const t = Date.parse(iso);
  if (isNaN(t)) return '';
  const mins = Math.floor((Date.now() - t) / 60000);
  if (mins < 1) return 'just now';
  if (mins < 60) return mins + 'm ago';
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return hrs + 'h ago';
  return Math.floor(hrs / 24) + 'd ago';
}

function settingsLoadRejectionStatus() {
  const el = document.getElementById('settings-rejection-status');
  if (!el) return;
  fetch('/api/integrations/rejection-sync')
    .then(function (r) {
      return r.json();
    })
    .then(function (d) {
      if (!d) return;
      let msg;
      let color = 'var(--text-muted)';
      if (!d.configured) {
        msg =
          'Not connected. Add your Gmail address and app password below, then Save.';
      } else if (d.paused) {
        msg = 'Sync paused. Credentials stay saved.';
      } else if (d.status && d.status.status === 'error') {
        msg =
          'Sync failed ' +
          (settingsTimeAgo(d.status.last_run_at) || 'recently') +
          ': ' +
          d.status.error;
        color = 'var(--red)';
      } else if (d.status) {
        msg =
          'Gmail connected · last sync ' +
          settingsTimeAgo(d.status.last_run_at);
        const n = d.appliedLast7d || 0;
        msg +=
          ' · ' +
          n +
          (n === 1 ? ' rejection applied' : ' rejections applied') +
          ' this week';
        color = 'var(--green)';
      } else {
        msg = 'Gmail connected. No sync has run yet; runs every 30 minutes.';
      }
      el.textContent = msg;
      el.style.color = color;
    })
    .catch(function () {
      el.textContent = 'Could not load sync status.';
    });
}

function settingsRejectionSyncNow() {
  const btn = document.getElementById('settings-sync-now-btn');
  if (btn) {
    if (btn.disabled) return;
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner"></span>Syncing…';
  }
  fetch('/api/integrations/rejection-sync/run', { method: 'POST' })
    .then(function (r) {
      return r.json().then(function (d) {
        return { status: r.status, data: d };
      });
    })
    .then(function (res) {
      const d = res.data || {};
      if (d.ok) {
        const n = d.applied || 0;
        showToast(
          'Sync complete: ' +
            n +
            (n === 1 ? ' rejection applied' : ' rejections applied'),
          'var(--green)'
        );
      } else if (d.busy) {
        showToast('A sync is already running', 'var(--text-muted)');
      } else {
        showToast(d.error || 'Sync failed', 'var(--red)');
      }
    })
    .catch(function () {
      showToast('Sync failed', 'var(--red)');
    })
    .finally(function () {
      if (btn) {
        btn.disabled = false;
        btn.textContent = 'Sync now';
      }
      settingsLoadRejectionStatus();
    });
}
