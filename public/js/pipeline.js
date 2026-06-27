'use strict';

// Pipeline colors come from the central theme (window.__THEME__, injected by
// dashboard-html.js from lib/theme.js). Fallback mirrors the theme defaults in
// case the injection is missing.
const PIPELINE_COLORS = (window.__THEME__ && window.__THEME__.pipeline) || {
  '': '#6B7280',
  applied: '#7C7C7C',
  phone_screen: '#A2A2A2',
  interview: '#6E6E6E',
  onsite: '#9A9A9A',
  offer: '#1F9D6B',
  closed: '#8A8A8A',
  rejected: '#DC4B43',
  ghosted: '#6B7280',
};
const PIPELINE_LABELS = {
  '': '\u2014',
  applied: 'Applied',
  phone_screen: 'Phone Screen',
  interview: 'Interview',
  onsite: 'Onsite',
  offer: 'Offer',
  closed: 'Closed',
  rejected: 'Rejected',
};

const UNDOABLE_PIPELINE = ['rejected', 'ghosted', 'closed'];

// revertPipelineSelect rolls a <select> back to its prior stage after a failed
// write, so the dropdown never shows a value the server didn't actually store.
function revertPipelineSelect(selectEl, prevValue) {
  if (!selectEl) return;
  selectEl.value = prevValue;
  selectEl.style.color = PIPELINE_COLORS[prevValue] || 'var(--slate)';
  selectEl.dataset.prev = prevValue;
}

// errorMessage pulls the human message out of the standard error envelope
// ({ ok:false, error }) returned by the backend, falling back when the body is
// missing or not JSON.
async function errorMessage(res, fallback) {
  try {
    const d = await res.json();
    return (d && d.error) || fallback;
  } catch (_e) {
    return fallback;
  }
}

async function setPipeline(id, value, selectEl) {
  const prevValue = selectEl ? selectEl.dataset.prev || '' : '';
  try {
    const res = await fetch('/pipeline', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, value }),
    });
    if (!res.ok) {
      revertPipelineSelect(selectEl, prevValue);
      showToast(
        await errorMessage(res, 'Could not update status'),
        'var(--red)'
      );
      return;
    }

    if (selectEl) {
      selectEl.style.color = PIPELINE_COLORS[value] || 'var(--slate)';
      selectEl.dataset.prev = value;
    }
    const label = PIPELINE_LABELS[value] || value;
    if (UNDOABLE_PIPELINE.includes(value)) {
      registerUndo(id, prevValue);
      showToast(label, PIPELINE_COLORS[value] || 'var(--slate)', performUndo);
    } else {
      showToast(label || 'Cleared', PIPELINE_COLORS[value] || 'var(--slate)');
    }

    const notesBtn = document.getElementById('notes-btn-' + id);
    if (notesBtn && !notesBtn.textContent.includes('View')) {
      notesBtn.style.display = ['phone_screen', 'interview'].includes(value)
        ? ''
        : 'none';
    }

    if (value === 'rejected') {
      const row = selectEl && selectEl.closest('.job-card');
      const arch = await fetch('/archive', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id }),
      });
      if (!arch.ok) {
        // Status saved, but the archive sweep failed; leave the row visible
        // rather than hiding a card that's still in the list.
        showToast(await errorMessage(arch, 'Could not archive'), 'var(--red)');
        return;
      }
      if (row) {
        row.style.transition = 'opacity 0.3s';
        row.style.opacity = '0';
        setTimeout(() => row.remove(), 300);
      }
    }
  } catch (_e) {
    revertPipelineSelect(selectEl, prevValue);
    showToast('Could not reach the server', 'var(--red)');
  }
}

async function archiveJob(id, btn) {
  const row = btn.closest('.job-card');
  const sel = row && row.querySelector('.pipeline-select');
  const prevValue = sel ? sel.dataset.prev || sel.value || '' : '';
  try {
    const res = await fetch('/archive', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id }),
    });
    if (!res.ok) {
      showToast(await errorMessage(res, 'Could not archive'), 'var(--red)');
      return;
    }
    row.style.transition = 'opacity 0.3s';
    row.style.opacity = '0';
    setTimeout(() => row.remove(), 300);
    registerUndo(id, prevValue);
    showToast('Archived', 'var(--slate)', performUndo);
  } catch (_e) {
    showToast('Could not reach the server', 'var(--red)');
  }
}

let _applyFiltersTimer = null;

function applyFilters() {
  const searchBox = document.querySelector('.search-box');
  if (!searchBox) return;

  const q = searchBox.value.trim();

  window.clearTimeout(_applyFiltersTimer);
  _applyFiltersTimer = window.setTimeout(() => {
    const params = new URLSearchParams(window.location.search);
    if (q) params.set('q', q);
    else params.delete('q');

    params.delete('page');

    const nextUrl = `/?${params.toString()}`;
    const currentUrl = `${window.location.pathname}${window.location.search}`;
    if (nextUrl !== currentUrl) navigateDashboardUrl(nextUrl);
  }, 600);
}

// ---------------------------------------------------------------------------
// Company notes modal
// ---------------------------------------------------------------------------

let _currentCompany = null;

function openCompanyNotes(company) {
  _currentCompany = company;
  document.getElementById('company-notes-sub').textContent = company;
  document.getElementById('company-tags-input').value = '';
  document.getElementById('company-notes-input').value = '';
  document.getElementById('company-notes-modal').classList.add('open');
  fetch('/company-notes?company=' + encodeURIComponent(company))
    .then((r) => r.json())
    .then((data) => {
      document.getElementById('company-tags-input').value = data.tags || '';
      document.getElementById('company-notes-input').value = data.notes || '';
    })
    .catch(() => {});
}

function closeCompanyNotes() {
  document.getElementById('company-notes-modal').classList.remove('open');
  _currentCompany = null;
}

function saveCompanyNotes() {
  if (!_currentCompany) return;
  const tags = document.getElementById('company-tags-input').value;
  const notes = document.getElementById('company-notes-input').value;
  fetch('/company-notes', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ company: _currentCompany, tags, notes }),
  })
    .then(() => {
      showToast('Notes saved', 'var(--green)');
      closeCompanyNotes();
    })
    .catch(() => showToast('Save failed', 'var(--red)'));
}

document
  .getElementById('company-notes-modal')
  .addEventListener('click', (e) => {
    if (e.target === document.getElementById('company-notes-modal'))
      closeCompanyNotes();
  });

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    closeCompanyNotes();
    document.getElementById('jd-modal').style.display = 'none';
  }
});

// ---------------------------------------------------------------------------
// Job description modal
// ---------------------------------------------------------------------------

async function openJobDescription(id, title, company) {
  const modal = document.getElementById('jd-modal');
  const body = document.getElementById('jd-modal-body');
  document.getElementById('jd-modal-title').textContent = title;
  document.getElementById('jd-modal-sub').textContent = company;
  body.textContent = 'Loading…';
  modal.style.display = 'flex';
  try {
    const data = await fetch(
      '/job-description?id=' + encodeURIComponent(id)
    ).then((r) => r.json());
    body.textContent = data.description || '(no description stored)';
  } catch {
    body.textContent = 'Failed to load.';
  }
}

document.getElementById('jd-modal').addEventListener('click', (e) => {
  if (e.target === document.getElementById('jd-modal'))
    document.getElementById('jd-modal').style.display = 'none';
});
