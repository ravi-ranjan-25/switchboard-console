(function () {
  'use strict';
  var csrf = document.querySelector('meta[name="csrf-token"]').content;
  var $ = function (id) { return document.getElementById(id); };
  var MODES = ['public', 'google', 'google-restricted', 'private'];
  var MODE_HELP = { 'public': 'Anyone on the internet', 'google': 'Any Google account', 'google-restricted': 'Only listed emails', 'private': 'Tailnet only' };
  var state = { me: {}, apps: [], filter: 'all', q: '', detail: null, tab: 'overview', busy: {} };

  // ---------- helpers ----------
  function h(tag, attrs) {
    var el = document.createElement(tag), i, c;
    if (attrs) for (var k in attrs) {
      var v = attrs[k];
      if (v === false || v == null) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k.slice(0, 2) === 'on') el.addEventListener(k.slice(2), v);
      else if (v === true) el.setAttribute(k, '');
      else el.setAttribute(k, v);
    }
    for (i = 2; i < arguments.length; i++) {
      c = arguments[i];
      if (c == null || c === false) continue;
      if (Array.isArray(c)) c.forEach(function (x) { if (x) el.appendChild(x); });
      else el.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
    }
    return el;
  }
  function icon(name) {
    var s = document.createElementNS('http://www.w3.org/2000/svg', 'svg'), u = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    s.setAttribute('class', 'ico'); s.setAttribute('aria-hidden', 'true');
    u.setAttribute('href', '#i-' + name); s.appendChild(u); return s;
  }
  function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }
  function api(method, path, body) {
    var o = { method: method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) { o.headers['Content-Type'] = 'application/json'; o.headers['X-CSRF-Token'] = csrf; o.body = JSON.stringify(body); }
    return fetch(path, o).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) { return { ok: r.ok, status: r.status, j: j }; });
    });
  }
  function toast(msg, isErr) {
    var t = h('div', { class: 'toast' + (isErr ? ' err' : '') }, icon(isErr ? 'warn' : 'check'), h('span', { text: msg }));
    $('toasts').appendChild(t);
    setTimeout(function () { if (t.parentNode) t.parentNode.removeChild(t); }, isErr ? 7000 : 3800);
  }
  function ago(iso) {
    if (!iso || iso.indexOf('0001') === 0) return '';
    var s = (Date.now() - new Date(iso).getTime()) / 1000;
    if (s < 90) return 'just now'; if (s < 5400) return Math.round(s / 60) + ' min ago';
    if (s < 129600) return Math.round(s / 3600) + ' h ago'; return Math.round(s / 86400) + ' d ago';
  }
  function modeBadge(m) { return h('span', { class: 'badge m-' + (m || 'none'), text: m || 'no exposure' }); }
  function pill(p) { return h('span', { class: 'pill pill-' + p, text: p }); }
  function closeBtns(dlg) { dlg.querySelectorAll('[data-close]').forEach(function (b) { b.onclick = function () { dlg.close(); }; }); }
  function openDlg(d) { if (!d.open) d.showModal(); }
  function field(label, input, hint, cls, name) {
    var id = input.id || (input.id = 'f-' + Math.random().toString(36).slice(2, 8));
    var f = h('div', { class: 'field ' + (cls || ''), 'data-field': name || '' }, h('label', { for: id, text: label }), input);
    if (hint) f.appendChild(h('span', { class: 'hint', text: hint }));
    return f;
  }
  function setInvalid(root, errors) {
    root.querySelectorAll('.field.invalid').forEach(function (f) { f.classList.remove('invalid'); var m = f.querySelector('.msg'); if (m) m.remove(); });
    var general = [];
    (errors || []).forEach(function (e) {
      var f = e.field && root.querySelector('[data-field="' + e.field + '"]');
      if (f) { f.classList.add('invalid'); f.appendChild(h('span', { class: 'msg', role: 'alert', text: e.msg })); } else general.push(e.msg);
    });
    var first = root.querySelector('.field.invalid input, .field.invalid select, .field.invalid textarea'); if (first) first.focus();
    return general;
  }

  // ---------- activity tray ----------
  function track(jobId, title, onEvent, onDone) {
    var tray = $('tray'); tray.hidden = false;
    var log = h('ol', { class: 'log', 'aria-live': 'polite' }), spin = h('span', { class: 'spin' });
    var li = h('li', null, h('div', { class: 'job' }, h('div', { class: 'jt' }, spin, h('span', { text: title })), log));
    $('tray-list').insertBefore(li, $('tray-list').firstChild);
    var es = new EventSource('/api/jobs/' + encodeURIComponent(jobId) + '/events');
    es.onmessage = function (m) {
      var ev = JSON.parse(m.data);
      log.appendChild(h('li', { class: ev.state, text: ev.msg }));
      li.firstChild.className = 'job ' + ev.state;
      if (onEvent) onEvent(ev);
      if (ev.state === 'done' || ev.state === 'error') {
        es.close(); toast(ev.state === 'done' ? title + ': done' : title + ': ' + ev.msg, ev.state === 'error');
        refresh(true); if (onDone) onDone(ev);
        if (state.detail && ev.state === 'done') loadDetail(state.detail.name, true);
      }
    };
    es.onerror = function () { es.close(); };
    return li;
  }
  $('tray-clear').onclick = function () {
    $('tray-list').querySelectorAll('li').forEach(function (li) { var c = li.firstChild.className; if (/done|error/.test(c)) li.remove(); });
    if (!$('tray-list').children.length) $('tray').hidden = true;
  };

  // ---------- review component ----------
  function diffEl(text) {
    var pre = h('pre', { class: 'diff', tabindex: '0', 'aria-label': 'Diff' });
    (text || '(no textual change)').split('\n').forEach(function (ln) {
      if (ln === '' ) return;
      var c = 'dl';
      if (ln.indexOf('+++') === 0 || ln.indexOf('---') === 0) c += ' meta'; else if (ln[0] === '@') c += ' hunk'; else if (ln[0] === '+') c += ' add'; else if (ln[0] === '-') c += ' del';
      pre.appendChild(h('span', { class: c, text: ln }));
    });
    return pre;
  }
  function review(plan, opts) {
    opts = opts || {};
    var wrap = h('div', null);
    var diffHost = h('div'), btns = [];
    function show(i) {
      btns.forEach(function (b, j) { b.setAttribute('aria-current', j === i ? 'true' : 'false'); });
      clear(diffHost).appendChild(diffEl(plan.files[i].diff));
    }
    var nrepo = plan.files.filter(function (f) { return f.repo; }).length;
    if (nrepo) wrap.appendChild(h('p', { class: 'summary' }, h('b', { text: nrepo + ' files' }), ' go into a NEW private repo first (the CI workflow is pushed last). Then the ' + (plan.files.length - nrepo) + ' citadel files below.'));
    wrap.appendChild(h('p', { class: 'summary' }, (plan.files.length - nrepo) + ' file' + (plan.files.length - nrepo === 1 ? '' : 's') + ' will be committed to ', h('code', { text: state.me.branch || 'main' }), ' in order, as: ', h('code', { text: plan.commit_message })));
    if (plan.danger && plan.danger.length) wrap.appendChild(h('div', { class: 'danger-box', role: 'alert' }, plan.danger.map(function (d) { return h('span', { text: d }); })));
    var list = h('ul', { class: 'flist', 'aria-label': 'Files' });
    plan.files.forEach(function (f, i) {
      var b = h('button', { type: 'button', class: 'fbtn', onclick: function () { show(i); } }, h('span', { class: 'act act-' + f.action, text: f.action }), h('span', null, f.repo ? h('b', { class: 'repo', text: f.repo.split('/')[1] + ' / ' }) : null, f.path));
      btns.push(b); list.appendChild(h('li', null, b));
    });
    wrap.appendChild(h('div', { class: 'review' }, list, diffHost));
    if (btns.length) show(0);
    var ul = h('ul', { class: 'checks', 'aria-label': 'Validation' });
    (plan.checks || []).forEach(function (c) {
      ul.appendChild(h('li', { class: c.status }, icon(c.status === 'ok' ? 'check' : 'warn'), h('span', { class: 't' }, h('b', { text: c.name + ': ' }), c.detail || (c.status === 'ok' ? 'ok' : ''))));
    });
    wrap.appendChild(h('h3', { text: plan.ok ? 'Validation passed' : 'Validation failed', class: plan.ok ? '' : 'err' }));
    wrap.appendChild(ul);
    if (plan.notes && plan.notes.length) wrap.appendChild(h('ul', { class: 'notes' }, plan.notes.map(function (n) { return h('li', { text: n }); })));
    return wrap;
  }

  // ---------- generic confirm flow for single ops ----------
  function reviewAndApply(op, o) {
    var dlg = $('confirm'), body = clear($('cf-body')), foot = clear($('cf-foot'));
    $('cf-title').textContent = o.title; body.appendChild(h('p', { class: 'muted', text: 'Checking…' }));
    foot.appendChild(h('button', { type: 'button', class: 'btn', text: 'Cancel', onclick: function () { dlg.close(); } }));
    openDlg(dlg);
    return api('POST', '/api/plan', op).then(function (r) {
      clear(body);
      if (!r.ok || !r.j.ok) {
        var msgs = (r.j.errors || []).map(function (e) { return e.msg; });
        if (r.j.checks) r.j.checks.forEach(function (c) { if (c.status === 'fail') msgs.push(c.name + ': ' + c.detail); });
        body.appendChild(h('div', { class: 'err', role: 'alert', text: msgs.join(' | ') || r.j.error || 'Cannot do that.' }));
        return;
      }
      body.appendChild(review(r.j));
      var typed, ack, go = h('button', { type: 'button', class: 'btn ' + (o.danger ? 'danger' : 'primary'), text: o.confirm || 'Commit' });
      if (o.typeName) {
        typed = h('input', { id: 'cf-typed', autocomplete: 'off', spellcheck: 'false', placeholder: op.app });
        body.appendChild(h('div', { class: 'field' }, h('label', { for: 'cf-typed', text: 'Type "' + op.app + '" to confirm' }), typed));
        if (r.j.danger && r.j.danger.length > 1) {
          ack = h('input', { type: 'checkbox', id: 'cf-ack' });
          body.appendChild(h('div', { class: 'check' }, ack, h('label', { for: 'cf-ack', text: 'I understand the volume and its data will be deleted.' })));
        }
        go.disabled = true;
        var upd = function () { go.disabled = typed.value !== op.app || (ack && !ack.checked); };
        typed.oninput = upd; if (ack) ack.onchange = upd;
      }
      go.onclick = function () {
        go.disabled = true;
        var req = Object.assign({}, op, { hash: r.j.hash });
        if (o.typeName) { req.confirm = typed.value; req.ack_data_loss = !!(ack && ack.checked); }
        api('POST', '/api/apply', req).then(function (a) {
          if (!a.ok) { toast(a.j.error || 'Failed', true); go.disabled = false; if (a.j.reload) { dlg.close(); refresh(true); } return; }
          dlg.close(); toast('Committing…'); track(a.j.job, o.title); markPending(op.app);
        });
      };
      foot.appendChild(go);
      (typed || go).focus();
    });
  }
  function markPending(app) { state.apps.forEach(function (a) { if (a.name === app) a.pending = true; }); render(); }

  // ---------- two-step flow dialogs (new app, add domain) ----------
  function flow(cfg) {
    var dlg = $(cfg.dialog), body = clear($(cfg.body)), foot = clear($(cfg.foot)), steps = $(cfg.steps).children;
    var form = cfg.build();
    function step(n) { Array.prototype.forEach.call(steps, function (li, i) { li.className = i < n ? 'done' : ''; if (i === n) li.setAttribute('aria-current', 'step'); else li.removeAttribute('aria-current'); }); }
    function stepOne() {
      step(0); clear(body).appendChild(form.el); clear(foot);
      var gen = h('div', { class: 'err', role: 'alert', hidden: true });
      var go = h('button', { type: 'button', class: 'btn primary', text: 'Preview files' });
      go.onclick = function () {
        go.disabled = true; gen.hidden = true;
        api('POST', '/api/plan', form.op()).then(function (r) {
          go.disabled = false;
          if (r.status === 400 || r.status === 502 || (!r.ok && !r.j.errors)) { gen.textContent = r.j.error || 'Request failed'; gen.hidden = false; return; }
          var general = setInvalid(form.el, r.j.errors);
          if (general.length) { gen.textContent = general.join(' | '); gen.hidden = false; }
          if (r.ok) stepTwo(r.j);
        });
      };
      foot.appendChild(h('button', { type: 'button', class: 'btn ghost', text: 'Cancel', onclick: function () { dlg.close(); } }));
      foot.appendChild(h('span', { class: 'grow' }, gen)); foot.appendChild(go);
    }
    function stepTwo(plan) {
      step(1); clear(body).appendChild(review(plan)); clear(foot);
      var go = h('button', { type: 'button', class: 'btn primary', text: cfg.commitLabel, disabled: !plan.ok });
      foot.appendChild(h('button', { type: 'button', class: 'btn', text: 'Back', onclick: stepOne }));
      foot.appendChild(h('span', { class: 'grow' }));
      if (!plan.ok) foot.insertBefore(h('span', { class: 'err', role: 'alert', text: 'Validation failed, fix the form first.' }), foot.lastChild);
      foot.appendChild(go);
      go.onclick = function () {
        go.disabled = true;
        api('POST', '/api/apply', Object.assign(form.op(), { hash: plan.hash })).then(function (a) {
          if (!a.ok) { toast(a.j.error || 'Failed', true); go.disabled = false; if (a.j.reload) stepOne(); return; }
          stepThree(a.j.job);
        });
      };
      go.focus();
    }
    function stepThree(job) {
      step(2); clear(foot);
      var log = h('ol', { class: 'log', 'aria-live': 'polite' }), status = h('p', { class: 'summary', text: 'Committing…' });
      clear(body).appendChild(h('div', null, status, log));
      var close = h('button', { type: 'button', class: 'btn primary', text: 'Close', disabled: true, onclick: function () { dlg.close(); } });
      foot.appendChild(h('span', { class: 'grow' })); foot.appendChild(close);
      track(job, cfg.title, function (ev) {
        log.appendChild(h('li', { class: ev.state, text: ev.msg }));
        if (ev.state === 'done') status.textContent = 'Done.'; else if (ev.state === 'error') status.textContent = 'Stopped.';
        if (ev.state === 'done' || ev.state === 'error') { close.disabled = false; close.focus(); }
      });
    }
    stepOne(); openDlg(dlg);
    var first = form.el.querySelector('input,select'); if (first) first.focus();
  }

  // ---------- New app form ----------
  function newAppForm() {
    var f = {};
    function inp(id, o) { return f[id] = h('input', Object.assign({ id: 'na-' + id, autocomplete: 'off', spellcheck: 'false' }, o || {})); }
    function sel(id, opts) { return f[id] = h('select', { id: 'na-' + id }, opts.map(function (o) { return h('option', { value: o[0], text: o[1] }); })); }
    function radio(name, v, title, sub, checked) {
      var i = h('input', { type: 'radio', name: 'na-' + name, value: v, checked: checked });
      return { i: i, el: h('label', { class: 'segopt' }, i, h('span', null, h('b', { text: title }), h('small', { text: sub }))) };
    }
    var canCreate = !!state.me.repo_create, stacks = state.me.stacks || [];
    var ik = [radio('kind', 'source', 'New project', canCreate ? 'Private repo with code, tests, CI' : 'Not configured (needs a token)', canCreate),
      radio('kind', 'built', 'Existing repo', 'GHCR image built by my CI', !canCreate), radio('kind', 'pinned', 'Public image', 'Pinned version or digest', false)];
    ik[0].i.disabled = !canCreate;
    var md = MODES.map(function (m) { return radio('mode', m, m, MODE_HELP[m], m === 'private'); });
    inp('name', { placeholder: 'notes', maxlength: 40 }); inp('image', { placeholder: 'my-service' }); inp('port', { type: 'number', min: 1, max: 65535, placeholder: '3000', inputmode: 'numeric' });
    inp('host', { placeholder: 'notes.example.com' }); inp('emails', { placeholder: 'a@example.com, b@example.com' });
    sel('namespace', (state.me.namespaces || ['personal']).map(function (n) { return [n, n]; }));
    inp('pvc', { placeholder: 'e.g. 2Gi (empty = no volume)' }); inp('datapath', { value: '/data' }); inp('uid', { type: 'number', value: '1000', min: 1 }); inp('health', { value: '/' });
    sel('size', [['small', 'small (50m / 128Mi, up to 500m / 384Mi)'], ['tiny', 'tiny (10m / 32Mi, up to 250m / 128Mi)'], ['medium', 'medium (100m / 256Mi, up to 1 CPU / 768Mi)']]);
    inp('title', { placeholder: 'Notes' }); inp('desc', { placeholder: 'One line for the homepage' });
    sel('group', [['Apps', 'Apps'], ['Tools', 'Tools'], ['Platform', 'Platform']]);
    f.dbmode = h('select', { id: 'na-dbmode' }, [['none', 'No database'], ['new', 'A new database for this app (role, database, password, DATABASE_URL)'], ['access', 'Network access to the shared Postgres only']].map(function (o) { return h('option', { value: o[0], text: o[1] }); }));
    f.secrets = h('textarea', { id: 'na-secrets', placeholder: 'API_KEY\nDATABASE_URL', spellcheck: 'false' });
    f.env = h('textarea', { id: 'na-env', placeholder: 'LOG_LEVEL=info\nTZ=UTC', spellcheck: 'false' });
    f.replicas = sel('replicas', [['auto', 'Automatic (0 for built images until CI has run, else 1)'], ['1', '1'], ['0', '0']]);
    var imgHint = h('span', { class: 'hint' });
    f.stack = h('select', { id: 'na-stack' }, stacks.map(function (st) { return h('option', { value: st.id, text: st.label }); }));
    f.repo = h('input', { id: 'na-repo', autocomplete: 'off', spellcheck: 'false', placeholder: 'same as the app name' });
    var stackHint = h('span', { class: 'hint' });
    function kind() { return ik[0].i.checked ? 'source' : ik[1].i.checked ? 'built' : 'pinned'; }
    function show(sel, on) { var e = el.querySelectorAll(sel); e.forEach(function (x) { x.hidden = !on; }); }
    function sync() {
      var src = kind() === 'source', built = kind() !== 'pinned' && !src;
      show('[data-field="source_stack"], [data-field="source_repo"]', src);
      show('[data-field="image"], [data-field="port"], [data-field="health_path"], [data-field="uid"]', !src);
      var st = stacks.filter(function (x) { return x.id === f.stack.value; })[0];
      if (st) stackHint.textContent = 'Listens on ' + st.port + ', health check ' + st.health + '. The template includes tests, Dockerfile, CI workflow, docker-compose and a README.';
      f.image.placeholder = built ? 'repo name, e.g. my-service' : 'docker.io/library/nginx:1.27.2';
      imgHint.textContent = built ? 'Image becomes ' + (state.me.image_prefix || 'ghcr.io/example-owner/') + '<name>. Flux image automation is added.' : 'Registry host, repository and a version tag or @sha256 digest. Never :latest.';
      var restricted = md[2].i.checked; f.emails.closest('.field').hidden = !restricted;
    }
    ik.concat(md).forEach(function (r) { r.i.addEventListener('change', sync); });
    f.stack.addEventListener('change', sync);
    var el = h('div', { class: 'form' },
      field('Name', f.name, 'Lowercase, digits and dashes. Used for the namespace objects, labels and the Flux Kustomization.', '', 'name'),
      field('Namespace', f.namespace, null, '', 'namespace'),
      h('fieldset', { class: 'full', 'data-field': 'image_kind' }, h('legend', { text: 'Image' }), h('div', { class: 'seg' }, ik.map(function (r) { return r.el; }))),
      h('div', { class: 'field', 'data-field': 'source_stack' }, h('label', { for: 'na-stack', text: 'Language and framework' }), f.stack, stackHint),
      h('div', { class: 'field', 'data-field': 'source_repo' }, h('label', { for: 'na-repo', text: 'New private repo name' }), f.repo, h('span', { class: 'hint', text: 'Created under your GitHub account as a private repo. Pushing to main builds and deploys.' })),
      h('div', { class: 'field full', 'data-field': 'image' }, h('label', { for: 'na-image', text: 'Image reference' }), f.image, imgHint),
      field('Container port', f.port, 'The port the app listens on.', '', 'port'),
      field('Host', f.host, 'Public hostname. Its DNS must already exist.', '', 'host'),
      h('fieldset', { class: 'full', 'data-field': 'mode' }, h('legend', { text: 'Who can reach it' }), h('div', { class: 'seg' }, md.map(function (r) { return r.el; }))),
      field('Allowed emails', f.emails, 'Comma separated. Only these Google accounts get in.', 'full', 'allowed_emails'),
      h('details', { class: 'adv' }, h('summary', { text: 'Storage, database, secrets and more' }),
        h('div', { class: 'form' },
          field('Persistent volume size', f.pvc, 'Adds a PVC with nightly restic backup and restore-if-empty.', '', 'pvc_size'),
          field('Mount path', f.datapath, null, '', 'data_path'),
          field('Postgres database', f.dbmode, 'New database: creates a role and database on the shared cluster, generates the password (you run make secrets and make vault-push afterwards) and wires DATABASE_URL.', 'full', 'database'),
          field('Run as user id', f.uid, 'Non-root. Also the backup uid.', '', 'uid'),
          field('Health check path', f.health, null, '', 'health_path'),
          field('Size', f.size, null, '', 'size'),
          field('Initial replicas', f.replicas, null, '', 'replicas'),
          field('Secret names (one per line)', f.secrets, 'Names only, like API_KEY. Creates an ExternalSecret mapping each to app-<name>--<kebab> in the vault. Enter the values with make secret, never here.', 'full', 'secrets'),
          field('Plain environment variables (NAME=value per line)', f.env, 'Not for secrets: names that look secret are refused.', 'full', 'env'),
          field('Homepage title', f.title, null, '', 'title'), field('Homepage group', f.group, null, '', 'group'),
          field('Homepage description', f.desc, null, 'full', 'description'))));
    sync();
    return {
      el: el,
      op: function () {
        var env = f.env.value.split('\n').map(function (l) { return l.trim(); }).filter(Boolean).map(function (l) { var i = l.indexOf('='); return { name: i < 0 ? l : l.slice(0, i).trim(), value: i < 0 ? '' : l.slice(i + 1) }; });
        var mode = md.filter(function (r) { return r.i.checked; })[0].i.value;
        var src = kind() === 'source';
        var n = { name: f.name.value.trim(), image_kind: kind() === 'pinned' ? 'pinned' : 'built', image: f.image.value.trim(), port: parseInt(f.port.value, 10) || 0, mode: mode,
          host: f.host.value.trim(), allowed_emails: f.emails.value, namespace: f.namespace.value, pvc_size: f.pvc.value.trim(), data_path: f.datapath.value.trim(),
          db_client: f.dbmode.value !== 'none', database: f.dbmode.value === 'new', uid: parseInt(f.uid.value, 10) || 0, health_path: f.health.value.trim(), size: f.size.value,
          secrets: f.secrets.value.split(/[\n,]+/).map(function (s) { return s.trim(); }).filter(Boolean), env: env,
          title: f.title.value.trim(), description: f.desc.value.trim(), group: f.group.value };
        if (f.replicas.value !== 'auto') n.replicas = parseInt(f.replicas.value, 10);
        if (src) { n.source = { stack: f.stack.value, repo: f.repo.value.trim(), description: f.desc.value.trim() }; n.image = ''; n.port = 0; n.health_path = ''; n.uid = 0; if (f.size.value === 'small') n.size = ''; }
        return { op: 'new-app', new: n };
      }
    };
  }

  function domainForm() {
    var d = h('input', { id: 'dm-domain', placeholder: 'example.com', autocomplete: 'off', spellcheck: 'false' });
    var www = h('input', { type: 'checkbox', id: 'dm-www', checked: true });
    var app = h('select', { id: 'dm-app' }, h('option', { value: '', text: 'None yet (only add the zone and tunnel hostnames)' }));
    state.apps.filter(function (a) { return a.managed && !a.protected; }).forEach(function (a) { app.appendChild(h('option', { value: a.name, text: a.name + ' (now ' + (a.host || 'no host') + ')' })); });
    var el = h('div', { class: 'form' },
      h('div', { class: 'banner full' }, h('strong', { text: 'Public sites only. ' }), 'The wildcard certificate, the private DNS and the sign-in cookie cover example.com only, so private, tailnet-only and Google-gated hosts cannot live on a new domain. The app you pick is switched to public.'),
      field('Domain', d, 'The apex, like example.com. No https://, no www.', 'full', 'domain'),
      h('div', { class: 'check full' }, www, h('label', { for: 'dm-www', text: 'Include www (www redirects to the apex)' })),
      field('Serve this app', app, 'Its HOST becomes the new domain. Its old host stops working.', 'full', 'app'));
    return { el: el, op: function () { return { op: 'domain', domain: { domain: d.value.trim(), www: www.checked, app: app.value } }; } };
  }

  // ---------- grid ----------
  function matches(a) {
    if (state.q) { var q = state.q.toLowerCase(); if (a.name.toLowerCase().indexOf(q) < 0 && (a.host || '').toLowerCase().indexOf(q) < 0) return false; }
    if (state.filter === 'all') return true;
    if (state.filter === 'attention') return a.pill === 'Failed' || a.pill === 'Unknown' && a.managed;
    return a.mode === state.filter;
  }
  function render() {
    var apps = state.apps, grid = $('grid');
    var ready = apps.filter(function (a) { return a.pill === 'Ready'; }).length, bad = apps.filter(function (a) { return a.pill === 'Failed'; }).length;
    var pub = apps.filter(function (a) { return a.mode === 'public'; }).length;
    var st = clear($('stats'));
    [['Apps', apps.length, ''], ['Ready', ready, ''], ['Failing', bad, bad ? 'bad' : ''], ['Public', pub, '']].forEach(function (s) {
      st.appendChild(h('div', { class: 'stat ' + s[2] }, h('dt', { text: s[0] }), h('dd', { text: String(s[1]) })));
    });
    var fl = clear($('filters'));
    [['all', 'All'], ['attention', 'Needs attention']].concat(MODES.map(function (m) { return [m, m]; })).forEach(function (f) {
      fl.appendChild(h('button', { type: 'button', class: 'fchip', 'aria-pressed': String(state.filter === f[0]), text: f[1], onclick: function () { state.filter = f[0]; render(); } }));
    });
    clear(grid); grid.setAttribute('aria-busy', 'false');
    var shown = apps.filter(matches);
    shown.forEach(function (a) { grid.appendChild(card(a)); });
    $('empty').hidden = shown.length > 0;
  }
  function card(a) {
    var c = h('article', { class: 'card' + (a.pending ? ' pending' : ''), 'aria-labelledby': 'c-' + a.name });
    var title = h('h3', { id: 'c-' + a.name }, a.name, a.protected ? h('span', { class: 'tag', title: 'Protected: ' + a.locked_mode }, 'locked') : null);
    var host = a.url ? h('a', { class: 'host', href: a.url, rel: 'noopener noreferrer', target: '_blank' }, icon('ext'), a.host) : h('span', { class: 'host', text: 'no public host' });
    c.appendChild(h('div', { class: 'card-top' }, h('div', null, title, host), h('div', { class: 'badges' }, modeBadge(a.mode), pill(a.pill))));
    var facts = h('div', { class: 'facts' });
    if (a.managed) {
      facts.appendChild(h('span', null, 'Replicas ', h('b', { text: String(a.replicas) })));
      if (a.tag) facts.appendChild(h('span', null, a.pinned ? 'Pinned ' : 'Tag ', h('b', { class: 'mono', text: a.tag.length > 22 ? a.tag.slice(0, 22) + '…' : a.tag })));
      if (a.has_pvc) facts.appendChild(h('span', null, h('b', { text: 'Volume' }), ' backed up'));
      if (a.secret_count) facts.appendChild(h('span', null, h('b', { text: String(a.secret_count) }), ' secret' + (a.secret_count > 1 ? 's' : '')));
    } else facts.appendChild(h('span', { text: a.switchable ? 'Exposure only' : 'Managed outside Switchboard' }));
    if (a.last_change && ago(a.last_change)) facts.appendChild(h('span', { text: 'Changed ' + ago(a.last_change) }));
    c.appendChild(facts);
    if (a.pill === 'Failed' && a.message) c.appendChild(h('p', { class: 'err', text: a.message.slice(0, 160) }));
    var foot = h('div', { class: 'card-foot' });
    if (a.switchable) {
      var s = h('select', { 'aria-label': 'Exposure for ' + a.name, disabled: a.pending }, MODES.map(function (m) {
        return h('option', { value: m, selected: m === a.mode, disabled: (a.disabled_modes || []).indexOf(m) >= 0, text: m });
      }));
      s.onchange = function () { askMode(a, s); };
      foot.appendChild(s);
    }
    if (a.managed && !a.protected) {
      var sw = h('input', { type: 'checkbox', class: 'sw', role: 'switch', checked: a.replicas > 0, disabled: a.pending, 'aria-label': 'Running: ' + a.name });
      sw.onchange = function () { sw.checked = !sw.checked; reviewAndApply({ op: 'replicas', app: a.name, replicas: sw.checked ? 0 : 1 }, { title: (sw.checked ? 'Stop ' : 'Start ') + a.name }); };
      foot.appendChild(h('label', { class: 'switch' }, sw, h('span', { text: a.replicas > 0 ? 'On' : 'Off' })));
    }
    foot.appendChild(h('button', { type: 'button', class: 'btn small', text: 'Details', onclick: function () { openDrawer(a.name); } }));
    c.appendChild(foot);
    return c;
  }

  // ---------- mode switch (v1 behaviour) ----------
  function askMode(a, sel) {
    var to = sel.value;
    if (to === a.mode && to !== 'google-restricted') return;
    var dlg = $('confirm'), body = clear($('cf-body')), foot = clear($('cf-foot'));
    $('cf-title').textContent = 'Change exposure of ' + a.name;
    var em = h('input', { id: 'sw-em', value: a.allowed_emails || '', placeholder: 'a@example.com, b@example.com', autocomplete: 'off', spellcheck: 'false' });
    var err = h('p', { class: 'err', role: 'alert', hidden: true });
    body.appendChild(h('p', null, 'Switch ', h('b', { text: a.name }), ' from ', modeBadge(a.mode), ' to ', modeBadge(to), '? This commits to git and Flux will apply it.'));
    if (to === 'google-restricted') body.appendChild(h('div', { class: 'field' }, h('label', { for: 'sw-em', text: 'Allowed emails' }), em));
    if (to === 'public') body.appendChild(h('div', { class: 'danger-box' }, h('span', { text: a.name + ' will be reachable by anyone on the internet.' })));
    body.appendChild(err);
    var go = h('button', { type: 'button', class: 'btn primary', text: 'Commit change' });
    var cancel = function () { sel.value = a.mode; dlg.close(); };
    foot.appendChild(h('button', { type: 'button', class: 'btn', text: 'Cancel', onclick: cancel })); foot.appendChild(go);
    dlg.onclose = function () { sel.value = a.mode; dlg.onclose = null; };
    go.onclick = function () {
      go.disabled = true; err.hidden = true;
      api('POST', '/api/switch', { app: a.name, mode: to, allowed_emails: to === 'google-restricted' ? em.value.trim() : '' }).then(function (r) {
        if (!r.ok) { err.textContent = r.j.error || 'Failed'; err.hidden = false; go.disabled = false; if (r.j.reload) setTimeout(function () { dlg.close(); refresh(true); }, 2000); return; }
        dlg.onclose = null; dlg.close(); markPending(a.name); track(r.j.job, a.name + ': ' + a.mode + ' → ' + to);
      });
    };
    openDlg(dlg); (to === 'google-restricted' ? em : go).focus();
  }

  // ---------- drawer ----------
  var TABS = [['overview', 'Overview'], ['image', 'Image'], ['pods', 'Pods & logs'], ['secrets', 'Secrets'], ['danger', 'Delete']];
  function openDrawer(name) { state.detail = { name: name, loading: true }; state.tab = 'overview'; renderDrawer(); openDlg($('drawer')); loadDetail(name); }
  function loadDetail(name, quiet) {
    api('GET', '/api/apps/' + encodeURIComponent(name)).then(function (r) {
      if (!state.detail || state.detail.name !== name) return;
      if (!r.ok) { if (!quiet) toast('Could not load ' + name, true); return; }
      state.detail = r.j; renderDrawer();
    });
  }
  $('drawer').addEventListener('close', function () { state.detail = null; });
  function renderDrawer() {
    var d = state.detail; if (!d) return;
    $('dr-title').textContent = d.name;
    var meta = clear($('dr-meta'));
    if (!d.loading) { meta.appendChild(modeBadge(d.mode)); meta.appendChild(pill(d.pill)); if (d.url) meta.appendChild(h('a', { class: 'host', href: d.url, rel: 'noopener noreferrer', target: '_blank' }, icon('ext'), d.host)); }
    var tabs = clear($('dr-tabs'));
    TABS.forEach(function (t) {
      if (d.loading) return;
      if (t[0] === 'image' && !(d.built)) return;
      if (t[0] === 'danger' && !(d.managed && !d.protected)) return;
      tabs.appendChild(h('button', { type: 'button', class: 'tab', role: 'tab', 'aria-selected': String(state.tab === t[0]), text: t[1], onclick: function () { state.tab = t[0]; renderDrawer(); } }));
    });
    var body = clear($('dr-body'));
    if (d.loading) { body.appendChild(h('div', { class: 'skeleton' })); return; }
    ({ overview: tabOverview, image: tabImage, pods: tabPods, secrets: tabSecrets, danger: tabDanger }[state.tab] || tabOverview)(d, body);
  }
  function tabOverview(d, body) {
    var s1 = h('section', { class: 'sect' }, h('h3', { text: 'Exposure' }));
    s1.appendChild(h('p', { class: 'muted', text: d.protected ? 'Protected app: ' + d.locked_mode + '.' : 'Switching commits to clusters/keep/apps/' + d.name + '.yaml.' }));
    if (d.switchable) {
      var emails = h('input', { id: 'dr-em', value: d.allowed_emails || '', placeholder: 'a@example.com, b@example.com', autocomplete: 'off' });
      var seg = h('div', { class: 'seg', role: 'radiogroup', 'aria-label': 'Exposure' }), picked = d.mode;
      MODES.forEach(function (m) {
        var i = h('input', { type: 'radio', name: 'dr-mode', value: m, checked: m === d.mode, disabled: (d.disabled_modes || []).indexOf(m) >= 0 });
        i.onchange = function () { picked = m; ef.hidden = m !== 'google-restricted'; apply.disabled = m === d.mode && m !== 'google-restricted'; };
        seg.appendChild(h('label', { class: 'segopt' }, i, h('span', null, h('b', { text: m }), h('small', { text: MODE_HELP[m] }))));
      });
      var ef = h('div', { class: 'field', hidden: d.mode !== 'google-restricted' }, h('label', { for: 'dr-em', text: 'Allowed emails' }), emails);
      var apply = h('button', { type: 'button', class: 'btn primary', text: 'Review change', disabled: true });
      apply.onclick = function () {
        var fake = h('select'); fake.appendChild(h('option', { value: picked })); fake.value = picked;
        $('drawer').close(); askModeWith(d, picked, emails.value.trim());
      };
      emails.oninput = function () { apply.disabled = false; };
      s1.appendChild(seg); s1.appendChild(ef); s1.appendChild(h('div', { class: 'row' }, apply));
    } else s1.appendChild(h('p', { text: 'This app has no expose-* component, so it cannot be switched here.' }));
    body.appendChild(s1);
    if (d.managed && !d.protected) {
      var run = h('section', { class: 'sect' }, h('h3', { text: 'Replicas' }), h('p', { class: 'muted', text: 'Replicas are 0 or 1. Stopping keeps the volume and its data.' }));
      var b = h('button', { type: 'button', class: 'btn', text: d.replicas > 0 ? 'Stop (replicas → 0)' : 'Start (replicas → 1)' });
      b.onclick = function () { $('drawer').close(); reviewAndApply({ op: 'replicas', app: d.name, replicas: d.replicas > 0 ? 0 : 1 }, { title: (d.replicas > 0 ? 'Stop ' : 'Start ') + d.name }); };
      run.appendChild(h('div', { class: 'row' }, h('span', null, 'Now ', h('b', { text: String(d.replicas) })), b));
      body.appendChild(run);
    }
    if (d.managed && !d.protected) {
      var dbs = h('section', { class: 'sect' }, h('h3', { text: 'Database' }));
      dbs.appendChild(h('p', { class: 'muted', text: d.db_role ? 'Has its own database on the shared Postgres.' : d.db_client ? 'Can reach the shared Postgres (no database of its own).' : 'No database.' }));
      if (!d.db_role) {
        var addb = h('button', { type: 'button', class: 'btn', text: 'Add database…' });
        addb.onclick = function () { $('drawer').close(); reviewAndApply({ op: 'add-database', app: d.name }, { title: 'Add a database for ' + d.name }); };
        dbs.appendChild(h('div', { class: 'row' }, addb));
      }
      body.appendChild(dbs);
    }
    var info = h('section', { class: 'sect' }, h('h3', { text: 'Flux' }));
    info.appendChild(h('p', null, 'Status ', pill(d.pill), ' ', h('span', { class: 'muted', text: d.last_change && ago(d.last_change) ? 'since ' + ago(d.last_change) : '' })));
    if (d.message) info.appendChild(h('p', { class: 'muted mono', text: d.message.slice(0, 400) }));
    if (d.revision) info.appendChild(h('p', { class: 'muted' }, 'Revision ', h('code', { text: d.revision })));
    if (d.depends_on && d.depends_on.length) info.appendChild(h('p', { class: 'muted', text: 'Depends on: ' + d.depends_on.join(', ') }));
    if (d.components && d.components.length) info.appendChild(h('p', { class: 'muted', text: 'Components: ' + d.components.map(function (c) { return c.split('/').pop(); }).join(', ') }));
    body.appendChild(info);
    if (d.files && d.files.length) body.appendChild(h('section', { class: 'sect' }, h('h3', { text: 'Files in the repo' }), h('ul', { class: 'flist' }, d.files.map(function (f) { return h('li', null, h('span', { class: 'fbtn' }, icon('file'), f)); }))));
  }
  function askModeWith(d, to, emails) {
    var a = state.apps.filter(function (x) { return x.name === d.name; })[0] || d;
    var sel = h('select'); sel.appendChild(h('option', { value: to })); sel.value = to;
    askMode(Object.assign({}, a, { allowed_emails: emails }), sel);
  }
  function tabImage(d, body) {
    var t = d.tags, sec = h('section', { class: 'sect' }, h('h3', { text: 'Image' }));
    sec.appendChild(h('p', null, h('code', { text: d.image })));
    sec.appendChild(h('p', null, 'Auto-update ', h('span', { class: 'badge ' + (d.pinned ? 'm-public' : 'm-private'), text: d.pinned ? 'pinned' : 'Flux tracks newest' })));
    if (d.pinned) {
      var r = h('button', { type: 'button', class: 'btn', text: 'Resume auto-update' });
      r.onclick = function () { $('drawer').close(); reviewAndApply({ op: 'resume', app: d.name }, { title: 'Resume auto-update for ' + d.name }); };
      sec.appendChild(h('div', { class: 'row' }, h('span', { class: 'muted', text: 'Rolled back on purpose; the Flux marker is off.' }), r));
    }
    body.appendChild(sec);
    var tt = h('section', { class: 'sect' }, h('h3', { text: 'Recent tags' }));
    if (!t) tt.appendChild(h('p', { class: 'muted', text: d.tags_error || 'No scan data (cluster not connected).' }));
    else {
      tt.appendChild(h('p', { class: 'muted', text: 'Flux scanned ' + t.tag_count + ' tags' + (t.last_scan && ago(t.last_scan) ? ', ' + ago(t.last_scan) : '') + '. Newest policy match: ' + (t.policy_tag || 'n/a') + '. Showing the latest ' + (t.tags || []).length + '.' }));
      var tb = h('tbody');
      (t.tags || []).forEach(function (tag) {
        var cur = tag === d.tag, b = h('button', { type: 'button', class: 'btn small', text: cur ? 'Current' : 'Deploy this tag', disabled: cur });
        b.onclick = function () { $('drawer').close(); reviewAndApply({ op: 'image', app: d.name, tag: tag }, { title: 'Roll ' + d.name + ' to ' + tag.slice(0, 14) }); };
        tb.appendChild(h('tr', null, h('td', null, h('code', { text: tag })), h('td', { text: tag === t.policy_tag ? 'newest' : '' }), h('td', null, b)));
      });
      tt.appendChild(h('table', { class: 't' }, h('thead', null, h('tr', null, h('th', { text: 'Tag' }), h('th', { text: '' }), h('th', { text: '' }))), tb));
      tt.appendChild(h('p', { class: 'hint', text: 'Rolling back removes the Flux image marker so automation does not bump the tag straight back. Resume auto-update hands control back to Flux.' }));
    }
    body.appendChild(tt);
  }
  function tabPods(d, body) {
    var sec = h('section', { class: 'sect' }, h('h3', { text: 'Pods' }));
    if (d.pods_error) sec.appendChild(h('p', { class: 'err', text: d.pods_error }));
    if (!d.pods.length) sec.appendChild(h('p', { class: 'muted', text: state.me.cluster ? 'No pods (replicas 0, or not scheduled yet).' : 'Cluster not connected.' }));
    else {
      var tb = h('tbody'), logs = h('pre', { class: 'logbox', tabindex: '0', 'aria-label': 'Log lines', hidden: true });
      d.pods.forEach(function (p) {
        var lb = h('button', { type: 'button', class: 'btn small', text: 'Last lines' });
        lb.onclick = function () {
          logs.hidden = false; logs.textContent = 'Loading…';
          api('GET', '/api/apps/' + encodeURIComponent(d.name) + '/logs?tail=80&pod=' + encodeURIComponent(p.name)).then(function (r) {
            logs.textContent = r.ok ? r.j.lines.join('\n') || '(no output)' : (r.j.error || 'Could not read logs'); logs.scrollTop = logs.scrollHeight;
          });
        };
        tb.appendChild(h('tr', null, h('td', null, h('code', { text: p.name }), p.reason ? h('div', { class: 'err', text: p.reason }) : null), h('td', { text: p.phase }), h('td', { text: p.ready }), h('td', { text: String(p.restarts) }), h('td', { text: ago(p.started) }), h('td', null, lb)));
      });
      sec.appendChild(h('table', { class: 't' }, h('thead', null, h('tr', null, ['Pod', 'Phase', 'Ready', 'Restarts', 'Started', ''].map(function (x) { return h('th', { text: x }); }))), tb));
      sec.appendChild(logs);
      sec.appendChild(h('p', { class: 'hint', text: 'Read-only. Obvious credentials in log lines are masked, but logs can still hold personal data.' }));
    }
    body.appendChild(sec);
  }
  function tabSecrets(d, body) {
    var sec = h('section', { class: 'sect' }, h('h3', { text: 'Secrets from the vault' }));
    if (!d.secrets.length) sec.appendChild(h('p', { class: 'muted', text: 'This app has no ExternalSecret.' }));
    d.secrets.forEach(function (s) {
      sec.appendChild(h('div', { class: 'row' }, h('b', { text: s.name }), pill(s.status.ready === 'True' ? 'Ready' : s.status.ready === 'False' ? 'Failed' : 'Unknown'), h('span', { class: 'muted', text: s.status.reason || '' })));
      if (s.status.ready === 'False' && s.status.message) sec.appendChild(h('p', { class: 'err', text: s.status.message.slice(0, 220) }));
      sec.appendChild(h('ul', { class: 'flist' }, s.keys.map(function (k) { return h('li', null, h('span', { class: 'fbtn' }, icon('lock'), h('code', { text: k }))); })));
    });
    sec.appendChild(h('p', { class: 'hint', text: 'Names only. Values are never shown or entered here: use make secret in the citadel repo, then Flux and External Secrets pick them up.' }));
    body.appendChild(sec);
  }
  function tabDanger(d, body) {
    var sec = h('section', { class: 'sect danger-zone' }, h('h3', { text: 'Delete ' + d.name }));
    sec.appendChild(h('p', { text: 'Removes the app directory, its Flux Kustomization, the registration, image automation and the homepage entry. Flux then prunes it from the cluster.' }));
    if (d.has_pvc) sec.appendChild(h('p', { class: 'err', text: 'This app has a volume: deleting removes the volume and its data.' }));
    var b = h('button', { type: 'button', class: 'btn danger', text: 'Review deletion…' });
    b.onclick = function () { $('drawer').close(); reviewAndApply({ op: 'delete', app: d.name }, { title: 'Delete ' + d.name, danger: true, typeName: true, confirm: 'Delete ' + d.name }); };
    sec.appendChild(h('div', { class: 'row' }, b));
    body.appendChild(sec);
  }


  // ---------- secrets (app secrets in the OCI Vault; write-only unless Reveal) ----------
  var sc = { data: null };
  function loadSecrets() {
    return api('GET', '/api/secrets').then(function (r) {
      if (!r.ok) { clear($('sc-body')).appendChild(h('div', { class: 'err', role: 'alert', text: r.j.error || 'Could not load secrets' })); return; }
      sc.data = r.j; renderSecrets();
    });
  }
  function openSecrets() { openDlg($('secrets')); clear($('sc-body')).appendChild(h('div', { class: 'skeleton' })); loadSecrets(); }
  function expiryPill(row) {
    if (row.expires_in == null) return h('span', { class: 'muted', text: row.expires ? row.expires : 'never' });
    if (row.expires_in < 0) return h('span', { class: 'pill pill-Expired', title: 'expired ' + row.expires, text: 'expired ' + row.expires.slice(5) });
    if (row.expires_in <= 14) return h('span', { class: 'pill pill-Expiring', text: 'in ' + row.expires_in + ' d' });
    return h('span', { text: row.expires });
  }
  function renderSecrets() {
    var d = sc.data, body = clear($('sc-body')), foot = clear($('sc-foot'));
    if (!d.enabled) { body.appendChild(h('p', { class: 'muted', text: 'The Secrets screen is not configured on this Switchboard (needs the vault settings, see the README).' })); return; }
    var apps = {};
    d.rows.forEach(function (r) { (apps[r.app] = apps[r.app] || []).push(r); });
    Object.keys(apps).sort().forEach(function (app) {
      body.appendChild(h('h3', { class: 'group-title', text: app }));
      var box = h('div', { class: 'scs' });
      apps[app].forEach(function (r) {
        var status = r.declared.length ? pill(r.ready === 'True' ? 'Ready' : r.ready === 'False' ? 'Failed' : 'Unknown') : h('span', { class: 'pill pill-Orphan', text: 'not used' });
        var acts = h('div', { class: 'acts' },
          h('button', { type: 'button', class: 'btn small', text: 'Reveal', onclick: function () { revealDialog(r.name); } }),
          h('button', { type: 'button', class: 'btn small', text: 'New version', onclick: function () { secretForm({ mode: 'update', name: r.name }); } }),
          r.declared.length ? null : h('button', { type: 'button', class: 'btn small', text: 'Declare', title: 'Add to the app ExternalSecret (a citadel commit)', onclick: function () { declare(r.app, r.name); } }));
        box.appendChild(h('div', { class: 'sc' },
          h('div', { class: 'nm' }, h('b', { text: r.key }), h('small', { class: 'mono', text: r.name })),
          h('div', { class: 'cell' }, h('small', { text: 'Version' }), 'v' + r.version),
          h('div', { class: 'cell' }, h('small', { text: 'Rotated' }), r.rotated || (r.created ? r.created.slice(0, 10) : '')),
          h('div', { class: 'cell' }, h('small', { text: 'Expires' }), expiryPill(r)),
          h('div', { class: 'cell' }, h('small', { text: 'Used by app' }), status), acts));
      });
      body.appendChild(box);
    });
    if (!d.rows.length) body.appendChild(h('p', { class: 'muted', text: 'No app secrets in the vault yet.' }));
    if (d.missing.length) {
      body.appendChild(h('h3', { class: 'group-title', text: 'Expected by an app, but not in the vault' }));
      var mb = h('div', { class: 'scs' });
      d.missing.forEach(function (m) {
        mb.appendChild(h('div', { class: 'sc' }, h('div', { class: 'nm' }, h('b', { text: m.Key }), h('small', { class: 'mono', text: m.Name })),
          h('div', { class: 'cell' }, h('small', { text: 'ExternalSecret' }), m.ES), h('span'), h('span'),
          h('div', { class: 'acts' }, h('button', { type: 'button', class: 'btn small primary', text: 'Create value', onclick: function () { secretForm({ mode: 'create', app: m.App, key: m.Key }); } }))));
      });
      body.appendChild(mb);
    }
    foot.appendChild(h('span', { class: 'grow muted', text: 'Only app-<app>--<key> secrets of your apps appear here. Infra secrets stay on make secret.' }));
    foot.appendChild(h('button', { type: 'button', class: 'btn primary', text: 'New secret', onclick: function () { secretForm({ mode: 'create' }); } }));
  }
  function declare(app, name) { reviewAndApply({ op: 'secret-declare', app: app, secret_name: name }, { title: 'Declare ' + name }); }

  function secretForm(o) {
    var dlg = $('confirm'), body = clear($('cf-body')), foot = clear($('cf-foot')), create = o.mode === 'create';
    $('cf-title').textContent = create ? 'New secret' : 'New version of ' + o.name;
    var err = h('p', { class: 'err', role: 'alert', hidden: true });
    var app = h('select', { id: 'sf-app' }, (sc.data.apps || []).map(function (a) { return h('option', { value: a, text: a, selected: a === o.app }); }));
    var key = h('input', { id: 'sf-key', autocomplete: 'off', spellcheck: 'false', placeholder: 'api-key or API_KEY', value: o.key || '' });
    var val = h('textarea', { id: 'sf-val', class: 'secretbox', autocomplete: 'off', spellcheck: 'false', 'aria-describedby': 'sf-hint' });
    var show = h('input', { type: 'checkbox', id: 'sf-show' }); show.onchange = function () { val.classList.toggle('shown', show.checked); };
    var exp = h('input', { id: 'sf-exp', type: 'date' });
    var decl = h('input', { type: 'checkbox', id: 'sf-decl', checked: true });
    if (create) body.appendChild(h('div', { class: 'form' }, field('App', app, null, '', 'app'), field('Key name', key, 'Becomes app-<app>--<key>; env var name is the upper-case form.', '', 'key')));
    else body.appendChild(h('p', null, 'Adds a new version of ', h('code', { text: o.name }), '. The old version stays in the vault.'));
    body.appendChild(h('div', { class: 'form' }, h('div', { class: 'field full' }, h('label', { for: 'sf-val', text: 'Value' }), val,
      h('span', { class: 'hint', id: 'sf-hint', text: 'Masked while typing. Never stored in git, never logged, not shown again unless you click Reveal.' }),
      h('div', { class: 'check' }, show, h('label', { for: 'sf-show', text: 'Show while typing' }))),
      field('Expires (optional)', exp, 'A reminder tag; the vault does not enforce it.', '', 'expires')));
    if (create) body.appendChild(h('div', { class: 'check' }, decl, h('label', { for: 'sf-decl', text: "Also add it to the app's ExternalSecret (a citadel commit you review next)" })));
    body.appendChild(err);
    var go = h('button', { type: 'button', class: 'btn primary', text: create ? 'Create secret' : 'Save new version' });
    foot.appendChild(h('button', { type: 'button', class: 'btn', text: 'Cancel', onclick: function () { val.value = ''; dlg.close(); } })); foot.appendChild(go);
    go.onclick = function () {
      go.disabled = true; err.hidden = true;
      var req = create ? { app: app.value, key: key.value.trim(), value: val.value, expires: exp.value } : { name: o.name, value: val.value, expires: exp.value };
      api('POST', '/api/secrets/' + (create ? 'create' : 'update'), req).then(function (r) {
        val.value = ''; req.value = '';
        if (!r.ok) { err.textContent = r.j.error || 'Failed'; err.hidden = false; go.disabled = false; return; }
        dlg.close(); toast((create ? 'Created ' : 'New version v' + r.j.version + ' of ') + r.j.name);
        if (r.j.note) toast(r.j.note);
        loadSecrets();
        if (create && decl.checked && !r.j.declared) setTimeout(function () { declare(app.value, r.j.name); }, 300);
      });
    };
    openDlg(dlg); (create && !o.app ? app : val).focus();
  }

  function revealDialog(name) {
    var dlg = $('confirm'), body = clear($('cf-body')), foot = clear($('cf-foot')), timer = null;
    $('cf-title').textContent = 'Reveal ' + name.split('--')[1];
    var out = h('div'), go = h('button', { type: 'button', class: 'btn primary', text: 'Reveal for 30 s' });
    body.appendChild(h('p', null, 'Shows the current value of ', h('code', { text: name }), ' on this screen for 30 seconds. The reveal is logged with your email and the version, never the value.'));
    body.appendChild(out);
    function wipe() { if (timer) clearInterval(timer); clear(out); }
    dlg.onclose = function () { wipe(); dlg.onclose = null; };
    foot.appendChild(h('button', { type: 'button', class: 'btn', text: 'Close', onclick: function () { dlg.close(); } })); foot.appendChild(go);
    go.onclick = function () {
      go.disabled = true;
      api('POST', '/api/secrets/reveal', { name: name }).then(function (r) {
        if (!r.ok) { clear(out).appendChild(h('p', { class: 'err', role: 'alert', text: r.j.error || 'Failed' })); go.disabled = false; return; }
        var left = r.j.seconds || 30, box = h('pre', { class: 'revealed', tabindex: '0', text: r.j.value }), cnt = h('span', { class: 'muted' });
        var copy = h('button', { type: 'button', class: 'btn small', text: 'Copy', onclick: function () { navigator.clipboard && navigator.clipboard.writeText(r.j.value).then(function () { toast('Copied. Clear your clipboard afterwards.'); }); } });
        clear(out).appendChild(h('div', null, h('p', { class: 'muted', text: 'Version ' + r.j.version + (r.j.encoding === 'base64' ? ' (binary, shown as base64)' : '') }), box, h('div', { class: 'row' }, copy, cnt)));
        timer = setInterval(function () { left--; cnt.textContent = 'hides in ' + left + ' s'; if (left <= 0) { wipe(); go.disabled = false; go.textContent = 'Reveal again'; } }, 1000);
        cnt.textContent = 'hides in ' + left + ' s'; r.j.value = '';
      });
    };
    openDlg(dlg); go.focus();
  }


  // ---------- network (app-to-app access) ----------
  var nw = { data: null };
  function loadNetwork() {
    return api('GET', '/api/network?fresh=1').then(function (r) {
      if (!r.ok) { clear($('nw-body')).appendChild(h('div', { class: 'err', role: 'alert', text: r.j.error || 'Could not load the network' })); return; }
      nw.data = r.j; renderNetwork();
    });
  }
  function openNetwork() { openDlg($('network')); clear($('nw-body')).appendChild(h('div', { class: 'skeleton' })); loadNetwork(); }
  function canLink(a, b) {
    if (a.name === b.name || !a.editable || !b.editable) return false;
    if (a.namespace !== b.namespace && (a.isolated || b.isolated || a.egress_restricted)) return false;
    return true;
  }
  function whyNot(a, b) {
    if (a.name === b.name) return '';
    if (a.protected || b.protected) return 'Protected app: not changed from here';
    if (!a.editable || !b.editable) return 'Not a standard app';
    if (a.namespace !== b.namespace && (a.isolated || b.isolated)) return 'Namespace kept isolated';
    if (a.egress_restricted && a.namespace !== b.namespace) return 'Caller restricts egress (not supported yet)';
    return '';
  }
  function netOp(a, b, allow) {
    $('network').close();
    reviewAndApply({ op: allow ? 'net-allow' : 'net-revoke', src: a.name, app: b.name }, { title: (allow ? 'Allow ' : 'Revoke ') + a.name + ' → ' + b.name });
  }
  function renderNetwork() {
    var d = nw.data, body = clear($('nw-body')), apps = d.apps, edge = {}, hasDB = false;
    d.edges.forEach(function (e) { edge[e.src + '>' + e.dst] = e; if (e.dst === 'db') hasDB = true; });
    body.appendChild(h('div', { class: 'legend' },
      h('span', null, h('span', { class: 'ncell' }, '+'), ' click to allow'), h('span', null, h('span', { class: 'ncell custom' }, '●'), ' hand-written rule (read only)'),
      h('span', null, h('span', { class: 'ncell' + ' ' }, '–'), ' not possible'), h('span', { text: 'Rows call, columns are called. Port = container port.' }),
      d.isolated_namespaces && d.isolated_namespaces.length ? h('span', { class: 'tag', text: 'isolated: ' + d.isolated_namespaces.join(', ') }) : null));
    // matrix
    var thead = h('tr', null, h('th', { scope: 'col' }, 'caller ↓ / called →'));
    apps.forEach(function (b) { thead.appendChild(h('th', { scope: 'col' }, b.name, h('small', { text: ':' + b.port + ' · ' + b.namespace }))); });
    if (hasDB) thead.appendChild(h('th', { scope: 'col' }, 'db', h('small', { text: ':5432 · configs' })));
    var tbody = h('tbody');
    apps.forEach(function (a) {
      var tr = h('tr', null, h('th', { scope: 'row' }, a.name, h('small', { text: a.namespace + (a.protected ? ' · protected' : '') })));
      apps.forEach(function (b) {
        var e = edge[a.name + '>' + b.name], td = h('td');
        if (a.name === b.name) td.appendChild(h('span', { class: 'ncell self', 'aria-hidden': 'true' }, '·'));
        else if (e && e.kind === 'managed') td.appendChild(h('button', { type: 'button', class: 'ncell allowed', title: 'Allowed. Click to revoke', 'aria-label': a.name + ' may call ' + b.name + ' on port ' + e.port + '. Revoke', onclick: function () { netOp(a, b, false); } }, '✓'));
        else if (e) td.appendChild(h('span', { class: 'ncell custom', title: 'Allowed by a hand-written policy (' + (e.file || '') + ')', role: 'img', 'aria-label': a.name + ' may call ' + b.name + ' (hand-written rule)' }, '●'));
        else if (canLink(a, b)) td.appendChild(h('button', { type: 'button', class: 'ncell none', title: 'Allow ' + a.name + ' to call ' + b.name, 'aria-label': 'Allow ' + a.name + ' to call ' + b.name + ' on port ' + b.port, onclick: function () { netOp(a, b, true); } }, '+'));
        else td.appendChild(h('span', { class: 'ncell', title: whyNot(a, b), role: 'img', 'aria-label': 'Not possible: ' + whyNot(a, b) }, '–'));
        tr.appendChild(td);
      });
      if (hasDB) tr.appendChild(h('td', null, edge[a.name + '>db'] ? h('span', { class: 'ncell db', title: 'db-client component (Postgres)' }, 'db') : h('span', { class: 'ncell' }, '–')));
      tbody.appendChild(tr);
    });
    body.appendChild(h('div', { class: 'netwrap' }, h('table', { class: 'net' }, h('thead', null, thead), tbody)));
    // phone: cards
    var cards = h('div', { class: 'netcards' });
    apps.forEach(function (a) {
      var out = h('div', { class: 'chips' }), inn = h('div', { class: 'chips' });
      d.edges.forEach(function (e) {
        if (e.src === a.name) out.appendChild(chip(e, e.dst, a, apps.filter(function (x) { return x.name === e.dst; })[0]));
        if (e.dst === a.name) inn.appendChild(chip(e, e.src, apps.filter(function (x) { return x.name === e.src; })[0], a));
      });
      if (!out.children.length) out.appendChild(h('span', { class: 'muted', text: 'nobody' })); if (!inn.children.length) inn.appendChild(h('span', { class: 'muted', text: 'nobody' }));
      var targets = apps.filter(function (b) { return canLink(a, b) && !edge[a.name + '>' + b.name]; });
      var sel = h('select', { 'aria-label': 'Allow ' + a.name + ' to call', disabled: !targets.length }, [h('option', { value: '', text: targets.length ? 'Allow ' + a.name + ' to call…' : 'No more targets' })].concat(targets.map(function (b) { return h('option', { value: b.name, text: b.name + ' :' + b.port }); })));
      sel.onchange = function () { var b = apps.filter(function (x) { return x.name === sel.value; })[0]; if (b) netOp(a, b, true); };
      cards.appendChild(h('div', { class: 'ncard' }, h('h3', null, a.name, h('span', { class: 'tag', text: a.namespace }), a.protected ? h('span', { class: 'tag', text: 'protected' }) : null),
        h('div', null, h('small', { class: 'muted', text: 'Can call' }), out), h('div', null, h('small', { class: 'muted', text: 'Can be called by' }), inn), sel));
    });
    body.appendChild(cards);
    clear($('nw-foot')).appendChild(h('span', { class: 'grow muted', text: 'Verify a change with a throwaway pod after Flux applied it: the call should fail before and work after.' }));
  }
  function chip(e, label, a, b) {
    var c = h('span', { class: 'chip2' + (e.kind === 'managed' ? '' : ' ro') }, label + ' :' + e.port + (e.kind === 'db' ? ' (db)' : e.kind === 'custom' ? ' (hand-written)' : ''));
    if (e.kind === 'managed' && a && b) c.appendChild(h('button', { type: 'button', 'aria-label': 'Revoke ' + a.name + ' to ' + b.name, onclick: function () { netOp(a, b, false); } }, '×'));
    return c;
  }

  // ---------- data ----------
  function refresh(fresh) {
    return api('GET', '/api/apps' + (fresh ? '?fresh=1' : '')).then(function (r) {
      var b = $('banner');
      if (!r.ok) { b.textContent = r.j.error || 'Could not load apps.'; b.className = 'banner err'; b.hidden = false; $('grid').setAttribute('aria-busy', 'false'); return; }
      b.hidden = true; state.apps = r.j; render();
    }).catch(function () {});
  }
  $('q').addEventListener('input', function (e) { state.q = e.target.value.trim(); render(); });
  $('btn-new').onclick = function () { flow({ dialog: 'wizard', body: 'wz-body', foot: 'wz-foot', steps: 'wz-steps', build: newAppForm, commitLabel: 'Commit new app', title: 'New app' }); };
  $('btn-domain').onclick = function () { flow({ dialog: 'domain', body: 'dm-body', foot: 'dm-foot', steps: 'dm-steps', build: domainForm, commitLabel: 'Commit domain', title: 'Add domain' }); };
  $('btn-secrets').onclick = openSecrets;
  $('btn-network').onclick = openNetwork;
  ['wizard', 'domain', 'confirm', 'drawer', 'secrets', 'network'].forEach(function (id) {
    var d = $(id); closeBtns(d);
    d.addEventListener('click', function (e) { if (e.target === d) d.close(); });
  });
  ['wz-form', 'dm-form', 'cf-form'].forEach(function (id) { $(id).addEventListener('submit', function (e) { e.preventDefault(); }); });

  api('GET', '/api/me').then(function (r) {
    state.me = r.j || {};
    $('who').textContent = state.me.email || '';
    if (state.me.repo) { var c = $('repo-chip'); c.textContent = state.me.repo + '@' + state.me.branch; c.hidden = false; }
    $('demo-chip').hidden = !state.me.demo;
    $('btn-secrets').hidden = !state.me.secrets;
  }).then(function () { return refresh(false); }).then(route);
  // Deep links: #network, #secrets, #domain, #new, #app:<name>
  function route() {
    var h = decodeURIComponent(location.hash.slice(1));
    if (h === 'network') openNetwork(); else if (h === 'secrets' && state.me.secrets) openSecrets();
    else if (h === 'domain') $('btn-domain').click(); else if (h === 'new') $('btn-new').click();
    else if (h.indexOf('app:') === 0) openDrawer(h.slice(4));
  }
  window.addEventListener('hashchange', route);
  setInterval(function () { if (!document.hidden) refresh(false); }, 10000);
})();
