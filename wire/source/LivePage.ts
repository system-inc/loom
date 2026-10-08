// The live page for one run: plain HTML and inline script, no framework, nothing loaded from elsewhere.
// The run id and the token are read from the page's own URL by the script, so nothing from the request is
// ever written into the HTML. Styles and the script carry the response's CSP nonce.

export function renderLivePage(nonce: string): string {
    return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Loom run</title>
<style nonce="${nonce}">
:root {
    color-scheme: light dark;
    --background: #f7f7f5;
    --surface: #ffffff;
    --surface-raised: #f0f0ec;
    --border: #e2e2dc;
    --text: #1d1d1b;
    --text-muted: #6b6b66;
    --queued: #8a8a84;
    --running: #2563eb;
    --running-surface: #e8effd;
    --passed: #15803d;
    --passed-surface: #e5f4ea;
    --failed: #c2261d;
    --failed-surface: #fdeceb;
    --failed-border: #f2b8b3;
    --broken: #a15c00;
    --broken-surface: #fbf0de;
    --stderr: #b4371f;
    --meta: #6b6b66;
    --monospace: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
@media (prefers-color-scheme: dark) {
    :root {
        --background: #121211;
        --surface: #1b1b1a;
        --surface-raised: #242422;
        --border: #2f2f2c;
        --text: #ececea;
        --text-muted: #9a9a94;
        --queued: #8a8a84;
        --running: #7aa2f7;
        --running-surface: #1c2840;
        --passed: #4ade80;
        --passed-surface: #15291c;
        --failed: #f87171;
        --failed-surface: #34191a;
        --failed-border: #6b2a28;
        --broken: #f2b155;
        --broken-surface: #33260f;
        --stderr: #f59e85;
        --meta: #9a9a94;
    }
}
* { box-sizing: border-box; }
html, body { margin: 0; }
body {
    background: var(--background);
    color: var(--text);
    font: 15px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif;
    -webkit-text-size-adjust: 100%;
}
main { max-width: 1040px; margin: 0 auto; padding: 20px 16px 48px; }
header { margin-bottom: 16px; }
.title { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
h1 { font: 600 18px/1.3 var(--monospace); margin: 0; overflow-wrap: anywhere; }
.connection { font-size: 12px; color: var(--text-muted); display: inline-flex; align-items: center; gap: 6px; }
.connection::before { content: ""; width: 8px; height: 8px; border-radius: 50%; background: var(--queued); }
.connection[data-state="live"]::before { background: var(--passed); }
.connection[data-state="reconnecting"]::before { background: var(--broken); }
.counts { display: flex; flex-wrap: wrap; gap: 6px 14px; margin-top: 8px; font-size: 13px; color: var(--text-muted); }
.counts b { color: var(--text); font-weight: 600; font-variant-numeric: tabular-nums; }
.verdict { margin-top: 14px; padding: 12px 14px; border-radius: 10px; border: 1px solid var(--border); background: var(--surface); }
.verdict[hidden] { display: none; }
.verdict .status { font-weight: 700; text-transform: uppercase; letter-spacing: 0.04em; }
.verdict[data-status="green"] { border-color: var(--passed); background: var(--passed-surface); }
.verdict[data-status="green"] .status { color: var(--passed); }
.verdict[data-status="red"] { border-color: var(--failed); background: var(--failed-surface); }
.verdict[data-status="red"] .status { color: var(--failed); }
.verdict[data-status="void"] { border-color: var(--broken); background: var(--broken-surface); }
.verdict[data-status="void"] .status { color: var(--broken); }
.verdict .label { margin-top: 6px; font-size: 13px; font-weight: 600; }
.verdict ul { margin: 2px 0 0; padding-left: 20px; font-size: 13px; }
.verdict li { overflow-wrap: anywhere; }
.rows { display: flex; flex-direction: column; gap: 6px; }
.empty { color: var(--text-muted); padding: 24px 0; text-align: center; }
.row { background: var(--surface); border: 1px solid var(--border); border-radius: 10px; overflow: hidden; }
.row[data-status="failed"] { background: var(--failed-surface); border-color: var(--failed-border); }
.summary {
    all: unset; box-sizing: border-box; cursor: pointer; width: 100%;
    display: grid; grid-template-columns: minmax(0, 14rem) auto 5.5rem minmax(0, 1fr);
    grid-template-areas: "unit chip elapsed last"; align-items: center; gap: 12px; padding: 10px 14px;
}
.summary:focus-visible { outline: 2px solid var(--running); outline-offset: -2px; }
.unit { grid-area: unit; font: 500 13px/1.3 var(--monospace); overflow-wrap: anywhere; }
.unit .stray { color: var(--broken); font: 11px system-ui, sans-serif; margin-left: 6px; }
.chip {
    grid-area: chip; justify-self: start; font-size: 11px; font-weight: 600; letter-spacing: 0.03em;
    text-transform: uppercase; padding: 2px 8px; border-radius: 999px; color: var(--queued);
    background: var(--surface-raised);
}
.row[data-status="running"] .chip { color: var(--running); background: var(--running-surface); }
.row[data-status="passed"] .chip { color: var(--passed); background: var(--passed-surface); }
.row[data-status="failed"] .chip { color: #ffffff; background: var(--failed); }
.row[data-status="broken"] .chip { color: var(--broken); background: var(--broken-surface); }
.elapsed { grid-area: elapsed; font: 12px var(--monospace); color: var(--text-muted); text-align: right; font-variant-numeric: tabular-nums; }
.last { grid-area: last; font: 12px/1.4 var(--monospace); color: var(--text-muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.lines {
    margin: 0; padding: 10px 14px 12px; border-top: 1px solid var(--border); background: var(--surface-raised);
    font: 12px/1.5 var(--monospace); white-space: pre-wrap; overflow-wrap: anywhere; max-height: 60vh; overflow-y: auto;
}
.row[data-status="failed"] .lines { border-top-color: var(--failed-border); }
.lines[hidden] { display: none; }
.lines .stderr { color: var(--stderr); }
.lines .meta { color: var(--meta); font-style: italic; }
.lines .error { color: var(--failed); font-weight: 600; }
.lines .note { color: var(--text-muted); display: block; margin-bottom: 6px; font-family: system-ui, sans-serif; }
@media (max-width: 640px) {
    main { padding: 14px 16px 40px; }
    .summary {
        grid-template-columns: minmax(0, 1fr) auto auto;
        grid-template-areas: "unit chip elapsed" "last last last"; gap: 4px 10px; padding: 10px 12px;
    }
    .elapsed { text-align: right; }
    .lines { padding: 10px 12px; }
}
</style>
</head>
<body>
<main>
<header>
    <div class="title"><h1 id="run-id">Loom run</h1><span class="connection" id="connection" data-state="connecting">connecting</span></div>
    <div class="counts" id="counts"></div>
    <section class="verdict" id="verdict" hidden></section>
</header>
<div class="rows" id="rows"><div class="empty" id="empty">Waiting for the plan.</div></div>
</main>
<script nonce="${nonce}">
(function () {
    'use strict';
    var tailLength = 20;
    var runId = decodeURIComponent(location.pathname.split('/')[2] || '');
    var token = new URLSearchParams(location.search).get('token') || '';
    var units = new Map();
    var plan = [];
    var lastPosition = 0;
    var verdict = null;
    var attempt = 0;
    var dirty = new Set();
    var headerDirty = true;
    var frameRequested = false;

    document.getElementById('run-id').textContent = runId;
    document.title = runId + ' · Loom';

    function unitFor(id, planned) {
        var unit = units.get(id);
        if (!unit) {
            unit = { id: id, planned: planned, status: 'queued', startedAt: null, endedAt: null, wallSeconds: null, lastLine: '', lines: [], expanded: false, element: null, renderedLines: -1, renderedMode: '' };
            units.set(id, unit);
            createRow(unit);
        }
        if (planned && !unit.planned) {
            unit.planned = true;
        }
        return unit;
    }

    function createRow(unit) {
        var row = document.createElement('div');
        row.className = 'row';
        var summary = document.createElement('button');
        summary.type = 'button';
        summary.className = 'summary';
        var name = document.createElement('span');
        name.className = 'unit';
        var chip = document.createElement('span');
        chip.className = 'chip';
        var elapsed = document.createElement('span');
        elapsed.className = 'elapsed';
        var last = document.createElement('span');
        last.className = 'last';
        summary.append(name, chip, elapsed, last);
        var lines = document.createElement('pre');
        lines.className = 'lines';
        lines.hidden = true;
        row.append(summary, lines);
        summary.addEventListener('click', function () {
            unit.expanded = !unit.expanded;
            summary.setAttribute('aria-expanded', String(unit.expanded));
            markDirty(unit);
        });
        unit.element = { row: row, name: name, chip: chip, elapsed: elapsed, last: last, lines: lines };
        var empty = document.getElementById('empty');
        if (empty) {
            empty.remove();
        }
        document.getElementById('rows').append(row);
    }

    // Planned units in plan order, then any unit nobody planned.
    function orderRows() {
        var container = document.getElementById('rows');
        plan.forEach(function (id) {
            container.append(units.get(id).element.row);
        });
        units.forEach(function (unit) {
            if (!unit.planned) {
                container.append(unit.element.row);
            }
        });
    }

    function apply(event) {
        var unit = unitFor(event.unit, false);
        var time = Date.parse(event.time);
        if (unit.status === 'queued') {
            unit.status = 'running';
            if (unit.startedAt === null) {
                unit.startedAt = time;
            }
        }
        if (event.type === 'started') {
            unit.startedAt = time;
            unit.lines.push({ kind: 'meta', text: 'started on ' + (event.machine || 'a runner') + (event.runnerVersion ? ' (runner ' + event.runnerVersion + ')' : '') });
        }
        else if (event.type === 'output') {
            var text = event.text || '';
            unit.lastLine = text;
            unit.lines.push({ kind: event.stream === 'stderr' ? 'stderr' : 'stdout', text: text });
        }
        else if (event.type === 'exit') {
            // A zero is left off the line (docs/protocol.md): a missing wallSeconds is an instant exit.
            unit.wallSeconds = event.wallSeconds || 0;
            var how = event.timedOut ? 'timed out' : event.signal ? 'killed by ' + event.signal : 'exited ' + (event.code === undefined ? '?' : event.code);
            unit.lines.push({ kind: 'meta', text: how + ' after ' + formatSeconds(unit.wallSeconds) });
        }
        else if (event.type === 'uploaded') {
            unit.lines.push({ kind: 'meta', text: 'uploaded ' + event.path + ' (' + (event.bytes || 0) + ' bytes)' });
        }
        else if (event.type === 'error') {
            unit.lines.push({ kind: 'error', text: event.phase + ' error: ' + (event.message || '') });
        }
        else if (event.type === 'cached') {
            unit.lines.push({ kind: 'meta', text: 'from the cache (run ' + event.fromRun + ')' });
        }
        else if (event.type === 'finished') {
            unit.status = event.status;
            unit.endedAt = time;
        }
        markDirty(unit);
    }

    function formatSeconds(seconds) {
        if (!(seconds >= 0)) {
            return '';
        }
        if (seconds < 10) {
            return seconds.toFixed(1) + 's';
        }
        var whole = Math.round(seconds);
        if (whole < 60) {
            return whole + 's';
        }
        var minutes = Math.floor(whole / 60);
        if (minutes < 60) {
            return minutes + 'm ' + String(whole % 60).padStart(2, '0') + 's';
        }
        return Math.floor(minutes / 60) + 'h ' + String(minutes % 60).padStart(2, '0') + 'm';
    }

    function elapsedFor(unit) {
        if (unit.startedAt === null || isNaN(unit.startedAt)) {
            return '';
        }
        // Once it has ended, the runner's own measure of the command beats two clocks' timestamps.
        if (unit.endedAt !== null && unit.wallSeconds !== null) {
            return formatSeconds(unit.wallSeconds);
        }
        var end = unit.endedAt !== null ? unit.endedAt : Date.now();
        return formatSeconds(Math.max(0, end - unit.startedAt) / 1000);
    }

    function markDirty(unit) {
        dirty.add(unit);
        headerDirty = true;
        if (!frameRequested) {
            frameRequested = true;
            requestAnimationFrame(render);
        }
    }

    function render() {
        frameRequested = false;
        dirty.forEach(renderUnit);
        dirty.clear();
        if (headerDirty) {
            renderHeader();
            headerDirty = false;
        }
    }

    function lineNode(line) {
        var span = document.createElement('span');
        if (line.kind !== 'stdout') {
            span.className = line.kind;
        }
        span.textContent = line.text + '\\n';
        return span;
    }

    function renderUnit(unit) {
        var element = unit.element;
        element.row.dataset.status = unit.status;
        element.name.textContent = unit.id;
        if (!unit.planned) {
            var stray = document.createElement('span');
            stray.className = 'stray';
            stray.textContent = 'not in the plan';
            element.name.append(stray);
        }
        element.chip.textContent = unit.status;
        element.elapsed.textContent = elapsedFor(unit);
        element.last.textContent = unit.lastLine;
        // A failed unit shows its last lines without a click; a click shows everything.
        var mode = unit.expanded ? 'full' : unit.status === 'failed' ? 'tail' : 'none';
        element.lines.hidden = mode === 'none';
        if (mode === 'full') {
            if (unit.renderedMode !== 'full') {
                element.lines.replaceChildren();
                unit.renderedLines = 0;
                if (unit.lines.length === 0) {
                    element.lines.append(note('No output yet.'));
                }
            }
            else if (unit.renderedLines === 0 && unit.lines.length > 0) {
                element.lines.replaceChildren();
            }
            var fragment = document.createDocumentFragment();
            for (var index = unit.renderedLines; index < unit.lines.length; index++) {
                fragment.append(lineNode(unit.lines[index]));
            }
            element.lines.append(fragment);
            unit.renderedLines = unit.lines.length;
        }
        else if (mode === 'tail') {
            var tail = unit.lines.slice(-tailLength);
            element.lines.replaceChildren();
            if (unit.lines.length > tailLength) {
                element.lines.append(note('Last ' + tailLength + ' of ' + unit.lines.length + ' lines. Click for all of them.'));
            }
            tail.forEach(function (line) {
                element.lines.append(lineNode(line));
            });
        }
        unit.renderedMode = mode;
    }

    function note(text) {
        var span = document.createElement('span');
        span.className = 'note';
        span.textContent = text;
        return span;
    }

    function renderHeader() {
        var counts = { queued: 0, running: 0, passed: 0, failed: 0, broken: 0 };
        var stray = 0;
        units.forEach(function (unit) {
            if (counts[unit.status] !== undefined) {
                counts[unit.status]++;
            }
            if (!unit.planned) {
                stray++;
            }
        });
        var container = document.getElementById('counts');
        container.replaceChildren();
        function count(label, value) {
            var item = document.createElement('span');
            var number = document.createElement('b');
            number.textContent = String(value);
            item.append(number, ' ' + label);
            container.append(item);
        }
        count(plan.length === 1 ? 'unit' : 'units', plan.length);
        count('queued', counts.queued);
        count('running', counts.running);
        count('passed', counts.passed);
        count('failed', counts.failed);
        count('broken', counts.broken);
        if (stray > 0) {
            count('not in the plan', stray);
        }
        var section = document.getElementById('verdict');
        if (verdict === null) {
            section.hidden = true;
            return;
        }
        section.hidden = false;
        section.dataset.status = verdict.status;
        section.replaceChildren();
        var heading = document.createElement('div');
        var status = document.createElement('span');
        status.className = 'status';
        status.textContent = verdict.status;
        var meaning = { green: 'Every planned unit passed, each exactly once.', red: 'Some units failed.', 'void': 'The run proved nothing either way.' };
        heading.append(status, ' ' + (meaning[verdict.status] || ''));
        section.append(heading);
        function list(title, items) {
            if (!items || items.length === 0) {
                return;
            }
            var label = document.createElement('div');
            label.className = 'label';
            label.textContent = title;
            var listElement = document.createElement('ul');
            items.forEach(function (item) {
                var entry = document.createElement('li');
                entry.textContent = item;
                listElement.append(entry);
            });
            section.append(label, listElement);
        }
        list('Failed', verdict.failed);
        list('Problems', verdict.problems);
        list('Served from the cache', verdict.cached);
        document.title = verdict.status + ' · ' + runId + ' · Loom';
    }

    function handle(frame) {
        if (frame.kind === 'plan') {
            plan = frame.units;
            plan.forEach(function (id) {
                markDirty(unitFor(id, true));
            });
            orderRows();
        }
        else if (frame.kind === 'event') {
            // A reconnect asks for everything after the last position seen, so this only guards a race.
            if (frame.position <= lastPosition) {
                return;
            }
            lastPosition = frame.position;
            apply(frame.event);
        }
        else if (frame.kind === 'verdict') {
            verdict = frame.verdict;
            headerDirty = true;
            if (!frameRequested) {
                frameRequested = true;
                requestAnimationFrame(render);
            }
        }
    }

    function setConnection(state) {
        var element = document.getElementById('connection');
        element.dataset.state = state;
        element.textContent = state;
    }

    function connect() {
        var scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
        var url = scheme + '//' + location.host + '/runs/' + encodeURIComponent(runId) + '/stream?token=' + encodeURIComponent(token) + '&after=' + lastPosition;
        var socket = new WebSocket(url);
        socket.addEventListener('open', function () {
            attempt = 0;
            setConnection('live');
        });
        socket.addEventListener('message', function (message) {
            try {
                handle(JSON.parse(message.data));
            }
            catch (error) {
                console.error('Loom: a frame could not be read', error);
            }
        });
        socket.addEventListener('close', function () {
            if (verdict !== null) {
                setConnection('done');
                return;
            }
            setConnection('reconnecting');
            var delay = Math.min(15000, 500 * Math.pow(2, attempt)) * (0.5 + Math.random() / 2);
            attempt++;
            setTimeout(connect, delay);
        });
    }

    // Running units' elapsed time moves on its own.
    setInterval(function () {
        units.forEach(function (unit) {
            if (unit.status === 'running') {
                markDirty(unit);
            }
        });
    }, 1000);

    renderHeader();
    connect();
})();
</script>
</body>
</html>
`;
}
