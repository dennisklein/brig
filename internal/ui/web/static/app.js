// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// brig ui's own script: the theme switch, Copy, the jump box and the live
// indicator. Pages are rendered on the server; htmx swaps their parts.
(() => {
  'use strict';

  // The theme is set before the page is drawn, so it does not flash.
  const root = document.documentElement;
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch (e) { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch (e) { /* private window */ } },
  };
  const saved = store.get('brig-ui-theme');
  if (saved === 'light' || saved === 'dark') root.dataset.theme = saved;

  function toast(msg) {
    const t = document.createElement('div');
    t.className = 'toast';
    t.textContent = msg;
    document.body.appendChild(t);
    setTimeout(() => t.remove(), 2600);
  }

  function copy(btn) {
    const pre = btn.parentElement.querySelector('pre');
    const text = btn.dataset.copy;
    const select = () => {
      const r = document.createRange();
      r.selectNodeContents(pre.querySelector('.text') || pre);
      const s = getSelection();
      s.removeAllRanges();
      s.addRange(r);
      toast('Selected. Press Ctrl+C to copy.');
    };
    try {
      navigator.clipboard.writeText(text).then(() => toast('Copied'), select);
    } catch (e) {
      select();
    }
  }

  // The jump box: / or Ctrl-K opens it, arrows move, Enter goes.
  function palette() { return document.getElementById('palette'); }
  function openPalette() {
    const p = palette();
    if (!p) return;
    p.hidden = false;
    const input = p.querySelector('input');
    input.value = '';
    input.focus();
    htmx.trigger(input, 'palette-open');
  }
  function closePalette() {
    const p = palette();
    if (p) p.hidden = true;
  }
  function movePalette(step) {
    const items = [...palette().querySelectorAll('li[data-link]')];
    if (!items.length) return;
    let i = items.findIndex(li => li.classList.contains('on'));
    items.forEach(li => li.classList.remove('on'));
    i = Math.max(0, Math.min(items.length - 1, i + step));
    items[i].classList.add('on');
    items[i].scrollIntoView({ block: 'nearest' });
  }

  // Open <details> stay open when live updates replace them.
  let openDetails = [];
  function rememberOpen(el) {
    openDetails = [...el.querySelectorAll('details[open][id]')].map(d => d.id);
  }
  function restoreOpen() {
    for (const id of openDetails) {
      const d = document.getElementById(id);
      if (d) d.open = true;
    }
  }

  // Dialogs are fetched into #layer. They close with Cancel, Escape, a click
  // beside them, and once their command has started.
  function closeDialog() {
    const layer = document.getElementById('layer');
    if (layer) layer.replaceChildren();
  }

  function setLive(on) {
    const el = document.getElementById('live');
    if (!el) return;
    el.classList.toggle('off', !on);
    el.title = on ? 'Live: changes show as they happen' : 'Not connected: reconnecting';
    el.lastElementChild.textContent = on ? 'live' : 'offline';
  }

  document.addEventListener('DOMContentLoaded', () => {
    document.addEventListener('click', ev => {
      const t = ev.target;
      const c = t.closest('[data-copy]');
      if (c) { ev.preventDefault(); copy(c); return; }
      if (t.closest('[data-theme-toggle]')) {
        const dark = root.dataset.theme ? root.dataset.theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
        root.dataset.theme = dark ? 'light' : 'dark';
        store.set('brig-ui-theme', root.dataset.theme);
        return;
      }
      if (t.closest('[data-palette-open]')) { ev.preventDefault(); openPalette(); return; }
      if (t.id === 'palette') { closePalette(); return; }
      if (t.closest('[data-dialog-close]') || t.hasAttribute('data-dialog')) { closeDialog(); return; }
      const row = t.closest('tr[data-href]');
      if (row && !t.closest('a, button')) { location.href = row.dataset.href; }
    });
    document.addEventListener('keydown', ev => {
      const p = palette();
      if (p && !p.hidden) {
        if (ev.key === 'Escape') { closePalette(); ev.preventDefault(); }
        if (ev.key === 'ArrowDown') { movePalette(1); ev.preventDefault(); }
        if (ev.key === 'ArrowUp') { movePalette(-1); ev.preventDefault(); }
        if (ev.key === 'Enter') {
          const on = p.querySelector('li.on[data-link]') || p.querySelector('li[data-link]');
          if (on) location.href = on.dataset.link;
          ev.preventDefault();
        }
        return;
      }
      if (ev.key === 'Escape' && document.getElementById('dialog')) { closeDialog(); ev.preventDefault(); return; }
      const typing = /INPUT|SELECT|TEXTAREA/.test(document.activeElement?.tagName);
      if (!typing && (ev.key === '/' || (ev.key === 'k' && (ev.ctrlKey || ev.metaKey)))) {
        ev.preventDefault();
        openPalette();
      }
    });
    // A dialog that is refused answers 422 with the reason, which htmx
    // would drop as an error.
    document.body.addEventListener('htmx:beforeSwap', ev => {
      if (ev.detail.xhr.status === 422) {
        ev.detail.shouldSwap = true;
        ev.detail.isError = false;
      }
    });
    document.body.addEventListener('htmx:afterRequest', ev => {
      const elt = ev.detail.elt;
      if (ev.detail.successful && elt && elt.matches && elt.matches('#dialog form')) closeDialog();
    });
    document.body.addEventListener('htmx:afterSwap', ev => {
      const target = ev.detail.target || ev.target;
      if (target.id === 'layer') {
        const first = target.querySelector('input[type=text], select');
        if (first) first.focus();
      }
      if (target.closest && target.closest('#palette')) {
        const first = palette().querySelector('li[data-link]');
        if (first) first.classList.add('on');
      }
    });
    document.body.addEventListener('htmx:sseBeforeMessage', ev => rememberOpen(ev.target));
    document.body.addEventListener('htmx:sseMessage', restoreOpen);
    document.body.addEventListener('htmx:sseOpen', () => setLive(true));
    document.body.addEventListener('htmx:sseError', () => setLive(false));
  });
})();
