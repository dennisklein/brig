// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// brig ui's help, in layers that open only when asked: a hint card when the
// pointer rests on a dotted word or an action's button, the help pane that ?
// opens, and help in the jump box. Hints can be switched off; ? keeps
// working. The texts come from the server, as HTML fragments.
(() => {
  'use strict';

  const local = {
    get(k) { try { return localStorage.getItem(k); } catch (e) { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch (e) { /* private window */ } },
  };
  const session = {
    get(k) { try { return JSON.parse(sessionStorage.getItem(k) || 'null'); } catch (e) { return null; } },
    set(k, v) { try { sessionStorage.setItem(k, JSON.stringify(v)); } catch (e) { /* private window */ } },
  };

  // Words you have looked at once lose their underline, so pages get
  // quieter as you learn them.
  const H = {
    hints: local.get('brig-ui-hints') !== 'off',
    seen: new Set(JSON.parse(local.get('brig-ui-help-seen') || '[]')),
    cache: new Map(),
    hint: null, timer: 0, over: null,
    // stack holds the pane's entries; PAGE stands for this page's help.
    stack: [],
  };
  const PAGE = 'page';
  const hintDelay = { concept: 300, action: 700 };

  const $ = id => document.getElementById(id);
  const pane = () => $('helppane');
  const typing = () => /INPUT|SELECT|TEXTAREA/.test(document.activeElement?.tagName);

  function pageURL() {
    const d = document.body.dataset;
    return '/help?' + new URLSearchParams({ page: d.helpPage || '', tab: d.helpTab || '' });
  }

  function see(id) {
    if (H.seen.has(id)) return;
    H.seen.add(id);
    local.set('brig-ui-help-seen', JSON.stringify([...H.seen]));
    mark();
  }

  // mark underlines the first mention of each concept on the page that you
  // have not looked at yet.
  function mark() {
    document.body.classList.toggle('hints-off', !H.hints);
    const first = new Set();
    for (const el of document.querySelectorAll('.main .term, #layer .term')) {
      const id = el.dataset.term;
      el.classList.toggle('seen', H.seen.has(id));
      el.classList.toggle('dup', first.has(id));
      first.add(id);
    }
    for (const el of document.querySelectorAll('#help-body [data-chip]')) {
      el.classList.toggle('seen', H.seen.has(el.dataset.chip));
    }
    for (const b of document.querySelectorAll('[data-hints]')) {
      b.classList.toggle('on', (b.dataset.hints === 'on') === H.hints);
      b.setAttribute('aria-pressed', String((b.dataset.hints === 'on') === H.hints));
    }
  }

  async function fetchHTML(url) {
    const r = await fetch(url, { headers: { Accept: 'text/html' } });
    if (!r.ok) throw new Error(r.status + ' ' + url);
    return r.text();
  }

  // Hint cards.

  function hintKey(el) {
    if (!el) return null;
    if (el.dataset.term) return 'concept/' + el.dataset.term;
    if (el.dataset.help) return 'action/' + el.dataset.help;
    return null;
  }
  const hintTarget = t => t?.closest?.('[data-term], [data-help]');

  async function showHint(el) {
    const key = hintKey(el);
    if (!key) return;
    let html = H.cache.get(key);
    if (html === undefined) {
      try { html = await fetchHTML('/help/hint/' + key); } catch (e) { html = null; }
      H.cache.set(key, html);
    }
    if (!html || !el.isConnected || (H.over !== el && document.activeElement !== el)) return;
    hideHint();
    const card = document.createElement('div');
    card.className = 'hintcard';
    card.id = 'help-hint';
    card.setAttribute('role', 'tooltip');
    card.innerHTML = html;
    document.body.appendChild(card);
    el.setAttribute('aria-describedby', 'help-hint');
    const r = el.getBoundingClientRect();
    const w = card.offsetWidth, h = card.offsetHeight;
    let y = r.bottom + 6;
    if (y + h > innerHeight - 8) y = Math.max(8, r.top - h - 6);
    card.style.left = Math.max(8, Math.min(r.left, innerWidth - w - 8)) + 'px';
    card.style.top = y + 'px';
    card.addEventListener('mouseenter', () => clearTimeout(H.timer));
    card.addEventListener('mouseleave', () => { H.timer = setTimeout(hideHint, 150); });
    H.hint = { card, anchor: el, key };
    if (key.startsWith('concept/')) see(el.dataset.term);
  }

  function hideHint() {
    if (!H.hint) return;
    H.hint.anchor.removeAttribute('aria-describedby');
    H.hint.card.remove();
    H.hint = null;
  }

  // The pane.

  // open shows entry in the pane: after the current one with push, else
  // after this page's help, so that Back leads there.
  function open(entry, push) {
    if (push && H.stack.length) H.stack.push(entry); else H.stack = entry === PAGE ? [PAGE] : [PAGE, entry];
    show();
  }

  function show() {
    const p = pane();
    if (!p) return;
    p.hidden = false;
    document.body.classList.add('help-open');
    syncButton();
    load(H.stack[H.stack.length - 1]);
  }

  function close() {
    const p = pane();
    if (!p || p.hidden) return;
    p.hidden = true;
    document.body.classList.remove('help-open');
    syncButton();
    save();
  }

  function syncButton() {
    const on = !pane()?.hidden;
    for (const b of document.querySelectorAll('[data-help-toggle]')) {
      b.classList.toggle('on', on);
      b.setAttribute('aria-expanded', String(on));
    }
  }

  function save() {
    session.set('brig-ui-help-pane', { open: !pane()?.hidden, stack: H.stack });
  }

  async function load(entry) {
    const body = $('help-body');
    const q = $('help-q');
    if (q) q.value = '';
    try {
      body.innerHTML = await fetchHTML(entry === PAGE ? pageURL() : entry);
    } catch (e) {
      body.innerHTML = '<p class="muted">This help is not there. Reload the page if brig ui was upgraded.</p>';
    }
    const m = entry.match(/^\/help\/concept\/(.+)$/);
    if (m) see(m[1]);
    afterLoad();
    body.scrollTop = 0;
    save();
  }

  function afterLoad() {
    const crumb = $('help-body').querySelector('[data-crumb]');
    $('help-crumb').textContent = crumb ? 'Help · ' + crumb.dataset.crumb : 'Help';
    const back = pane().querySelector('[data-help-back]');
    back.hidden = H.stack.length < 2;
    mark();
  }

  let searchTimer = 0;
  function search(q) {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(async () => {
      if (!q.trim()) { load(H.stack[H.stack.length - 1]); return; }
      try {
        $('help-body').innerHTML = await fetchHTML('/help/search?' + new URLSearchParams({ q }));
        afterLoad();
      } catch (e) { /* keep what shows */ }
    }, 120);
  }

  // help explains what the pointer or the focus is on, else what the open
  // dialog does, else the page; a second ? closes the page's help.
  function help() {
    if (H.hint) { const k = H.hint.key; hideHint(); open('/help/' + k); return; }
    const t = hintTarget(document.activeElement) || H.over;
    const key = hintKey(t);
    if (key && !t.closest('#helppane')) { open('/help/' + key); return; }
    const dlg = document.querySelector('#dialog [data-help-open]');
    if (dlg) { open(dlg.dataset.helpOpen); return; }
    if (!pane().hidden && H.stack.length === 1) { close(); return; }
    open(PAGE);
  }

  // Show me: a task's link names the action whose button it points at.
  function showMe() {
    const m = location.hash.match(/^#show-([\w-]+)$/);
    if (!m) return;
    history.replaceState(null, '', location.pathname + location.search);
    const el = document.querySelector('.main [data-help="' + m[1] + '"]');
    if (!el) return;
    el.scrollIntoView({ block: 'center' });
    el.classList.add('showme');
    setTimeout(() => el.classList.remove('showme'), 3500);
  }

  // Events. They are added before app.js's, so that ? and Escape reach the
  // help first, and help links in the jump box open the pane.

  document.addEventListener('mouseover', ev => {
    const el = hintTarget(ev.target);
    H.over = el;
    if (!H.hints || !el || el.closest('#helppane, .hintcard')) return;
    if (H.hint?.anchor === el) { clearTimeout(H.timer); return; }
    clearTimeout(H.timer);
    H.timer = setTimeout(() => { if (H.over === el) showHint(el); }, el.dataset.term ? hintDelay.concept : hintDelay.action);
  });
  document.addEventListener('mouseout', ev => {
    const el = hintTarget(ev.target);
    if (!el || el.contains(ev.relatedTarget)) return;
    if (H.over === el) H.over = null;
    clearTimeout(H.timer);
    if (H.hint?.anchor === el) H.timer = setTimeout(hideHint, 150);
  });
  document.addEventListener('focusin', ev => {
    const el = ev.target.closest?.('.term');
    if (H.hints && el && !el.closest('#helppane')) showHint(el);
    else if (H.hint && H.hint.anchor !== ev.target) hideHint();
  });

  document.addEventListener('click', ev => {
    const t = ev.target;
    if (H.hint && !t.closest('.hintcard')) hideHint();
    if (t.closest('[data-help-toggle]')) { if (pane().hidden) open(PAGE); else close(); return; }
    if (t.closest('[data-help-close]')) { close(); return; }
    if (t.closest('[data-help-back]')) { H.stack.pop(); load(H.stack[H.stack.length - 1]); return; }
    const opener = t.closest('[data-help-open]');
    if (opener) { ev.preventDefault(); open(opener.dataset.helpOpen); return; }
    const hints = t.closest('[data-hints]');
    if (hints) {
      H.hints = hints.dataset.hints === 'on';
      local.set('brig-ui-hints', H.hints ? 'on' : 'off');
      mark();
      return;
    }
    const link = t.closest('#helppane a[data-help-link], #palette a[href^="/help/"]');
    if (link) {
      ev.preventDefault();
      ev.stopPropagation();
      const fromPalette = link.closest('#palette');
      if (fromPalette) $('palette').hidden = true;
      open(link.getAttribute('href'), !fromPalette && !pane().hidden);
      return;
    }
    const show = t.closest('#helppane a[data-help-show]');
    if (show) {
      const u = new URL(show.href);
      if (u.pathname + u.search === location.pathname + location.search) {
        ev.preventDefault();
        location.hash = u.hash;
        showMe();
      }
    }
  }, true);

  // Capture, so that Enter on a help entry of the jump box opens the pane
  // before app.js follows the link.
  window.addEventListener('keydown', ev => {
    const p = $('palette');
    if (p && !p.hidden && ev.key === 'Enter') {
      const on = p.querySelector('li.on[data-link]') || p.querySelector('li[data-link]');
      if (on && on.dataset.link.startsWith('/help/')) {
        ev.preventDefault();
        ev.stopImmediatePropagation();
        p.hidden = true;
        open(on.dataset.link);
      }
      return;
    }
    if (ev.key === 'Escape') {
      if (H.hint) { hideHint(); ev.preventDefault(); ev.stopImmediatePropagation(); return; }
      if (!document.getElementById('dialog') && pane() && !pane().hidden && (!p || p.hidden)) { close(); ev.preventDefault(); }
      return;
    }
    if (ev.key === '?' && !typing() && !ev.ctrlKey && !ev.metaKey && !ev.altKey) {
      ev.preventDefault();
      help();
    }
  }, true);

  document.addEventListener('input', ev => { if (ev.target.id === 'help-q') search(ev.target.value); });

  document.addEventListener('DOMContentLoaded', () => {
    mark();
    const st = session.get('brig-ui-help-pane');
    if (st && Array.isArray(st.stack) && st.stack.length) {
      H.stack = st.stack.filter(e => e === PAGE || /^\/help[/?]/.test(e));
      if (st.open && H.stack.length) show();
    }
    showMe();
    const changed = () => {
      mark();
      if (H.hint && !H.hint.anchor.isConnected) hideHint();
    };
    document.body.addEventListener('htmx:afterSwap', changed);
    document.body.addEventListener('htmx:sseMessage', changed);
  });
})();
