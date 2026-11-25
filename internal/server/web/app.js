'use strict';
const $ = (s) => document.querySelector(s);
const $$ = (s) => document.querySelectorAll(s);

const state = {
  photos: new Map(),   // id -> photo
  tiles: new Map(),    // id -> element
  selected: new Set(),
  selecting: false,
  status: null,
  tab: 'photos',
};

async function api(path, opts = {}) {
  const res = await fetch(path, opts);
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch {}
    throw new Error(msg);
  }
  const ct = res.headers.get('content-type') || '';
  return ct.includes('json') ? res.json() : res.text();
}

const fmtBytes = (n) => {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
};
const fmtDur = (s) => {
  const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
};
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

/* ---------- tabs ---------- */
$$('.tab').forEach((b) => b.addEventListener('click', () => {
  state.tab = b.dataset.tab;
  $$('.tab').forEach((x) => x.classList.toggle('active', x === b));
  $$('.panel').forEach((p) => p.classList.toggle('active', p.id === 'tab-' + state.tab));
  if (state.tab === 'frame') enterFrameTab();
  if (state.tab === 'logs') resetLogs();
}));

/* ---------- photos grid ---------- */
function tileFor(p) {
  let el = state.tiles.get(p.id);
  if (!el) {
    el = document.createElement('div');
    el.className = 'tile';
    el.dataset.id = p.id;
    el.innerHTML = '<div class="ph"></div><img loading="lazy" alt="" hidden><span class="badge hidden"></span><span class="tick"></span>';
    el.addEventListener('click', () => onTileClick(p.id));
    state.tiles.set(p.id, el);
  }
  const img = el.querySelector('img'), ph = el.querySelector('.ph'), badge = el.querySelector('.badge');
  if (p.status === 'ready' && p.rev) {
    const src = `/api/photos/${p.id}/thumb?v=${p.rev}`;
    if (img.getAttribute('src') !== src) img.src = src;
    img.hidden = false; ph.textContent = '';
  } else {
    img.hidden = true;
    ph.textContent = p.status === 'error' ? p.name : 'Processing…';
  }
  badge.className = 'badge';
  if (p.status === 'error') { badge.textContent = 'Error'; badge.classList.add('error'); }
  else if (p.status === 'processing') { badge.textContent = 'Processing'; }
  else if (!p.synced) { badge.textContent = 'Sending to frame'; }
  else badge.classList.add('hidden');
  el.title = p.status === 'error' ? `${p.name}\n${p.error}\nClick to retry` :
    `${p.name}\n${p.width}×${p.height} · ${fmtBytes(p.size)}\nUploaded ${new Date(p.uploadedAt).toLocaleString()}`;
  el.classList.toggle('selected', state.selected.has(p.id));
  applyFilter(el, p);
  return el;
}

function applyFilter(el, p) {
  const f = $('#filter').value;
  const show = f === 'all' || (f === 'error' && p.status === 'error') || (f === 'pending' && !p.synced && p.status !== 'error');
  el.classList.toggle('hidden', !show);
}

function renderAll(list) {
  state.photos = new Map(list.map((p) => [p.id, p]));
  for (const [id, el] of state.tiles) if (!state.photos.has(id)) { el.remove(); state.tiles.delete(id); }
  const frag = document.createDocumentFragment();
  for (const p of list) frag.appendChild(tileFor(p)); // list is newest first
  $('#grid').replaceChildren(frag);
  updateCounts();
}

function upsertPhoto(p) {
  const isNew = !state.photos.has(p.id);
  state.photos.set(p.id, p);
  const el = tileFor(p);
  if (isNew) $('#grid').prepend(el);
  updateCounts();
}

function removePhoto(id) {
  state.photos.delete(id);
  state.selected.delete(id);
  state.tiles.get(id)?.remove();
  state.tiles.delete(id);
  updateCounts();
  updateSelectionUI();
}

let countsQueued = false;
function updateCounts() {
  if (countsQueued) return;
  countsQueued = true;
  requestAnimationFrame(() => {
    countsQueued = false;
    let total = 0, ready = 0, synced = 0, proc = 0, err = 0;
    for (const p of state.photos.values()) {
      total++;
      if (p.status === 'ready') ready++;
      if (p.status === 'processing') proc++;
      if (p.status === 'error') err++;
      if (p.synced) synced++;
    }
    const parts = [`${total} photos`, `${synced} on frame`];
    if (proc) parts.push(`${proc} processing`);
    if (err) parts.push(`${err} failed`);
    $('#photo-count').textContent = parts.join(' · ');
    $('#summary').textContent = parts.join(' · ');
    $('#empty').classList.toggle('hidden', total > 0);
  });
}

$('#filter').addEventListener('change', () => {
  for (const [id, el] of state.tiles) applyFilter(el, state.photos.get(id));
});

function onTileClick(id) {
  const p = state.photos.get(id);
  if (state.selecting) {
    state.selected.has(id) ? state.selected.delete(id) : state.selected.add(id);
    tileFor(p);
    updateSelectionUI();
  } else if (p.status === 'error') {
    api(`/api/photos/${id}/retry`, { method: 'POST' }).catch((e) => alert(e.message));
  } else {
    window.open(`/api/photos/${id}/original`, '_blank');
  }
}

function updateSelectionUI() {
  $('#grid').classList.toggle('selecting', state.selecting);
  $('#select-toggle').textContent = state.selecting ? 'Done' : 'Select';
  $('#select-all').classList.toggle('hidden', !state.selecting);
  const del = $('#delete-selected');
  del.classList.toggle('hidden', !state.selecting);
  del.disabled = state.selected.size === 0;
  del.textContent = state.selected.size ? `Delete ${state.selected.size}` : 'Delete';
}

$('#select-toggle').addEventListener('click', () => {
  state.selecting = !state.selecting;
  if (!state.selecting) { const ids = [...state.selected]; state.selected.clear(); ids.forEach((id) => state.photos.has(id) && tileFor(state.photos.get(id))); }
  updateSelectionUI();
});
$('#select-all').addEventListener('click', () => {
  for (const [id, el] of state.tiles) if (!el.classList.contains('hidden')) { state.selected.add(id); el.classList.add('selected'); }
  updateSelectionUI();
});
$('#delete-selected').addEventListener('click', async () => {
  const ids = [...state.selected];
  if (!ids.length || !confirm(`Delete ${ids.length} photo(s) from the server and the frame?`)) return;
  try {
    await api('/api/photos/delete', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ids }) });
    ids.forEach(removePhoto);
  } catch (e) { alert('Delete failed: ' + e.message); }
});

/* ---------- uploads ---------- */
const UPLOAD_CONCURRENCY = 4;
const up = { queue: [], active: new Set(), total: 0, done: 0, dups: 0, failed: 0, fromZip: 0, unpacking: 0, bytesTotal: 0, bytesDone: 0, inflight: new Map() };
const isZipFile = (f) => /\.zip$/i.test(f.name) || /zip/.test(f.type);

function addFiles(files) {
  const list = [...files].filter((f) => f.size > 0);
  if (!list.length) return;
  if (up.active.size === 0 && up.queue.length === 0) {
    Object.assign(up, { total: 0, done: 0, dups: 0, failed: 0, fromZip: 0, unpacking: 0, bytesTotal: 0, bytesDone: 0 });
    $('#upload-errors').replaceChildren();
  }
  up.queue.push(...list);
  up.total += list.length;
  up.bytesTotal += list.reduce((n, f) => n + f.size, 0);
  $('#upload-panel').classList.remove('hidden');
  pumpUploads();
  renderUpload();
}

function pumpUploads() {
  while (up.active.size < UPLOAD_CONCURRENCY && up.queue.length) {
    const file = up.queue.shift();
    const xhr = new XMLHttpRequest();
    up.active.add(xhr);
    up.inflight.set(xhr, 0);
    const zip = isZipFile(file);
    xhr.upload.onprogress = (e) => { up.inflight.set(xhr, e.loaded); renderUpload(); };
    // The server unpacks a zip after receiving it; say so instead of sitting at 100%.
    let unpacking = false;
    if (zip) xhr.upload.onload = () => { unpacking = true; up.unpacking++; renderUpload(); };
    xhr.onloadend = () => {
      up.active.delete(xhr);
      up.inflight.delete(xhr);
      if (unpacking) up.unpacking--;
      up.bytesDone += file.size;
      up.done++;
      let results = null;
      try { results = JSON.parse(xhr.responseText); } catch {}
      if (xhr.status === 0) uploadError(file.name, 'cancelled or connection lost');
      else if (!Array.isArray(results) || !results.length) uploadError(file.name, results?.error || `HTTP ${xhr.status}`);
      else {
        for (const res of results) {
          // Photos from a zip are labelled "archive.zip › photo.jpg"; a zip-level error is just "archive.zip".
          const label = res.zip && res.name !== res.zip ? `${res.zip} › ${res.name}` : res.name || file.name;
          if (res.error) { uploadError(label, res.error); continue; }
          if (res.zip) up.fromZip++;
          if (res.duplicate) up.dups++;
          if (res.photo) upsertPhoto(res.photo);
        }
      }
      renderUpload();
      pumpUploads();
    };
    const fd = new FormData();
    fd.append('file', file, file.name);
    xhr.open('POST', '/api/upload');
    xhr.send(fd);
  }
}

function uploadError(name, msg) {
  up.failed++;
  const li = document.createElement('li');
  li.textContent = `${name}: ${msg}`;
  $('#upload-errors').appendChild(li);
}

function renderUpload() {
  const inflight = [...up.inflight.values()].reduce((a, b) => a + b, 0);
  const pct = up.bytesTotal ? Math.min(100, (up.bytesDone + inflight) / up.bytesTotal * 100) : 0;
  $('#upload-bar').style.width = pct.toFixed(1) + '%';
  const busy = up.active.size + up.queue.length > 0;
  let t = `${busy ? 'Uploading' : 'Uploaded'} ${up.done} of ${up.total}`;
  if (up.unpacking) t += ` · unpacking ${up.unpacking} zip${up.unpacking === 1 ? '' : 's'}…`;
  if (up.fromZip) t += ` · ${up.fromZip} photo${up.fromZip === 1 ? '' : 's'} from zips`;
  if (up.dups) t += ` · ${up.dups} already uploaded`;
  if (up.failed) t += ` · ${up.failed} failed`;
  t += ` · ${fmtBytes(up.bytesDone + inflight)} / ${fmtBytes(up.bytesTotal)}`;
  $('#upload-text').textContent = t;
  $('#upload-cancel').textContent = busy ? 'Cancel' : 'Dismiss';
}

$('#upload-cancel').addEventListener('click', () => {
  if (up.active.size + up.queue.length > 0) {
    up.done += up.queue.length; up.queue = [];
    for (const x of up.active) x.abort();
  } else $('#upload-panel').classList.add('hidden');
});

const drop = $('#drop');
$('#file-input').addEventListener('change', (e) => { addFiles(e.target.files); e.target.value = ''; });
['dragenter', 'dragover'].forEach((ev) => document.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add('over'); }));
['dragleave', 'drop'].forEach((ev) => document.addEventListener(ev, (e) => { e.preventDefault(); if (ev === 'drop' || e.target === drop) drop.classList.remove('over'); }));
document.addEventListener('drop', (e) => { if (e.dataTransfer?.files?.length) addFiles(e.dataTransfer.files); });
window.addEventListener('beforeunload', (e) => { if (up.active.size + up.queue.length) e.preventDefault(); });

/* ---------- status / frame ---------- */
function renderStatus(st) {
  if (st) state.status = st;
  const f = state.status?.frame;
  if (!f) return;
  const pill = $('#frame-pill');
  pill.className = 'pill ' + (!f.online ? 'offline' : f.syncing ? 'syncing' : 'online');
  pill.textContent = !f.online ? 'Frame offline' : f.syncing ? `Syncing · ${f.pendingPush} left` : 'Frame online';
  pill.title = f.online ? f.url : `${f.url}\n${f.lastError || ''}`;
  if (state.tab === 'frame') renderInfo();
}

function renderInfo() {
  const st = state.status, f = st?.frame, i = f?.info;
  const rows = [['Server version', st?.version], ['Frame URL', f?.url], ['Connection', f?.online ? 'online' : `offline: ${f?.lastError || ''}`]];
  if (i) {
    rows.push(
      ['Frame version', i.version], ['Hostname', i.hostname], ['Addresses', (i.addresses || []).join(', ')],
      ['Display', i.display], ['Photos on frame', i.photoCount], ['Slideshow', i.paused ? 'paused' : 'playing'],
      ['CPU temperature', i.cpuTempC ? i.cpuTempC.toFixed(1) + ' °C' : 'n/a'],
      ['Disk free', i.diskTotalBytes ? `${fmtBytes(i.diskFreeBytes)} of ${fmtBytes(i.diskTotalBytes)}` : 'n/a'],
      ['Memory available', i.memAvailBytes ? fmtBytes(i.memAvailBytes) : 'n/a'], ['Frame uptime', fmtDur(i.uptimeSeconds)],
    );
    $('#pause-btn').textContent = i.paused ? 'Resume' : 'Pause';
    $('#pause-btn').dataset.ctl = i.paused ? 'resume' : 'pause';
  }
  if (f?.lastSync) rows.push(['Last sync', new Date(f.lastSync).toLocaleTimeString()]);
  rows.push(['Render size', `${st.display.width}×${st.display.height}`], ['Processing queue', st.queue]);
  $('#info').innerHTML = rows.map(([k, v]) => `<tr><td>${esc(k)}</td><td>${esc(v ?? '')}</td></tr>`).join('');
}

let screenTimer = null;
function refreshScreen() {
  if (state.tab !== 'frame' || document.hidden) return;
  const img = new Image();
  img.onload = () => { $('#screen').src = img.src; };
  img.src = '/api/frame/screen?t=' + Date.now();
}
async function enterFrameTab() {
  renderInfo();
  refreshScreen();
  if (!bg.saved) loadBackground();
  clearInterval(screenTimer);
  screenTimer = setInterval(() => { if (state.tab === 'frame') refreshScreen(); else clearInterval(screenTimer); }, 4000);
  try {
    const s = await api('/api/frame/settings');
    const form = $('#settings');
    for (const [k, v] of Object.entries(s)) {
      const el = form.elements[k];
      if (!el) continue;
      if (el.type === 'checkbox') el.checked = v; else el.value = v;
    }
    $('#settings-msg').textContent = '';
  } catch (e) { $('#settings-msg').textContent = 'Frame unreachable: ' + e.message; }
}

$('#settings').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.target.elements;
  const body = {
    intervalSeconds: +f.intervalSeconds.value, shuffle: f.shuffle.checked, showNewFirst: f.showNewFirst.checked,
    transition: f.transition.value, transitionMs: +f.transitionMs.value,
  };
  try {
    await api('/api/frame/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    $('#settings-msg').textContent = 'Saved';
  } catch (err) { $('#settings-msg').textContent = 'Failed: ' + err.message; }
});

/* ---------- photo background ---------- */
const bg = { saved: null, timer: null, url: null, seq: 0 };
const bgForm = $('#background');
const DEFAULT_BG = { mode: 'blur', color: '#000000', brightness: 55, blur: 5 };

function bgValues() {
  const f = bgForm.elements;
  return { mode: f.mode.value || 'blur', color: f.color.value, brightness: +f.brightness.value, blur: +f.blur.value };
}
function bgFill(b) {
  const f = bgForm.elements;
  f.mode.value = b.mode; f.color.value = b.color; f.brightness.value = b.brightness; f.blur.value = b.blur;
  bgChanged();
}
function bgChanged() {
  const b = bgValues(), f = bgForm.elements;
  f.brightnessOut.value = b.brightness + '%';
  f.blurOut.value = b.blur;
  bgForm.querySelectorAll('[data-for]').forEach((el) => el.classList.toggle('hidden', el.dataset.for !== b.mode));
  bgForm.querySelectorAll('[data-color]').forEach((el) => el.classList.toggle('on', el.dataset.color === b.color));
  const dirty = bg.saved && JSON.stringify(b) !== JSON.stringify(bg.saved);
  $('#bg-msg').textContent = dirty ? 'Not saved' : '';
  clearTimeout(bg.timer);
  bg.timer = setTimeout(bgPreview, 120); // sliders fire constantly; render once they settle
}
async function bgPreview() {
  const seq = ++bg.seq;
  const qs = new URLSearchParams(Object.entries(bgValues()).map(([k, v]) => [k, String(v)]));
  try {
    const res = await fetch('/api/background/preview?' + qs);
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
    const blob = await res.blob();
    if (seq !== bg.seq) return; // a newer preview was requested meanwhile
    if (bg.url) URL.revokeObjectURL(bg.url);
    bg.url = URL.createObjectURL(blob);
    $('#bg-preview').src = bg.url;
    $('#bg-preview').classList.remove('hidden');
    $('#bg-preview-msg').classList.add('hidden');
  } catch (e) {
    if (seq !== bg.seq) return;
    $('#bg-preview').classList.add('hidden');
    const msg = $('#bg-preview-msg');
    msg.textContent = e.message === 'no photos yet' ? 'Upload a photo to see a preview.' : 'Preview unavailable: ' + e.message;
    msg.classList.remove('hidden');
  }
}
async function loadBackground() {
  try {
    bg.saved = await api('/api/background');
    bgFill(bg.saved);
  } catch (e) { $('#bg-msg').textContent = 'Failed to load: ' + e.message; }
}
bgForm.addEventListener('input', bgChanged);
bgForm.querySelectorAll('[data-color]').forEach((b) => {
  b.style.background = b.dataset.color;
  b.addEventListener('click', () => { bgForm.elements.color.value = b.dataset.color; bgChanged(); });
});
$('#bg-reset').addEventListener('click', () => bgFill(DEFAULT_BG));
bgForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    const res = await api('/api/background', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(bgValues()) });
    bg.saved = res.background;
    bgFill(bg.saved);
    const n = res.rerendering;
    $('#bg-msg').textContent = n ? `Saved. Re-rendering ${n} photo${n === 1 ? '' : 's'}…` : 'Saved';
  } catch (err) { $('#bg-msg').textContent = 'Failed: ' + err.message; }
});

$$('[data-ctl]').forEach((b) => b.addEventListener('click', async () => {
  try {
    await api('/api/frame/control/' + b.dataset.ctl, { method: 'POST' });
    setTimeout(refreshScreen, 1200);
  } catch (e) { alert(e.message); }
}));
$('#sync-now').addEventListener('click', () => api('/api/sync', { method: 'POST' }));

/* ---------- logs ---------- */
const logs = { since: 0, timer: null, gen: 0 };
const MAX_LOG_LINES = 4000;

function resetLogs() {
  logs.since = 0; logs.gen++;
  $('#logs').replaceChildren();
  $('#log-error').textContent = '';
  clearTimeout(logs.timer);
  pollLogs(logs.gen);
}

async function pollLogs(gen) {
  if (gen !== logs.gen || state.tab !== 'logs') return;
  const src = $('#log-source').value, level = $('#log-level').value;
  try {
    if (src === 'journal') {
      const text = await api('/api/frame/journal?lines=1000');
      if (gen !== logs.gen) return;
      const box = $('#logs');
      const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 30;
      box.textContent = text;
      applyLogSearch();
      if (atBottom) box.scrollTop = box.scrollHeight;
    } else {
      const url = (src === 'server' ? '/api/logs' : '/api/frame/logs') + `?since=${logs.since}&level=${level}&limit=1000`;
      const res = await api(url);
      if (gen !== logs.gen) return;
      if (res.next < logs.since) { resetLogs(); return; } // process restarted
      logs.since = res.next;
      appendLogs(res.entries);
    }
    $('#log-error').textContent = '';
  } catch (e) {
    if (gen === logs.gen) $('#log-error').textContent = e.message;
  }
  if ($('#log-follow').checked) logs.timer = setTimeout(() => pollLogs(gen), src === 'journal' ? 5000 : 2000);
}

function appendLogs(entries) {
  if (!entries.length) return;
  const box = $('#logs');
  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 30;
  const q = $('#log-search').value.toLowerCase();
  const frag = document.createDocumentFragment();
  for (const e of entries) {
    const div = document.createElement('div');
    div.className = 'l ' + e.level;
    const attrs = e.attrs ? Object.entries(e.attrs).map(([k, v]) => `${k}=${typeof v === 'string' && v.includes(' ') ? JSON.stringify(v) : v}`).join(' ') : '';
    const t = new Date(e.time);
    div.innerHTML = `<span class="t">${t.toLocaleDateString()} ${t.toLocaleTimeString()}</span> ${e.level.padEnd(5)} ${esc(e.msg)} <span class="a">${esc(attrs)}</span>`;
    if (q && !div.textContent.toLowerCase().includes(q)) div.hidden = true;
    frag.appendChild(div);
  }
  box.appendChild(frag);
  while (box.childElementCount > MAX_LOG_LINES) box.firstElementChild.remove();
  if (atBottom) box.scrollTop = box.scrollHeight;
}

function applyLogSearch() {
  const q = $('#log-search').value.toLowerCase();
  for (const d of $('#logs').children) d.hidden = q && !d.textContent.toLowerCase().includes(q);
}

$('#log-source').addEventListener('change', resetLogs);
$('#log-level').addEventListener('change', resetLogs);
$('#log-search').addEventListener('input', applyLogSearch);
$('#log-follow').addEventListener('change', () => { clearTimeout(logs.timer); pollLogs(logs.gen); });
$('#log-clear').addEventListener('click', () => $('#logs').replaceChildren());

/* ---------- live updates ---------- */
async function reload() {
  try {
    const [list, st] = await Promise.all([api('/api/photos'), api('/api/status')]);
    renderAll(list);
    renderStatus(st);
  } catch (e) { console.error(e); }
}

function connectEvents() {
  const es = new EventSource('/api/events');
  es.onopen = reload; // also re-syncs state after a reconnect
  es.onmessage = (m) => {
    const ev = JSON.parse(m.data);
    if (ev.type === 'photo') upsertPhoto(ev.data);
    else if (ev.type === 'deleted') removePhoto(ev.data.id);
    else if (ev.type === 'frame' && state.status) { state.status.frame = ev.data; renderStatus(); }
  };
}

setInterval(() => api('/api/status').then(renderStatus).catch(() => {}), 10000);
connectEvents();
