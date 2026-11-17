'use strict';
(function () {
  var POLL_MS = 2000;
  var LIVE_MS = 1000;
  var last = null;
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
    last = s;
    if (current === 'live') renderLive(s);
    $('updated').textContent = 'updated ' + now.toLocaleTimeString();
  }

  var VIEWS = ['overview', 'pools', 'endpoints', 'live'];
  var current = '';
  function route() {
    var v = (location.hash.replace(/^#\/?/, '') || 'overview');
    if (VIEWS.indexOf(v) < 0) v = 'overview';
    VIEWS.forEach(function (name) {
      $('view-' + name).hidden = name !== v;
      var a = document.querySelector('.sidebar a[data-view="' + name + '"]');
      if (name === v) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    });
    current = v;
    if (v === 'live' && last) renderLive(last);
    document.title = v.charAt(0).toUpperCase() + v.slice(1) + ' · LLM Proxy Manifestd dashboard';
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

  // ---- Live View: inline SVG, deterministic layout, updated in place ----
  var NS = 'http://www.w3.org/2000/svg';
  var live = { sig: '', els: {} };
  function svg(tag, attrs, text) {
    var e = document.createElementNS(NS, tag);
    for (var k in attrs) e.setAttribute(k, attrs[k]);
    if (text != null) e.textContent = text;
    return e;
  }
  // Keep header text inside the node: ellipsize (full text stays in a <title> tooltip).
  function clip(t, n) { return t.length > n ? t.slice(0, n - 1) + '…' : t; }
  function modelOf(ep, t) { return t.target_model || ep.name; }
  function renderLive(s) {
    var pools = s.pools || [], eps = s.endpoints || [];
    // Pools sharing a concurrency group are one provider: show a single node per group.
    var byPool = {}, poolOrder = [], nodeOf = {};
    pools.forEach(function (p) {
      var k = p.concurrency_group ? 'g\0' + p.concurrency_group : 'p\0' + p.name;
      nodeOf[p.name] = k;
      if (!byPool[k]) { byPool[k] = { p: p, models: [], title: p.concurrency_group || p.name, protos: [] }; poolOrder.push(k); }
      if (p.protocol && byPool[k].protos.indexOf(p.protocol) < 0) byPool[k].protos.push(p.protocol);
    });
    eps.forEach(function (ep) {
      ep.targets.forEach(function (t) {
        var b = byPool[nodeOf[t.pool]];
        if (b && b.models.indexOf(modelOf(ep, t)) < 0) b.models.push(modelOf(ep, t));
      });
    });
    // Group routes by protocol (anthropic first), stable order.
    var routes = eps.slice().sort(function (a, b) { return a.protocol === b.protocol ? 0 : (a.protocol < b.protocol ? -1 : 1); });
    var ROW = 34, GAP = 8, RW = 190, MW = 220, PADX = 14, PW = MW + 2 * PADX, HEAD = 44, PGAP = 18, TOP = 28, X1 = 10, X2 = 10 + RW + 360;
    // Structure signature: only rebuild the DOM when the topology changes.
    var sig = JSON.stringify([routes.map(function (e) { return [e.protocol, e.name, e.targets.map(function (t) { return [nodeOf[t.pool], modelOf(e, t)]; })]; }),
      poolOrder.map(function (n) { return [n, byPool[n].title, byPool[n].models]; })]);
    var svgEl = $('live');
    if (sig !== live.sig) {
      live.sig = sig; live.els = { routes: {}, pools: {}, arrows: [] };
      svgEl.replaceChildren();
      var defs = svg('defs', {});
      defs.appendChild(svg('marker', { id: 'arr', viewBox: '0 0 10 10', refX: '9', refY: '5', markerWidth: '7', markerHeight: '7', orient: 'auto-start-reverse' })).appendChild(svg('path', { d: 'M0 0L10 5L0 10z', 'class': 'arrowhead' }));
      svgEl.appendChild(defs);
      var layer = { links: svg('g', {}), nodes: svg('g', {}), labels: svg('g', {}) };
      svgEl.appendChild(layer.links); svgEl.appendChild(layer.nodes); svgEl.appendChild(layer.labels);
      // Pools.
      var y = TOP, modelPos = {};
      poolOrder.forEach(function (n) {
        var b = byPool[n], h = HEAD + Math.max(1, b.models.length) * (ROW + GAP) + 4;
        var g = svg('g', { 'class': 'pool' });
        var rect = svg('rect', { x: X2, y: y, width: PW, height: h, rx: 10 });
        var title = svg('text', { x: X2 + PADX, y: y + 20, 'class': 'ptitle' }, clip(b.title, 20));
        title.appendChild(svg('title', {}, b.title));
        var sub = svg('text', { x: X2 + PADX, y: y + 36, 'class': 'psub' });
        var badge = svg('text', { x: X2 + PW - PADX, y: y + 20, 'class': 'pbadge', 'text-anchor': 'end' });
        [rect, title, sub, badge].forEach(function (e) { g.appendChild(e); });
        layer.nodes.appendChild(g);
        b.models.forEach(function (m, i) {
          var my = y + HEAD + i * (ROW + GAP);
          modelPos[n + '\0' + m] = my + ROW / 2;
          var mg = svg('g', { 'class': 'model' });
          mg.appendChild(svg('rect', { x: X2 + PADX, y: my, width: MW, height: ROW, rx: 6 }));
          mg.appendChild(svg('text', { x: X2 + PADX + 10, y: my + ROW / 2 + 4 }, m.length > 30 ? m.slice(0, 29) + '…' : m)).appendChild(svg('title', {}, m));
          layer.nodes.appendChild(mg);
        });
        live.els.pools[n] = { rect: rect, sub: sub, badge: badge, top: y, h: h };
        y += h + PGAP;
      });
      var poolsH = y;
      // Routes.
      var ry = TOP, lastProto = '', routeH = 0, fan = {}, seen = {};
      routes.forEach(function (ep) { ep.targets.forEach(function (t) { var k = nodeOf[t.pool] + '\0' + modelOf(ep, t); fan[k] = (fan[k] || 0) + 1; }); });
      routes.forEach(function (ep) {
        if (ep.protocol !== lastProto) {
          lastProto = ep.protocol;
          layer.nodes.appendChild(svg('text', { x: X1, y: ry + 4, 'class': 'proto' }, ep.protocol.toUpperCase()));
          ry += 14;
        }
        var key = ep.protocol + '/' + ep.name;
        var g = svg('g', { 'class': 'route' });
        g.appendChild(svg('rect', { x: X1, y: ry, width: RW, height: ROW + 4, rx: 6 }));
        g.appendChild(svg('text', { x: X1 + 10, y: ry + 15 }, ep.name.length > 24 ? ep.name.slice(0, 23) + '…' : ep.name)).appendChild(svg('title', {}, ep.name));
        var q = svg('text', { x: X1 + 10, y: ry + 29, 'class': 'psub' });
        g.appendChild(q);
        layer.nodes.appendChild(g);
        live.els.routes[key] = { q: q, ep: ep };
        var cy = ry + (ROW + 4) / 2;
        ep.targets.forEach(function (t, i) {
          var mk = nodeOf[t.pool] + '\0' + modelOf(ep, t), ty = modelPos[mk];
          if (ty == null) return;
          var k = seen[mk] = (seen[mk] || 0) + 1;
          ty += (k - 1 - (fan[mk] - 1) / 2) * 11; // fan arrows out on the model's edge
          var x1 = X1 + RW, x2 = X2 + PADX, mx = (x1 + x2) / 2;
          var path = svg('path', { d: 'M' + x1 + ' ' + cy + ' C' + mx + ' ' + cy + ' ' + mx + ' ' + ty + ' ' + (x2 - 1) + ' ' + ty, 'class': 'link' + (i > 0 ? ' fo' : ''), 'marker-end': 'url(#arr)', tabindex: '0' });
          var lbl = svg('text', { x: x2 - 36, y: ty - 4, 'class': 'llabel', 'text-anchor': 'middle' });
          path.addEventListener('focus', function () { lbl.classList.add('focus'); });
          path.addEventListener('blur', function () { lbl.classList.remove('focus'); });
          layer.links.appendChild(path); layer.labels.appendChild(lbl);
          live.els.arrows.push({ path: path, lbl: lbl, key: key, i: i, pool: nodeOf[t.pool], model: modelOf(ep, t), order: i > 0 ? String(i + 1) : '' });
        });
        ry += ROW + 4 + GAP;
      });
      routeH = ry;
      svgEl.setAttribute('viewBox', '0 0 ' + (X2 + PW + 10) + ' ' + Math.max(poolsH, routeH, 60));
      svgEl.setAttribute('width', X2 + PW + 10);
      svgEl.setAttribute('height', Math.max(poolsH, routeH, 60));
    }
    // In-place update of values.
    var epByKey = {};
    eps.forEach(function (e) { epByKey[e.protocol + '/' + e.name] = e; });
    var perPool = {};
    live.els.arrows.forEach(function (a) {
      var ep = epByKey[a.key], t = ep && ep.targets[a.i];
      a.x = t ? t.in_flight || 0 : 0;
      a.skipped = t ? t.skipped_concurrency_full || 0 : 0;
      perPool[a.pool] = (perPool[a.pool] || 0) + a.x;
    });
    poolOrder.forEach(function (n) {
      var e = live.els.pools[n], p = byPool[n].p;
      if (!e) return;
      var sat = !p.unlimited && p.active_slots >= p.max_concurrency;
      e.rect.setAttribute('class', sat ? 'sat' : (p.active_slots > 0 ? 'busy' : ''));
      var subTxt = byPool[n].protos.join(' + ') + ' · ' + p.active_slots + (p.unlimited ? '' : '/' + p.max_concurrency) + ' slots' + (p.coordinated ? ' · shared' : '');
      if (e.sub.textContent !== clip(subTxt, 40)) { e.sub.textContent = clip(subTxt, 40); e.sub.appendChild(svg('title', {}, subTxt)); }
      e.badge.textContent = p.queued > 0 ? p.queued + ' queued' : '';
    });
    live.els.arrows.forEach(function (a) {
      var p = byPool[a.pool].p;
      var txt = a.x + (p.unlimited ? '' : '/' + p.max_concurrency);
      var cls = 'link' + (a.i > 0 ? ' fo' : '') + (a.x > 0 ? ' on' : '');
      if (a.path.getAttribute('class') !== cls) a.path.setAttribute('class', cls);
      if (a.lbl.textContent !== (a.order ? '#' + a.order + ' ' + txt : txt)) a.lbl.textContent = a.order ? '#' + a.order + ' ' + txt : txt;
      a.lbl.setAttribute('class', 'llabel' + (a.x > 0 ? ' on' : '') + (a.lbl.classList.contains('focus') ? ' focus' : ''));
      var label = a.key.split('/')[1] + ' to ' + a.model + ' in ' + byPool[a.pool].title + ': ' + a.x + (p.unlimited ? ' active' : ' of ' + p.max_concurrency + ' active') + (a.order ? ' (failover target ' + a.order + ')' : '') + (a.skipped ? ', skipped ' + a.skipped + ' (concurrency full)' : '');
      if (a.path.getAttribute('aria-label') !== label) a.path.setAttribute('aria-label', label);
    });
    for (var k in live.els.routes) {
      var r = live.els.routes[k], e = epByKey[k];
      var act = e ? e.targets.reduce(function (a, t) { return a + (t.in_flight || 0); }, 0) : 0;
      r.q.textContent = e ? act + ' active' + (e.queued > 0 ? ' · ' + e.queued + ' queued' : '') : '';
    }
  }

  function liveTick() {
    if (current !== 'live' || document.hidden) return;
    fetch('api/state', { cache: 'no-store' }).then(function (r) { return r.ok ? r.json() : null; })
      .then(function (s) { if (s && current === 'live') { last = s; renderLive(s); } }).catch(function () {});
  }
  setInterval(liveTick, LIVE_MS);

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
