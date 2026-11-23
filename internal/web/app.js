// The web interface of tcpraid. Plain JavaScript, no framework, nothing loaded from
// anywhere but this node. It is a client of the node's API: everything it shows comes
// from /api/v1/..., everything it does goes there.
'use strict';

// Where the documentation is, once it has an address; empty: the page says where to look.
var DOCS_URL = 'https://github.com/Claw-King/tcpraid';

var API = '/api/v1/';
var dict = { en: {}, zh: {} };
var lang = 'en';
var state = {
  page: 'overview',
  loggedIn: false,
  status: null,
  sessions: [],
  rates: {},      // session id -> {tx, rx} in bytes a second
  prev: {},       // session id -> {tx, rx, at}
  peers: [],
  codes: [],
  rules: [],
  peer: '',       // name of the peer whose detail is shown
  detailKey: '',  // what the detail was drawn from: it is drawn again only when this changes
  checks: {}      // peer name -> result of the last check
};
var busy = false;

// ---------- small things ----------

function $(sel, root) { return (root || document).querySelector(sel); }

// h makes an element: h('td', {class: 'x', onclick: f}, 'text', child, ...).
function h(tag, props) {
  var el = document.createElement(tag);
  props = props || {};
  Object.keys(props).forEach(function (k) {
    var v = props[k];
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else if (k.slice(0, 2) === 'on') el.addEventListener(k.slice(2), v);
    else if (v === true) el.setAttribute(k, '');
    else if (v !== false && v !== null && v !== undefined) el.setAttribute(k, v);
  });
  for (var i = 2; i < arguments.length; i++) add(el, arguments[i]);
  return el;
}

function add(el, kid) {
  if (kid === null || kid === undefined || kid === false) return;
  if (Array.isArray(kid)) { kid.forEach(function (k) { add(el, k); }); return; }
  el.append(kid);
}

function t(key) {
  var s = (dict[lang] && dict[lang][key]) || dict.en[key] || key;
  var args = arguments;
  return s.replace(/\{(\d+)\}/g, function (m, i) {
    var v = args[Number(i) + 1];
    return v === undefined ? m : String(v);
  });
}

function applyI18n() {
  document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  document.querySelectorAll('[data-i18n]').forEach(function (el) { el.textContent = t(el.dataset.i18n); });
}

function fmtBytes(n) {
  var units = ['B', 'KB', 'MB', 'GB', 'TB'];
  var i = 0;
  n = Number(n) || 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? String(Math.round(n)) : n.toFixed(n >= 100 ? 0 : 1)) + ' ' + units[i];
}

function fmtRate(n) { return fmtBytes(n) + '/s'; }

function fmtDuration(sec) {
  sec = Math.max(0, Math.floor(sec));
  var d = Math.floor(sec / 86400), hh = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  if (d > 0) return d + 'd ' + hh + 'h';
  if (hh > 0) return hh + 'h ' + m + 'm';
  if (m > 0) return m + 'm ' + s + 's';
  return s + 's';
}

function fmtTime(iso) {
  var d = new Date(iso);
  return isNaN(d) ? '' : d.toLocaleString();
}

var toastTimer = 0;
function toast(msg, isErr) {
  var el = $('#toast');
  el.textContent = msg;
  el.className = isErr ? 'err' : '';
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(function () { el.hidden = true; }, isErr ? 7000 : 3500);
}

// copyText puts text on the clipboard. Without a secure context (plain HTTP on a local
// network) the clipboard interface is not there, so a selected text area is the way.
function copyText(text, within) {
  function done(ok) { toast(ok ? t('msg.copied') : t('msg.copy_failed'), !ok); }
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(false); });
    return;
  }
  var ta = h('textarea', { class: 'offscreen', readonly: true });
  ta.value = text;
  (within || document.body).append(ta);
  ta.select();
  var ok = false;
  try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
  ta.remove();
  done(ok);
}

// ---------- the API ----------

// api sends one request and returns the decoded answer (null for an empty one). An
// error has the message the API gave. A 401 means the login is gone: the login form
// comes back.
function api(method, path, body) {
  var opt = { method: method, headers: { 'X-Tcpraid-Request': '1' }, credentials: 'same-origin', cache: 'no-store' };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  return fetch(API + path, opt).then(function (r) {
    return r.text().then(function (text) {
      var data = null;
      try { data = text ? JSON.parse(text) : null; } catch (e) { data = null; }
      if (r.status === 401 && path !== 'login') {
        showLogin();
        throw new Error(t('msg.login_needed'));
      }
      if (!r.ok) throw new Error((data && data.error) || (r.status + ' ' + r.statusText));
      return data;
    });
  });
}

function seg(s) { return encodeURIComponent(s); }

// ---------- dialogs ----------

var dlgSubmit = null;

// openDialog shows the one dialog of the page. o.submit, if given, runs when the form
// is sent; it returns a promise; a rejection is shown in the dialog, which stays open;
// a result of true keeps it open too.
function openDialog(o) {
  $('#dlg-title').textContent = o.title;
  var body = $('#dlg-body');
  body.replaceChildren();
  add(body, o.body);
  $('#dlg-error').textContent = '';
  var ok = $('#dlg-ok');
  ok.textContent = o.ok || t('act.ok');
  ok.className = o.danger ? 'danger' : 'primary';
  ok.disabled = false;
  $('#dlg-cancel').hidden = !!o.noCancel;
  dlgSubmit = o.submit || null;
  var dlg = $('#dlg');
  if (!dlg.open) dlg.showModal();
}

function field(label, input, hint) {
  return h('label', {}, h('span', { text: label }), input, hint ? h('span', { class: 'hint', text: hint }) : null);
}

function checkbox(label, checked) {
  var box = h('input', { type: 'checkbox', checked: checked });
  return { box: box, el: h('label', { class: 'check' }, box, h('span', { text: label })) };
}

function select(options, value) {
  var el = h('select', {});
  options.forEach(function (o) {
    var opt = h('option', { value: o[0], text: o[1] });
    el.append(opt);
  });
  el.value = value;
  return el;
}

function confirmDialog(title, text, okLabel, action) {
  openDialog({ title: title, body: h('p', { text: text }), ok: okLabel, danger: true, submit: action });
}

// ---------- tables ----------

function table(el, heads, rows, empty) {
  var thead = h('thead', {}, h('tr', {}, heads.map(function (x) {
    return typeof x === 'string' ? h('th', { text: x }) : h('th', { class: 'num-cell', text: x.num });
  })));
  var tbody = h('tbody', {});
  if (rows.length === 0) tbody.append(h('tr', { class: 'empty' }, h('td', { colspan: String(heads.length), text: empty })));
  rows.forEach(function (r) { tbody.append(r); });
  el.replaceChildren(thead, tbody);
}

function num(text) { return h('td', { class: 'num-cell', text: text }); }

// ---------- overview and sessions ----------

function sessionRate(s) { return state.rates[s.id] || { tx: 0, rx: 0 }; }

function ruleOf(s) { return s.rule || (s.role === 'server' ? t('sessions.incoming') : '-'); }

function speedText(r) { return t('sessions.speed_value', fmtRate(r.tx), fmtRate(r.rx)); }

function renderOverview() {
  var st = state.status;
  if (!st) return;
  $('#ov-name').textContent = st.node.name;
  $('#ov-fingerprint').textContent = st.node.fingerprint;
  $('#ov-version').textContent = st.version;
  $('#ov-uptime').textContent = fmtDuration(st.uptime_seconds);
  $('#ov-peers').textContent = st.peers;
  $('#ov-rules').textContent = st.rules;
  $('#ov-sessions').textContent = state.sessions.length;
  var tx = 0, rx = 0;
  state.sessions.forEach(function (s) { var r = sessionRate(s); tx += r.tx; rx += r.rx; });
  $('#ov-tx').textContent = fmtRate(tx);
  $('#ov-rx').textContent = fmtRate(rx);
  var rows = state.sessions.slice(0, 10).map(function (s) {
    return h('tr', {},
      h('td', { text: ruleOf(s) }),
      h('td', { text: s.peer || '-' }),
      h('td', { text: s.destination }),
      num(speedText(sessionRate(s))));
  });
  table($('#ov-table'), [t('col.rule'), t('col.peer'), t('col.destination'), { num: t('col.speed') }], rows, t('sessions.none'));
  $('#ov-more').hidden = state.sessions.length <= 10;
}

function renderSessions() {
  var now = Date.now();
  var rows = state.sessions.map(function (s) {
    var resent = s.tx_bytes > 0 ? (100 * (s.resent_bytes || 0) / s.tx_bytes).toFixed(1) + '%' : '-';
    return h('tr', {},
      h('td', { text: ruleOf(s) }),
      h('td', { text: s.peer || '-' }),
      h('td', { text: s.destination }),
      num(fmtDuration((now - new Date(s.started).getTime()) / 1000)),
      num(fmtBytes(s.tx_bytes)),
      num(fmtBytes(s.rx_bytes)),
      num(speedText(sessionRate(s))),
      num(String(s.subs)),
      num(resent));
  });
  table($('#sessions-table'), [t('col.rule'), t('col.peer'), t('col.destination'), { num: t('col.running') },
    { num: t('col.sent') }, { num: t('col.received') }, { num: t('col.speed') }, { num: t('col.subs') }, { num: t('col.resent') }],
    rows, t('sessions.none'));
}

// ---------- rules ----------

function ruleStatus(r) {
  if (!r.enabled) return h('span', { class: 'muted', text: t('rules.off') });
  if (r.listening) {
    return h('span', {}, h('span', { class: 'ok', text: t('rules.listening') }),
      r.error ? h('div', { class: 'warn', text: t('rules.last_problem', r.error) }) : null);
  }
  return h('span', { class: 'bad', text: t('rules.not_listening', r.error || '') });
}

function ruleSpec(r, enabled) {
  var spec = { name: r.name, peer: r.peer, enabled: enabled };
  if (r.match) spec.match = r.match;
  else { spec.listen = r.listen; spec.destination = r.destination; }
  return spec;
}

function ruleSpeed(name) {
  var tx = 0, rx = 0;
  state.sessions.forEach(function (s) {
    if (s.rule === name) { var r = sessionRate(s); tx += r.tx; rx += r.rx; }
  });
  return { tx: tx, rx: rx };
}

function ruleRow(r, transparent) {
  var toggle = h('input', {
    type: 'checkbox', checked: r.enabled, 'aria-label': t('col.enabled'),
    onchange: function () {
      api('PUT', 'rules/' + seg(r.name), ruleSpec(r, !r.enabled)).then(function () {
        toast(r.enabled ? t('msg.rule_off', r.name) : t('msg.rule_on', r.name));
        return refresh();
      }).catch(function (e) { toast(e.message, true); refresh(); });
    }
  });
  var actions = h('td', { class: 'actions' },
    h('button', { type: 'button', class: 'small', text: t('act.edit'), onclick: function () { ruleForm(r, transparent); } }),
    h('button', {
      type: 'button', class: 'small danger', text: t('act.delete'), onclick: function () {
        confirmDialog(t('rules.delete_title', r.name), t('rules.delete_text'), t('act.delete'), function () {
          return api('DELETE', 'rules/' + seg(r.name)).then(function () { toast(t('msg.rule_deleted', r.name)); return refresh(); });
        });
      }
    }));
  var cells = [h('td', { text: r.name })];
  if (transparent) cells.push(h('td', { class: 'wrap', text: r.match }), h('td', { text: r.peer }));
  else cells.push(h('td', { text: r.listen }), h('td', { text: r.peer }), h('td', { text: r.destination }));
  cells.push(h('td', {}, toggle), h('td', {}, ruleStatus(r)), num(String(r.sessions)), num(speedText(ruleSpeed(r.name))), actions);
  return h('tr', {}, cells);
}

function renderRules() {
  var fixed = state.rules.filter(function (r) { return !r.match; });
  var transparent = state.rules.filter(function (r) { return !!r.match; });
  table($('#rules-table'), [t('col.name'), t('col.listen'), t('col.peer'), t('col.destination'), t('col.enabled'), t('col.status'),
    { num: t('col.sessions') }, { num: t('col.speed') }, ''], fixed.map(function (r) { return ruleRow(r, false); }), t('rules.none'));
  table($('#trules-table'), [t('col.name'), t('col.match'), t('col.peer'), t('col.enabled'), t('col.status'),
    { num: t('col.sessions') }, { num: t('col.speed') }, ''], transparent.map(function (r) { return ruleRow(r, true); }), t('rules.none_transparent'));
}

// ruleForm makes or edits a rule. old is the rule to edit, or null.
function ruleForm(old, transparent) {
  api('GET', 'peers').then(function (peers) {
    var names = peers.map(function (p) { return [p.name, p.name]; });
    if (old && !names.some(function (n) { return n[0] === old.peer; })) names.push([old.peer, old.peer]);
    var name = h('input', { value: old ? old.name : '', required: true, maxlength: '64' });
    var peer = select(names, old ? old.peer : (names[0] ? names[0][0] : ''));
    var listen = h('input', { value: old ? old.listen || '' : '', placeholder: '0.0.0.0:5000', required: !transparent });
    var dest = h('input', { value: old ? old.destination || '' : '', placeholder: '192.0.2.10:5000', required: !transparent });
    var match = h('input', { value: old ? old.match || '' : '', placeholder: '192.0.2.0/24:873', required: transparent });
    var enabled = checkbox(t('rules.enabled_label'), old ? old.enabled : true);
    var body = [field(t('col.name'), name)];
    if (transparent) body.push(field(t('col.match'), match, t('rules.match_hint')));
    else body.push(field(t('rules.listen_label'), listen, t('rules.listen_hint')));
    body.push(field(t('col.peer'), peer, names.length ? '' : t('rules.no_peers')));
    if (!transparent) body.push(field(t('rules.destination_label'), dest, t('rules.destination_hint')));
    body.push(enabled.el);
    openDialog({
      title: old ? t('rules.edit_title', old.name) : (transparent ? t('rules.new_transparent') : t('rules.new')),
      body: body,
      ok: t('act.save'),
      submit: function () {
        var spec = { name: name.value.trim(), peer: peer.value, enabled: enabled.box.checked };
        if (transparent) spec.match = match.value.trim();
        else { spec.listen = listen.value.trim(); spec.destination = dest.value.trim(); }
        var req = old ? api('PUT', 'rules/' + seg(old.name), spec) : api('POST', 'rules', spec);
        return req.then(function () { toast(t('msg.rule_saved', spec.name)); return refresh(); });
      }
    });
  }).catch(function (e) { toast(e.message, true); });
}

// ---------- peers ----------

function peerStatus(p) {
  if (p.online === true) return h('span', { class: 'ok', text: t('peers.online') });
  if (p.online === false) return h('span', { class: 'bad', text: t('peers.offline') });
  return h('span', { class: 'muted', text: t('peers.unknown') });
}

function renderPeers() {
  var rows = state.peers.map(function (p) {
    return h('tr', {},
      h('td', { text: p.name }),
      h('td', {}, peerStatus(p)),
      h('td', { text: p.version || '-' }),
      num(String(p.paths.length)),
      h('td', { class: 'actions' }, h('button', {
        type: 'button', class: 'small', text: t('act.details'),
        onclick: function () { state.peer = p.name; state.detailKey = ''; renderPeerDetail(); $('#peer-detail').scrollIntoView({ block: 'nearest' }); }
      })));
  });
  table($('#peers-table'), [t('col.name'), t('col.status'), t('col.version'), { num: t('col.paths') }, ''], rows, t('peers.none'));

  var codes = state.codes.filter(function (c) { return !c.used; });
  $('#codes-card').hidden = codes.length === 0;
  table($('#codes-table'), [t('codes.id'), t('codes.expires'), ''], codes.map(function (c) {
    var left = (new Date(c.expires).getTime() - Date.now()) / 1000;
    return h('tr', {},
      h('td', {}, h('code', { text: c.id })),
      h('td', { text: t('codes.left', fmtDuration(left)) }),
      h('td', { class: 'actions' }, h('button', {
        type: 'button', class: 'small danger', text: t('codes.revoke'), onclick: function () {
          api('DELETE', 'pairing-codes/' + seg(c.id)).then(function () { toast(t('msg.code_revoked')); return refresh(); })
            .catch(function (e) { toast(e.message, true); });
        }
      })));
  }), '');
  renderPeerDetail();
}

function pathSummary(p) {
  var sec = p.security;
  if (p.security === 'owncert') sec += ' (' + (p.verify || 'pin') + ')';
  return sec;
}

function apiPaths(paths) {
  return paths.map(function (p) {
    return { name: p.name, type: p.type, addr: p.addr, proxy: p.proxy || '', security: p.security, verify: p.verify || '', pin: p.pin || '', roles: p.roles, subs: p.subs };
  });
}

function savePaths(peer, paths, done) {
  return api('PUT', 'peers/' + seg(peer.name), { paths: apiPaths(paths) }).then(function () {
    toast(done);
    state.detailKey = '';
    return refresh();
  });
}

// pathForm adds a path to the peer, or edits its path number index.
function pathForm(peer, index) {
  var old = index >= 0 ? peer.paths[index] : { name: '', type: 'tcp', addr: '', proxy: '', security: 'tls', verify: '', pin: '', roles: ['data'], subs: 8 };
  var name = h('input', { value: old.name, required: true, maxlength: '32' });
  var addr = h('input', { value: old.addr, required: true, placeholder: '203.0.113.5:9851' });
  var type = select([['tcp', t('paths.type_tcp')], ['socks5', t('paths.type_socks5')]], old.type);
  var proxy = h('input', { value: old.proxy || '', placeholder: 'socks5://127.0.0.1:1080' });
  var security = select([['tls', t('paths.sec_tls')], ['owncert', t('paths.sec_owncert')], ['off', t('paths.sec_off')]], old.security);
  var verify = select([['pin', t('paths.verify_pin')], ['ca', t('paths.verify_ca')]], old.verify || 'pin');
  var pin = h('input', { value: old.pin || '', placeholder: t('paths.pin_placeholder') });
  var roleNames = { data: t('paths.role_data'), repair: t('paths.role_repair'), control: t('paths.role_control') };
  var roles = Object.keys(roleNames).map(function (r) {
    var c = checkbox(roleNames[r], old.roles.indexOf(r) >= 0);
    c.role = r;
    return c;
  });
  var subs = h('input', { type: 'number', min: '1', max: '64', value: String(old.subs), required: true });
  openDialog({
    title: index >= 0 ? t('paths.edit_title', old.name) : t('paths.add'),
    body: [
      field(t('col.name'), name),
      field(t('paths.addr'), addr, t('paths.addr_hint')),
      field(t('paths.type'), type),
      field(t('paths.proxy'), proxy, t('paths.proxy_hint')),
      field(t('paths.security'), security, t('paths.security_hint')),
      field(t('paths.verify'), verify, t('paths.verify_hint')),
      field(t('paths.pin'), pin),
      h('p', { class: 'hint', text: t('paths.roles') }),
      roles.map(function (c) { return c.el; }),
      field(t('paths.subs'), subs, t('paths.subs_hint'))
    ],
    ok: t('act.save'),
    submit: function () {
      var p = {
        name: name.value.trim(), type: type.value, addr: addr.value.trim(),
        proxy: type.value === 'socks5' ? proxy.value.trim() : '',
        security: security.value,
        verify: security.value === 'owncert' ? verify.value : '',
        pin: security.value === 'owncert' && verify.value === 'pin' ? pin.value.trim() : '',
        roles: roles.filter(function (c) { return c.box.checked; }).map(function (c) { return c.role; }),
        subs: Number(subs.value)
      };
      var paths = peer.paths.slice();
      if (index >= 0) paths[index] = p; else paths.push(p);
      return savePaths(peer, paths, t('msg.path_saved', p.name));
    }
  });
}

function renderPeerDetail() {
  var box = $('#peer-detail');
  var peer = null;
  state.peers.forEach(function (p) { if (p.name === state.peer) peer = p; });
  if (!peer) { box.hidden = true; state.detailKey = ''; return; }
  var check = state.checks[peer.name];
  var key = lang + JSON.stringify(peer) + JSON.stringify(check || null);
  if (key === state.detailKey && !box.hidden) return; // nothing changed: leave what the user has open
  state.detailKey = key;
  box.hidden = false;

  var rows = peer.paths.map(function (p, i) {
    return h('tr', {},
      h('td', { text: p.name }),
      h('td', { class: 'wrap', text: p.addr + (p.type === 'socks5' ? ' (' + t('paths.via', p.proxy || 'socks5') + ')' : '') }),
      h('td', { text: pathSummary(p) }),
      h('td', { text: p.roles.join(', ') }),
      num(String(p.subs)),
      h('td', { class: 'actions' },
        h('button', { type: 'button', class: 'small', text: t('act.edit'), onclick: function () { pathForm(peer, i); } }),
        h('button', {
          type: 'button', class: 'small danger', text: t('act.delete'), onclick: function () {
            confirmDialog(t('paths.delete_title', p.name), t('paths.delete_text'), t('act.delete'), function () {
              var paths = peer.paths.slice();
              paths.splice(i, 1);
              return savePaths(peer, paths, t('msg.path_deleted', p.name));
            });
          }
        })));
  });
  var paths = h('table', {});
  table(paths, [t('col.name'), t('paths.addr'), t('paths.security'), t('paths.roles_short'), { num: t('paths.subs_short') }, ''], rows, t('paths.none'));

  var results = null;
  if (check) {
    results = h('div', {}, check.paths.length === 0 ? h('p', { class: 'warn', text: t('check.no_paths') }) : check.paths.map(function (r) {
      return h('p', { class: r.ok ? 'ok' : 'bad', text: r.ok ? t('check.ok', r.name, r.addr) : t('check.failed', r.name, r.addr, r.error || '') });
    }));
  }

  var checkBtn = h('button', {
    type: 'button', text: t('check.run'), onclick: function () {
      checkBtn.disabled = true;
      checkBtn.textContent = t('check.running');
      api('POST', 'peers/' + seg(peer.name) + '/check').then(function (res) {
        state.checks[peer.name] = res;
      }).catch(function (e) { toast(e.message, true); }).then(function () { state.detailKey = ''; renderPeerDetail(); });
    }
  });

  box.replaceChildren();
  add(box, [
    h('div', { class: 'bar' },
      h('h3', { text: t('peers.detail_title', peer.name) }),
      h('button', { type: 'button', class: 'small', text: t('act.close'), onclick: function () { state.peer = ''; renderPeerDetail(); } })),
    h('dl', { class: 'facts' },
      h('dt', { text: t('ov.fingerprint') }), h('dd', {}, h('code', { class: 'wrap', text: peer.fingerprint })),
      h('dt', { text: t('peers.paired_at') }), h('dd', { text: fmtTime(peer.paired_at) }),
      h('dt', { text: t('peers.addresses') }), h('dd', { class: 'wrap', text: peer.addresses.length ? peer.addresses.join(', ') : t('peers.no_addresses') })),
    h('div', { class: 'bar' },
      h('h3', { text: t('paths.title') }),
      h('button', { type: 'button', text: t('paths.add'), onclick: function () { pathForm(peer, -1); } }),
      checkBtn),
    h('div', { class: 'table-wrap' }, paths),
    results,
    h('details', {},
      h('summary', { text: t('peers.advanced') }),
      h('dl', { class: 'facts' },
        h('dt', { text: t('peers.allow') }), h('dd', { class: 'wrap', text: peer.allow || t('peers.allow_any') }),
        h('dt', { text: t('peers.udp_dial') }), h('dd', { class: 'wrap', text: peer.udp_dial || t('peers.udp_none') })),
      h('button', { type: 'button', class: 'small', text: t('act.edit'), onclick: function () { peerOptionsForm(peer); } })),
    h('p', {}, h('button', {
      type: 'button', class: 'danger', text: t('peers.remove'), onclick: function () {
        confirmDialog(t('peers.remove_title', peer.name), t('peers.remove_text'), t('peers.remove'), function () {
          return api('DELETE', 'peers/' + seg(peer.name)).then(function () {
            toast(t('msg.peer_removed', peer.name));
            state.peer = '';
            return refresh();
          });
        });
      }
    }))]);
}

function peerOptionsForm(peer) {
  var allow = h('input', { value: peer.allow || '', placeholder: '192.0.2.0/24:873, 198.51.100.7/32' });
  var udp = h('input', { value: peer.udp_dial || '', placeholder: '203.0.113.5:9852' });
  openDialog({
    title: t('peers.advanced_title', peer.name),
    body: [field(t('peers.allow'), allow, t('peers.allow_hint')), field(t('peers.udp_dial'), udp, t('peers.udp_hint'))],
    ok: t('act.save'),
    submit: function () {
      return api('PUT', 'peers/' + seg(peer.name), { allow: allow.value.trim(), udp_dial: udp.value.trim() }).then(function () {
        toast(t('msg.saved'));
        state.detailKey = '';
        return refresh();
      });
    }
  });
}

function createCodeForm() {
  var addresses = h('input', { placeholder: '203.0.113.5:9851' });
  var ttl = h('input', { type: 'number', min: '1', max: '43200', value: '60', required: true });
  openDialog({
    title: t('peers.create_code'),
    body: [h('p', { class: 'hint', text: t('codes.create_intro') }),
      field(t('codes.addresses'), addresses, t('codes.addresses_hint')),
      field(t('codes.ttl'), ttl)],
    ok: t('codes.create'),
    submit: function () {
      var req = { ttl_seconds: Math.round(Number(ttl.value) * 60) };
      var list = addresses.value.split(',').map(function (a) { return a.trim(); }).filter(Boolean);
      if (list.length) req.addresses = list;
      return api('POST', 'pairing-codes', req).then(function (code) {
        refresh();
        var text = h('textarea', { readonly: true, rows: '5' });
        text.value = code.code;
        var body = h('div', {},
          h('p', { text: t('codes.made_intro') }),
          text,
          h('p', {}, h('button', { type: 'button', text: t('act.copy'), onclick: function () { copyText(code.code, $('#dlg')); } })),
          h('p', { class: 'hint', text: t('codes.made_expires', fmtTime(code.expires)) }),
          h('p', { class: 'warn', text: t('codes.made_once') }));
        openDialog({ title: t('codes.made_title'), body: body, ok: t('act.close'), noCancel: true });
        return true;
      });
    }
  });
}

function enterCodeForm() {
  var code = h('textarea', { required: true, rows: '5', spellcheck: 'false', autocomplete: 'off' });
  var address = h('input', { placeholder: '203.0.113.5:9851' });
  openDialog({
    title: t('peers.enter_code'),
    body: [field(t('codes.code'), code, t('codes.code_hint')), field(t('codes.address'), address, t('codes.address_hint'))],
    ok: t('codes.join'),
    submit: function () {
      return api('POST', 'peers', { code: code.value.trim(), address: address.value.trim() }).then(function (peer) {
        toast(t('msg.paired', peer.name));
        state.peer = peer.name;
        state.detailKey = '';
        return refresh();
      });
    }
  });
}

// ---------- pages, refresh, login ----------

var renderers = { overview: renderOverview, peers: renderPeers, rules: renderRules, sessions: renderSessions, settings: function () {} };

function setPage(name) {
  if (!renderers[name]) name = 'overview';
  state.page = name;
  document.querySelectorAll('.page').forEach(function (el) { el.hidden = el.id !== 'page-' + name; });
  document.querySelectorAll('#nav a').forEach(function (a) { a.classList.toggle('active', a.dataset.page === name); });
  if (name === 'settings' && state.status) {
    $('#set-name').value = state.status.node.name;
    $('#set-version').textContent = state.status.version;
  }
  refresh();
}

function updateRates(sessions) {
  var now = Date.now(), prev = {}, rates = {};
  sessions.forEach(function (s) {
    var was = state.prev[s.id];
    if (was && now > was.at) {
      var dt = (now - was.at) / 1000;
      rates[s.id] = { tx: Math.max(0, (s.tx_bytes - was.tx) / dt), rx: Math.max(0, (s.rx_bytes - was.rx) / dt) };
    }
    prev[s.id] = { tx: s.tx_bytes, rx: s.rx_bytes, at: now };
  });
  state.prev = prev;
  state.rates = rates;
}

// refresh reads what the page in front shows and draws it. It runs every two seconds
// and after every change.
function refresh() {
  if (!state.loggedIn || busy) return Promise.resolve();
  busy = true;
  var page = state.page;
  var reads = [api('GET', 'status'), api('GET', 'sessions')];
  if (page === 'rules') reads.push(api('GET', 'rules'));
  if (page === 'peers') reads.push(api('GET', 'peers'), api('GET', 'pairing-codes'));
  return Promise.all(reads).then(function (res) {
    state.status = res[0];
    updateRates(res[1]);
    state.sessions = res[1];
    if (page === 'rules') state.rules = res[2];
    if (page === 'peers') { state.peers = res[2]; state.codes = res[3]; }
    $('#node-name').textContent = state.status.node.name;
    document.title = 'tcpraid - ' + state.status.node.name;
    renderers[state.page]();
  }).catch(function (e) {
    if (state.loggedIn) toast(e.message, true);
  }).then(function () {
    busy = false;
    if (state.page !== page) return refresh(); // the page changed meanwhile: read what it shows
  });
}

function showLogin() {
  state.loggedIn = false;
  var dlg = $('#dlg');
  if (dlg.open) dlg.close();
  $('#app').hidden = true;
  $('#login').hidden = false;
  $('#login-password').focus();
}

function showApp() {
  state.loggedIn = true;
  $('#login').hidden = true;
  $('#app').hidden = false;
  setPage(location.hash.slice(1) || 'overview');
}

function setLang(l) {
  lang = dict[l] ? l : 'en';
  try { localStorage.setItem('tcpraid.lang', lang); } catch (e) { /* the choice lasts for this visit only */ }
  $('#set-lang').value = lang;
  applyI18n();
  state.detailKey = '';
  if (state.loggedIn) renderers[state.page]();
}

function start() {
  $('#login-form').addEventListener('submit', function (e) {
    e.preventDefault();
    var pw = $('#login-password');
    $('#login-error').textContent = '';
    api('POST', 'login', { password: pw.value }).then(function () {
      pw.value = '';
      showApp();
    }).catch(function (err) { $('#login-error').textContent = err.message; });
  });
  $('#dlg-form').addEventListener('submit', function (e) {
    e.preventDefault();
    var dlg = $('#dlg'), ok = $('#dlg-ok');
    if (!dlgSubmit) { dlg.close(); return; }
    ok.disabled = true;
    $('#dlg-error').textContent = '';
    dlgSubmit().then(function (keep) {
      ok.disabled = false;
      if (keep !== true) dlg.close();
    }, function (err) {
      ok.disabled = false;
      $('#dlg-error').textContent = err.message;
    });
  });
  $('#dlg-cancel').addEventListener('click', function () { $('#dlg').close(); });
  $('#ov-copy').addEventListener('click', function () { if (state.status) copyText(state.status.node.fingerprint); });
  $('#code-create').addEventListener('click', createCodeForm);
  $('#code-enter').addEventListener('click', enterCodeForm);
  $('#rule-new').addEventListener('click', function () { ruleForm(null, false); });
  $('#trule-new').addEventListener('click', function () { ruleForm(null, true); });
  $('#set-lang').addEventListener('change', function () { setLang($('#set-lang').value); });
  $('#set-name-form').addEventListener('submit', function (e) {
    e.preventDefault();
    api('PUT', 'node', { name: $('#set-name').value.trim() }).then(function (n) {
      toast(t('msg.renamed', n.name));
      return refresh();
    }).catch(function (err) { toast(err.message, true); });
  });
  $('#logout').addEventListener('click', function () {
    api('POST', 'logout').catch(function () { /* the login is gone either way */ }).then(showLogin);
  });
  window.addEventListener('hashchange', function () { if (state.loggedIn) setPage(location.hash.slice(1)); });
  if (DOCS_URL) {
    var docs = document.querySelector('[data-i18n="settings.docs_text"]');
    docs.removeAttribute('data-i18n');
    docs.replaceChildren(h('a', { href: DOCS_URL, rel: 'noreferrer', target: '_blank', text: DOCS_URL }));
  }

  fetch('i18n.json', { cache: 'no-cache' }).then(function (r) { return r.json(); }).then(function (d) { dict = d; }, function () { /* keys are shown instead */ }).then(function () {
    var want = '';
    try { want = localStorage.getItem('tcpraid.lang') || ''; } catch (e) { want = ''; }
    if (!want) want = (navigator.language || '').toLowerCase().indexOf('zh') === 0 ? 'zh' : 'en';
    setLang(want);
    // is there a login already?
    return fetch(API + 'status', { credentials: 'same-origin', cache: 'no-store' });
  }).then(function (r) {
    if (r.ok) showApp(); else showLogin();
  }, showLogin);
  setInterval(refresh, 2000);
}

start();
