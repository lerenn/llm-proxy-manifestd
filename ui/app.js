'use strict';
(function () {
  var POLL_MS = 2000;
  var $ = function (id) { return document.getElementById(id); };

  // Build DOM with textContent only: API data is never parsed as HTML.
  function td(text, cls) {
    var c = document.createElement('td');
    c.textContent = text;
    if (cls) c.className = cls;
    return c;
  }
  function fill(tbody, rows, cols) {
    tbody.replaceChildren();
    if (!rows.length) {
      var tr = document.createElement('tr');
      var c = td('none configured', 'empty');
      c.colSpan = cols;
      tr.appendChild(c);
      tbody.appendChild(tr);
      return;
    }
    rows.forEach(function (cells) {
      var tr = document.createElement('tr');
      cells.forEach(function (c) { tr.appendChild(td(c[0], c[1])); });
      tbody.appendChild(tr);
    });
  }
  function dur(s) {
    var d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
    if (d) return d + 'd ' + h + 'h';
    if (h) return h + 'h ' + m + 'm';
    if (m) return m + 'm ' + (s % 60) + 's';
    return s + 's';
  }
  function ago(iso, now) {
    var s = Math.max(0, Math.round((now - new Date(iso)) / 1000));
    return dur(s) + ' ago';
  }
  function n(v) { return Number(v).toLocaleString(); }

  function setStatus(text, cls) {
    var el = $('status');
    el.textContent = text;
    el.className = 'badge ' + cls;
  }

  function row(dl, k, v, sub) {
    var d = document.createElement('div');
    var dt = document.createElement('dt'); dt.textContent = k;
    var dd = document.createElement('dd'); dd.textContent = v;
    if (sub) { var sp = document.createElement('span'); sp.className = 'sub'; sp.textContent = sub; dd.appendChild(sp); }
    d.appendChild(dt); d.appendChild(dd); dl.appendChild(d);
  }

  function render(s) {
    var now = new Date(s.now);
    $('error').hidden = true;
    setStatus(s.status === 'ok' ? 'Healthy' : 'Degraded: last config reload failed', s.status === 'ok' ? 'ok' : 'warn');
    var t = s.totals;
    var pools = s.pools || [], eps = s.endpoints || [], clients = s.clients || [];
    var groups = {};
    pools.forEach(function (p) { if (p.concurrency_group) groups[p.concurrency_group] = true; });
    var r = 'applied ' + ago(s.config.loaded_at, now);
    if (s.config.last_reload_attempt) {
      r += '; last check ' + ago(s.config.last_reload_attempt, now);
      if (s.config.last_reload_failed) r += ' (rejected, previous config kept)';
    }
    var dl = $('overview');
    dl.replaceChildren();
    row(dl, 'Version', s.version);
    row(dl, 'Uptime', dur(s.uptime_seconds));
    row(dl, 'Pools', n(pools.length));
    row(dl, 'Endpoints', n(eps.length));
    row(dl, 'Concurrency groups', n(Object.keys(groups).length));
    row(dl, 'Requests in flight', n(t.in_flight));
    row(dl, 'Queued requests', n(t.queued));
    row(dl, 'Slots in use / total', t.active_slots + ' / ' + (t.unlimited ? 'unlimited' : t.slots),
      t.unlimited ? 'at least one pool has no slot limit' : '');
    row(dl, 'Requests / errors', n(t.requests) + ' / ' + n(t.errors));
    row(dl, 'Clients', s.auth_enabled ? n(clients.length) + ': ' + (clients.map(function (c) { return c.name + ' (' + n(c.requests) + ')'; }).join(', ') || '–') : 'auth disabled');
    row(dl, 'Config generation', String(s.config.generation), r);

    fill($('pools'), pools.map(function (p) {
      var q = (p.queue_by_priority || []).map(function (x) { return 'p' + x.priority + ': ' + x.count; }).join(', ') || '–';
      var grp = (p.concurrency_group || '–') + (p.coordinated ? ' (coordinated)' : '');
      return [[p.name], [p.protocol || '–'], [p.upstream], [grp], [p.active_slots + ' / ' + (p.unlimited ? 'unlimited' : p.max_concurrency), 'num'],
        [n(p.queued), 'num'], [q], [n(p.total_served), 'num']];
    }), 8);
    fill($('endpoints'), eps.map(function (x) {
      var tg = x.targets.map(function (t) { return t.pool + (t.target_model ? ' → ' + t.target_model : ''); }).join(', ');
      var ups = x.targets.map(function (t) { return t.upstream; }).join(', ');
      return [[x.name], [x.protocol], [x.mode], [String(x.priority), 'num'], [tg], [ups], [n(x.in_flight), 'num'],
        [n(x.queued), 'num'], [n(x.requests), 'num'], [n(x.errors), 'num' + (x.errors ? ' err' : '')]];
    }), 10);
    $('updated').textContent = 'updated ' + now.toLocaleTimeString();
  }

  var VIEWS = ['overview', 'pools', 'endpoints'];
  function route() {
    var v = (location.hash.replace(/^#\/?/, '') || 'overview');
    if (VIEWS.indexOf(v) < 0) v = 'overview';
    VIEWS.forEach(function (name) {
      $('view-' + name).hidden = name !== v;
      var a = document.querySelector('.sidebar a[data-view="' + name + '"]');
      if (name === v) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
    document.title = v.charAt(0).toUpperCase() + v.slice(1) + ' · llm-proxy-manifestd dashboard';
    closeNav();
  }
  function closeNav() {
    document.body.classList.remove('nav-open');
    $('nav-toggle').setAttribute('aria-expanded', 'false');
  }
  $('nav-toggle').addEventListener('click', function () {
    var open = document.body.classList.toggle('nav-open');
    this.setAttribute('aria-expanded', open ? 'true' : 'false');
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') closeNav(); });
  window.addEventListener('hashchange', route);
  route();

  function fail(msg) {
    setStatus('Unreachable', 'bad');
    var e = $('error');
    e.textContent = msg + ' Showing last known data; retrying…';
    e.hidden = false;
  }

  function tick() {
    var ctl = new AbortController();
    var to = setTimeout(function () { ctl.abort(); }, 5000);
    // Relative URL: works at "/" on the dashboard host and under the path prefix.
    fetch('api/state', { cache: 'no-store', signal: ctl.signal })
      .then(function (r) {
        if (!r.ok) throw new Error('API returned HTTP ' + r.status + '.');
        return r.json();
      })
      .then(render)
      .catch(function (e) { fail(e.name === 'AbortError' ? 'Request timed out.' : (e.message || 'Request failed.')); })
      .finally(function () { clearTimeout(to); setTimeout(tick, POLL_MS); });
  }
  tick();
})();
