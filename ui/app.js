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

  function render(s) {
    var now = new Date(s.now);
    $('error').hidden = true;
    setStatus(s.status === 'ok' ? 'Healthy' : 'Degraded: last config reload failed', s.status === 'ok' ? 'ok' : 'warn');
    $('version').textContent = s.version;
    $('uptime').textContent = dur(s.uptime_seconds);
    var t = s.totals;
    $('slots').textContent = t.active_slots + ' / ' + (t.unlimited ? 'unlimited' : t.slots);
    $('slots-bar').style.width = (!t.unlimited && t.slots ? Math.min(100, 100 * t.active_slots / t.slots) : 0) + '%';
    $('queued').textContent = n(t.queued);
    $('inflight').textContent = n(t.in_flight);
    $('totals').textContent = n(t.requests) + ' / ' + n(t.errors);
    $('config').textContent = 'gen ' + s.config.generation;
    var r = 'applied ' + ago(s.config.loaded_at, now);
    if (s.config.last_reload_attempt) {
      r += '; last check ' + ago(s.config.last_reload_attempt, now);
      if (s.config.last_reload_failed) r += ' (rejected, previous config kept)';
    }
    $('reload').textContent = r;

    fill($('pools'), s.pools.map(function (p) {
      var q = p.queue_by_priority.map(function (x) { return 'p' + x.priority + ': ' + x.count; }).join(', ') || '–';
      return [[p.name + (p.protocol ? ' (' + p.protocol + ')' : '')], [p.upstream], [p.active_slots + ' / ' + (p.unlimited ? 'unlimited' : p.max_concurrency), 'num'],
        [n(p.queued), 'num'], [q], [n(p.total_served), 'num']];
    }), 6);
    fill($('endpoints'), (s.endpoints || []).map(function (x) {
      var tg = x.targets.map(function (t) { return t.pool + (t.target_model ? ' → ' + t.target_model : ''); }).join(', ');
      var ups = x.targets.map(function (t) { return t.upstream; }).join(', ');
      var prio = x.targets.map(function (t) { return t.priority; }).join(' / ');
      return [[x.name], [x.protocol], [x.mode], [tg], [ups], [prio, 'num'], [n(x.in_flight), 'num'],
        [n(x.queued), 'num'], [n(x.requests), 'num'], [n(x.errors), 'num' + (x.errors ? ' err' : '')]];
    }), 10);
    $('updated').textContent = 'updated ' + now.toLocaleTimeString();
  }

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
