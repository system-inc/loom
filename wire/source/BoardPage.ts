// The board: every run on every machine, live (docs/protocol.md, "The board"). Plain HTML and inline script,
// nothing loaded from elsewhere. The board token rides after the # in the page's address, which a browser
// never sends, and goes to the stream only as a WebSocket subprotocol, so it is in no URL a server logs.
// Styles and the script carry the response's CSP nonce. This is the first draft; the design comes after.

export function renderBoardPage(nonce: string): string {
    return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Loom</title>
<style nonce="${nonce}">
:root {
    color-scheme: light dark;
    --background: #f6f6f3;
    --surface: #ffffff;
    --surface-sunk: #efefea;
    --border: #e3e3dc;
    --text: #1b1b19;
    --text-muted: #6d6d66;
    --text-faint: #9d9d95;
    --queued: #b9b9b0;
    --running: #2f6bed;
    --running-soft: #e7eefd;
    --passed: #16803c;
    --passed-soft: #e4f3e9;
    --failed: #c8261d;
    --failed-soft: #fcebea;
    --void: #8a5a00;
    --void-soft: #f8efdc;
    --loom: #7c4dff;
    --loom-soft: #efe9ff;
    --shimmer: rgba(47, 107, 237, 0.16);
    --monospace: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    --radius: 12px;
}
@media (prefers-color-scheme: dark) {
    :root {
        --background: #0e0f11;
        --surface: #16171a;
        --surface-sunk: #1d1f23;
        --border: #2a2c31;
        --text: #ecedef;
        --text-muted: #9b9ea6;
        --text-faint: #6b6e76;
        --queued: #4a4d55;
        --running: #79a6ff;
        --running-soft: #18233a;
        --passed: #4fd68a;
        --passed-soft: #132a1d;
        --failed: #ff7a70;
        --failed-soft: #341818;
        --void: #f0b34e;
        --void-soft: #33270f;
        --loom: #b69cff;
        --loom-soft: #231b3d;
        --shimmer: rgba(121, 166, 255, 0.16);
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
main { max-width: 1180px; margin: 0 auto; padding: 18px 16px 56px; }
.top { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.brand { display: flex; align-items: center; gap: 10px; font-weight: 650; letter-spacing: 0.01em; font-size: 17px; }
.mark { width: 22px; height: 22px; border: 2px solid var(--text); border-radius: 3px; display: grid; place-items: center; }
.mark::after { content: ""; width: 13px; height: 13px; border-radius: 50%; border: 2px solid var(--text); }
.connection { font-size: 12px; color: var(--text-muted); display: inline-flex; align-items: center; gap: 6px; }
.connection::before { content: ""; width: 8px; height: 8px; border-radius: 50%; background: var(--queued); }
.connection[data-state="live"]::before { background: var(--passed); box-shadow: 0 0 0 3px var(--passed-soft); }
.connection[data-state="reconnecting"]::before { background: var(--void); }

.pulse { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; margin: 16px 0 18px; }
.stat { background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius); padding: 12px 14px; min-width: 0; }
.stat .value { font: 600 26px/1.1 system-ui, sans-serif; font-variant-numeric: tabular-nums; }
.stat .label { font-size: 12px; color: var(--text-muted); margin-top: 4px; }
.stat .detail { font: 12px/1.3 var(--monospace); color: var(--text-muted); margin-top: 4px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.stat.running .value { color: var(--running); }

h2 { font-size: 12px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.08em; color: var(--text-muted); margin: 22px 0 10px; }
.runs { display: grid; gap: 12px; }
.run { background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius); padding: 14px 16px; border-left-width: 4px; }
.run[data-state="running"] { border-left-color: var(--running); }
.run[data-state="green"] { border-left-color: var(--passed); }
.run[data-state="red"] { border-left-color: var(--failed); }
.run[data-state="void"] { border-left-color: var(--void); border-left-style: dashed; }
.run .head { display: flex; align-items: baseline; gap: 10px; flex-wrap: wrap; }
.run .job { font-weight: 650; font-size: 16px; }
.run .id { font: 12px var(--monospace); color: var(--text-faint); overflow-wrap: anywhere; }
.run .spacer { flex: 1; }
.badge { font: 600 11px/1 system-ui, sans-serif; letter-spacing: 0.06em; text-transform: uppercase; padding: 5px 8px; border-radius: 999px; display: inline-flex; align-items: center; gap: 6px; white-space: nowrap; }
.badge[data-state="running"] { color: var(--running); background: var(--running-soft); }
.badge[data-state="green"] { color: var(--passed); background: var(--passed-soft); }
.badge[data-state="red"] { color: var(--failed); background: var(--failed-soft); }
.badge[data-state="void"] { color: var(--void); background: var(--void-soft); }
.elapsed { font: 13px var(--monospace); color: var(--text-muted); font-variant-numeric: tabular-nums; }
.open { font: inherit; font-size: 13px; color: var(--text); background: var(--surface-sunk); border: 1px solid var(--border); border-radius: 8px; padding: 5px 10px; cursor: pointer; }
.open:hover { border-color: var(--text-faint); }

.bar { display: flex; height: 8px; border-radius: 999px; overflow: hidden; background: var(--surface-sunk); margin: 12px 0 8px; }
.bar span { height: 100%; transition: flex-grow 0.4s ease; }
.bar .passed { background: var(--passed); }
.bar .failed { background: var(--failed); }
.bar .broken { background: var(--void); background-image: repeating-linear-gradient(45deg, transparent 0 3px, rgba(0,0,0,0.25) 3px 6px); }
.bar .running { background: var(--running); background-image: repeating-linear-gradient(90deg, transparent 0 6px, rgba(255,255,255,0.3) 6px 8px); animation: drift 1.2s linear infinite; background-size: 16px 100%; }
.counts { display: flex; flex-wrap: wrap; gap: 4px 14px; font-size: 13px; color: var(--text-muted); }
.counts b { color: var(--text); font-weight: 600; font-variant-numeric: tabular-nums; }

.threads { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 12px; }
.thread { font: 12px/1 var(--monospace); background: var(--running-soft); color: var(--text); border-radius: 7px; padding: 6px 8px; display: inline-flex; gap: 8px; align-items: center; max-width: 100%; }
.thread .unit { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.thread .where { color: var(--text-muted); }
.thread .time { color: var(--running); font-variant-numeric: tabular-nums; }
.more { font-size: 12px; color: var(--text-muted); align-self: center; }

.failures { margin-top: 12px; display: grid; gap: 8px; }
.failure { background: var(--failed-soft); border-radius: 9px; padding: 9px 11px; }
.failure[data-status="broken"] { background: var(--void-soft); }
.failure .what { font: 600 13px var(--monospace); display: flex; gap: 8px; align-items: center; overflow-wrap: anywhere; }
.failure pre { margin: 6px 0 0; font: 12px/1.45 var(--monospace); white-space: pre-wrap; overflow-wrap: anywhere; color: var(--text); opacity: 0.9; }
.failure.fresh { animation: flare 1.4s ease-out 1; }

.history { display: grid; gap: 6px; }
.past { display: flex; align-items: center; gap: 10px; background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 8px 12px; font-size: 13px; cursor: pointer; }
.past .job { font-weight: 600; }
.past .id { font: 12px var(--monospace); color: var(--text-faint); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; flex: 1; min-width: 0; }
.past .tally { color: var(--text-muted); font-variant-numeric: tabular-nums; white-space: nowrap; }

.empty { color: var(--text-muted); background: var(--surface); border: 1px dashed var(--border); border-radius: var(--radius); padding: 22px; text-align: center; }
.empty code { font-family: var(--monospace); }

/* The machines: one block each, one line per slot, the way the gate itself is laid out. */
.fabric { display: grid; gap: 10px; }
.box { background: var(--surface); border: 1px solid var(--border); border-radius: var(--radius); padding: 6px 0 8px; }
.box-head { display: flex; align-items: baseline; gap: 10px; padding: 6px 14px 4px; flex-wrap: wrap; }
.box-name { font: 650 15px/1.3 var(--monospace); text-transform: capitalize; }
.box-meta { font: 12px var(--monospace); color: var(--text-faint); }
.box-busy { margin-left: auto; font-size: 12px; color: var(--text-muted); font-variant-numeric: tabular-nums; }
.line { position: relative; display: grid; grid-template-columns: 22px 76px minmax(0, 1fr) auto; align-items: center; gap: 10px; margin: 3px 10px 0 22px; padding: 7px 10px; border-radius: 9px; overflow: hidden; font-size: 13px; isolation: isolate; }
.line .glyph { font-weight: 700; text-align: center; }
.line .slot-label { font: 12px var(--monospace); color: var(--text-muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.line .what { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.line .what b { font-weight: 600; }
.line .what .sha, .line .what .step { font-family: var(--monospace); font-size: 12px; color: var(--text-muted); }
.line .detail { display: block; font: 12px/1.4 var(--monospace); color: var(--text-muted); white-space: normal; overflow-wrap: anywhere; margin-top: 2px; }
.line .time { font: 600 13px var(--monospace); font-variant-numeric: tabular-nums; white-space: nowrap; }
.line[data-state="idle"] { opacity: 0.42; }
.line[data-state="idle"] .glyph { color: var(--text-faint); }
.line[data-state="gating"] { background: var(--running-soft); }
.line[data-state="gating"] .glyph, .line[data-state="gating"] .time { color: var(--running); }
.line[data-state="loom"] { background: var(--loom-soft); }
.line[data-state="loom"] .glyph, .line[data-state="loom"] .time { color: var(--loom); }
.line[data-state="green"] { background: var(--passed-soft); }
.line[data-state="green"] .glyph { color: var(--passed); }
.line[data-state="red"] { background: var(--failed-soft); box-shadow: inset 3px 0 0 var(--failed); }
.line[data-state="red"] .glyph, .line[data-state="red"] .time { color: var(--failed); }
.line[data-state="crash"] { background: var(--void-soft); box-shadow: inset 3px 0 0 var(--void); }
.line[data-state="crash"] .glyph { color: var(--void); }
/* Busy lines move: a light that runs along them, slower for the whole gate. */
.line[data-state="gating"]::before, .line[data-state="loom"]::before, .line[data-state="red"][data-live="yes"]::before {
    content: ""; position: absolute; inset: 0; z-index: -1;
    background: linear-gradient(90deg, transparent 0%, var(--shimmer) 50%, transparent 100%);
    background-size: 40% 100%; background-repeat: no-repeat; animation: run 2.4s linear infinite;
}
.line[data-kind="full"]::before { animation-duration: 5s; }
.line[data-state="red"][data-live="yes"] { animation: alarm 2s ease-in-out infinite; }
/* The star wears the waterfall's rainbow ring. */
.line[data-star="yes"]::after {
    content: ""; position: absolute; inset: 0; border-radius: inherit; padding: 2px; pointer-events: none;
    background: linear-gradient(135deg, #3b82f6, #8b5cf6, #ec4899, #f97316); background-size: 300% 300%;
    -webkit-mask: linear-gradient(#fff 0 0) content-box, linear-gradient(#fff 0 0); -webkit-mask-composite: xor;
    mask: linear-gradient(#fff 0 0) content-box exclude, linear-gradient(#fff 0 0);
    animation: rainbow-flow 4s ease-in-out infinite alternate;
}
@keyframes run { from { background-position: -40% 0; } to { background-position: 140% 0; } }
@keyframes alarm { 50% { box-shadow: inset 3px 0 0 var(--failed), 0 0 0 2px var(--failed-soft); } }
@keyframes rainbow-flow { from { background-position: 0% 50%; } to { background-position: 100% 50%; } }
.legend { display: flex; flex-wrap: wrap; gap: 6px 14px; font-size: 12px; color: var(--text-muted); margin: -2px 0 10px; }
.legend span b { font-weight: 700; margin-right: 4px; }
.freshness { font-size: 12px; color: var(--text-faint); margin-left: 8px; font-weight: 400; text-transform: none; letter-spacing: 0; }
@keyframes drift { to { background-position: 16px 0; } }
@keyframes flare { from { box-shadow: 0 0 0 3px var(--failed); } to { box-shadow: 0 0 0 0 transparent; } }
@media (prefers-reduced-motion: reduce) { *, *::after { animation: none !important; transition: none !important; } }
@media (max-width: 640px) {
    .pulse { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    .stat .value { font-size: 22px; }
    .run { padding: 12px; }
    .run .open { width: 100%; margin-top: 4px; }
    .line { grid-template-columns: 18px minmax(0, 1fr) auto; margin-left: 10px; gap: 8px; }
    .line .slot-label { display: none; }
}
</style>
</head>
<body>
<main>
<div class="top">
    <div class="brand"><span class="mark" aria-hidden="true"></span>Loom</div>
    <span class="connection" id="connection" data-state="connecting">connecting</span>
</div>
<section class="pulse" id="pulse" aria-label="Right now"></section>
<h2>Machines<span class="freshness" id="freshness"></span></h2>
<div class="legend"><span><b>\u25B6</b>gating</span><span><b>\u2713</b>green</span><span><b>\u2715</b>red</span><span><b>\u26A0</b>tool crash</span><span><b>\u00B7</b>idle</span><span><b>\u25C6</b>Loom unit</span><span>rainbow: the star</span></div>
<section class="fabric" id="fabric"></section>
<h2>Runs</h2>
<section class="runs" id="runs"></section>
<h2 id="history-title" hidden>Finished</h2>
<section class="history" id="history"></section>
</main>
<script nonce="${nonce}">
(function () {
    'use strict';
    var token = decodeURIComponent(location.hash.slice(1));
    var runs = new Map();
    var machines = [];
    var gate = null;
    var pulse = null;
    var seenFailures = new Set();
    var firstPaint = true;
    var attempt = 0;
    var frameRequested = false;
    var keepAlive = null;
    var symbols = { running: '\\u25B6', green: '\\u2713', red: '\\u2715', void: '\\u25CC' };
    var words = { running: 'running', green: 'green', red: 'red', void: 'void: proved nothing' };

    function element(tag, className, text) {
        var node = document.createElement(tag);
        if (className) { node.className = className; }
        if (text !== undefined && text !== null) { node.textContent = String(text); }
        return node;
    }

    function duration(seconds) {
        seconds = Math.max(0, Math.floor(seconds));
        var hours = Math.floor(seconds / 3600);
        var minutes = Math.floor((seconds % 3600) / 60);
        var rest = seconds % 60;
        if (hours > 0) { return hours + 'h ' + String(minutes).padStart(2, '0') + 'm'; }
        if (minutes > 0) { return minutes + 'm ' + String(rest).padStart(2, '0') + 's'; }
        return rest + 's';
    }

    function since(time) {
        var at = Date.parse(time);
        return isNaN(at) ? '' : duration((Date.now() - at) / 1000);
    }

    function stateOf(run) {
        return run.verdict || 'running';
    }

    function badge(state) {
        var node = element('span', 'badge');
        node.dataset.state = state;
        node.appendChild(element('span', null, symbols[state]));
        node.appendChild(element('span', null, words[state]));
        return node;
    }

    function renderPulse() {
        var section = document.getElementById('pulse');
        section.replaceChildren();
        var values = pulse || { running: 0, finishedLastMinute: 0, queued: 0, longest: null };
        var stats = [
            ['running', values.running, 'units running now', null],
            ['', values.finishedLastMinute, 'finished in the last minute', null],
            ['', values.queued, 'waiting for a slot', null],
            ['longest', values.longest ? since(values.longest.since) : '\\u2013', 'longest running',
                values.longest ? values.longest.unit + ' on ' + values.longest.machine : 'nothing running'],
        ];
        stats.forEach(function (stat) {
            var node = element('div', 'stat ' + stat[0]);
            node.appendChild(element('div', 'value', stat[1]));
            node.appendChild(element('div', 'label', stat[2]));
            if (stat[3]) { node.appendChild(element('div', 'detail', stat[3])); }
            section.appendChild(node);
        });
    }

    var glyphs = { gating: '\u25B6', green: '\u2713', red: '\u2715', crash: '\u26A0', idle: '\u00B7', loom: '\u25C6' };

    // Every machine the gate names, one line per slot, and every Loom unit running on a machine, merged by name.
    function renderFabric() {
        var section = document.getElementById('fabric');
        section.replaceChildren();
        var boxes = (gate && gate.machines ? gate.machines : []).map(function (machine) {
            return { name: machine.name, aliases: machine.aliases || [], cores: machine.cores, lines: machine.lines.slice() };
        });
        function boxFor(name) {
            var lower = name.toLowerCase();
            var found = boxes.find(function (box) {
                return box.name.toLowerCase() === lower || box.aliases.some(function (alias) { return alias.toLowerCase() === lower; });
            });
            if (!found) {
                var posted = machines.find(function (machine) { return machine.name.toLowerCase() === lower; });
                found = { name: name, aliases: [], cores: posted ? posted.cores : 0, lines: [] };
                boxes.push(found);
            }
            return found;
        }
        runs.forEach(function (run) {
            if (run.verdict) { return; }
            (run.active || []).forEach(function (active) {
                var box = boxFor(active.machine);
                // A Loom unit takes the machine's idle line if it has one, so the line count stays the slot count.
                box.lines = box.lines.filter(function (line) { return !(line.slot === 0 && line.state === 'idle'); });
                box.lines.push({ slot: -1, class: 'loom', state: 'loom', kind: 'loom', branch: run.job, sha: active.unit, step: run.run, since: active.since, star: false, detail: '' });
            });
        });
        if (boxes.length === 0) {
            section.appendChild(element('div', 'empty', 'Waiting for the gate lines. They come from loom gate-lines on Kirk\\u2019s Mac.'));
            return;
        }
        var freshness = document.getElementById('freshness');
        freshness.dataset.from = gate ? gate.at : '';
        boxes.forEach(function (box) {
            var node = element('div', 'box');
            var head = element('div', 'box-head');
            head.appendChild(element('span', 'box-name', box.name));
            var meta = (box.cores ? box.cores + ' cores' : '') + (box.aliases.length ? '  also ' + box.aliases.join(', ') : '');
            head.appendChild(element('span', 'box-meta', meta));
            var busy = box.lines.filter(function (line) { return line.state !== 'idle'; }).length;
            var slots = box.lines.filter(function (line) { return line.slot !== 0; }).length;
            head.appendChild(element('span', 'box-busy', slots ? busy + ' of ' + slots + ' busy' : 'idle'));
            node.appendChild(head);
            box.lines.forEach(function (line) {
                var row = element('div', 'line');
                row.dataset.state = line.state;
                row.dataset.kind = line.kind || '';
                row.dataset.star = line.star ? 'yes' : 'no';
                row.dataset.live = line.since ? 'yes' : 'no';
                row.appendChild(element('span', 'glyph', glyphs[line.state] || '?'));
                var label = line.slot > 0 ? 'slot ' + line.slot + (line.class ? ' \u00B7 ' + line.class : '') : (line.slot < 0 ? 'loom' : '');
                row.appendChild(element('span', 'slot-label', label));
                var what = element('span', 'what');
                what.title = line.state;
                if (line.state === 'idle' && !line.branch) {
                    what.appendChild(document.createTextNode(line.detail || 'idle'));
                }
                else {
                    what.appendChild(element('b', null, line.branch));
                    what.appendChild(document.createTextNode(' '));
                    what.appendChild(element('span', 'sha', line.sha));
                    what.appendChild(document.createTextNode(' '));
                    var step = line.kind === 'loom' ? line.step : (line.class ? line.class + ' \u00B7 ' : '') + line.step;
                    what.appendChild(element('span', 'step', '\u00B7 ' + step + ' \u00B7 ' + line.state));
                    if (line.detail && line.state !== 'gating') { what.appendChild(element('span', 'detail', line.detail)); }
                }
                row.appendChild(what);
                var time = element('span', 'time', '');
                if (line.since) { time.dataset.from = line.since; }
                row.appendChild(time);
                node.appendChild(row);
            });
            section.appendChild(node);
        });
    }

    function openRun(run) {
        fetch('/board/runs/' + encodeURIComponent(run) + '/viewer', { method: 'POST', headers: { Authorization: 'Bearer ' + token } })
            .then(function (response) { return response.ok ? response.json() : Promise.reject(response.status); })
            .then(function (body) { window.open('/runs/' + encodeURIComponent(run) + '?token=' + encodeURIComponent(body.token), '_blank', 'noopener'); })
            .catch(function () { setConnection('reconnecting', 'could not open ' + run); });
    }

    function renderRun(run) {
        var state = stateOf(run);
        var node = element('article', 'run');
        node.dataset.state = state;
        var head = element('div', 'head');
        head.appendChild(element('span', 'job', run.job));
        head.appendChild(element('span', 'id', run.run));
        head.appendChild(element('span', 'spacer'));
        var elapsed = element('span', 'elapsed', '');
        elapsed.dataset.from = run.plannedAt;
        if (run.verdict) { elapsed.dataset.to = run.updatedAt; }
        head.appendChild(elapsed);
        head.appendChild(badge(state));
        var open = element('button', 'open', 'Open run');
        open.addEventListener('click', function () { openRun(run.run); });
        head.appendChild(open);
        node.appendChild(head);

        var bar = element('div', 'bar');
        bar.setAttribute('role', 'img');
        bar.setAttribute('aria-label', run.passed + ' passed, ' + run.failed + ' failed, ' + run.broken + ' broken, ' + run.running + ' running, ' + run.queued + ' queued');
        ['passed', 'failed', 'broken', 'running'].forEach(function (kind) {
            if (run[kind] > 0) {
                var part = element('span', kind);
                part.style.flexGrow = String(run[kind]);
                bar.appendChild(part);
            }
        });
        if (run.queued > 0) {
            var rest = element('span');
            rest.style.flexGrow = String(run.queued);
            bar.appendChild(rest);
        }
        node.appendChild(bar);

        var counts = element('div', 'counts');
        [['passed', '\\u2713 '], ['failed', '\\u2715 '], ['broken', '\\u25CC '], ['running', '\\u25B6 '], ['queued', '\\u2026 ']].forEach(function (pair) {
            var item = element('span');
            item.appendChild(element('b', null, pair[1] + run[pair[0]]));
            item.appendChild(document.createTextNode(' ' + pair[0]));
            counts.appendChild(item);
        });
        if (run.cached > 0) { counts.appendChild(element('span', null, run.cached + ' from the cache')); }
        counts.appendChild(element('span', null, run.units + ' units'));
        node.appendChild(counts);

        if (run.active && run.active.length > 0) {
            var threads = element('div', 'threads');
            var shown = run.active.slice().sort(function (left, right) { return Date.parse(left.since) - Date.parse(right.since); });
            shown.slice(0, 24).forEach(function (active) {
                var thread = element('span', 'thread');
                thread.appendChild(element('span', 'unit', active.unit));
                thread.appendChild(element('span', 'where', active.machine));
                var time = element('span', 'time', '');
                time.dataset.from = active.since;
                thread.appendChild(time);
                threads.appendChild(thread);
            });
            if (shown.length > 24) { threads.appendChild(element('span', 'more', 'and ' + (shown.length - 24) + ' more')); }
            node.appendChild(threads);
        }

        if (run.failures && run.failures.length > 0) {
            var failures = element('div', 'failures');
            run.failures.slice(0, 3).forEach(function (failure) {
                var key = run.run + ' ' + failure.unit + ' ' + failure.at;
                var item = element('div', 'failure' + (!firstPaint && !seenFailures.has(key) ? ' fresh' : ''));
                seenFailures.add(key);
                item.dataset.status = failure.status;
                var what = element('div', 'what');
                what.appendChild(element('span', null, failure.status === 'broken' ? '\\u25CC' : '\\u2715'));
                what.appendChild(element('span', null, failure.unit + ' ' + failure.status));
                item.appendChild(what);
                if (failure.lines && failure.lines.length > 0) { item.appendChild(element('pre', null, failure.lines.join('\\n'))); }
                failures.appendChild(item);
            });
            if (run.failures.length > 3) { failures.appendChild(element('span', 'more', 'and ' + (run.failures.length - 3) + ' more failures; open the run to see them')); }
            node.appendChild(failures);
        }
        return node;
    }

    function renderRuns() {
        var live = [];
        var finished = [];
        runs.forEach(function (run) { (run.verdict ? finished : live).push(run); });
        live.sort(function (left, right) { return Date.parse(right.plannedAt) - Date.parse(left.plannedAt); });
        finished.sort(function (left, right) { return Date.parse(right.updatedAt) - Date.parse(left.updatedAt); });
        // The newest finished runs stay as full bands for a while; older ones fold into the history strip.
        var recent = finished.filter(function (run) { return Date.now() - Date.parse(run.updatedAt) < 15 * 60 * 1000; });
        var older = finished.filter(function (run) { return recent.indexOf(run) < 0; });
        var section = document.getElementById('runs');
        section.replaceChildren();
        live.concat(recent).forEach(function (run) { section.appendChild(renderRun(run)); });
        if (live.length + recent.length === 0) {
            var empty = element('div', 'empty');
            empty.appendChild(document.createTextNode('Nothing running. Start a run with '));
            empty.appendChild(element('code', null, 'loom run job.json'));
            empty.appendChild(document.createTextNode(' and it appears here.'));
            section.appendChild(empty);
        }
        var history = document.getElementById('history');
        history.replaceChildren();
        document.getElementById('history-title').hidden = older.length === 0;
        older.forEach(function (run) {
            var row = element('div', 'past');
            row.appendChild(badge(run.verdict));
            row.appendChild(element('span', 'job', run.job));
            row.appendChild(element('span', 'id', run.run));
            row.appendChild(element('span', 'tally', run.passed + '/' + run.units + ' passed, ' + duration((Date.parse(run.updatedAt) - Date.parse(run.plannedAt)) / 1000)));
            row.addEventListener('click', function () { openRun(run.run); });
            history.appendChild(row);
        });
    }

    function tick() {
        document.querySelectorAll('[data-from]').forEach(function (node) {
            var from = Date.parse(node.dataset.from);
            var to = node.dataset.to ? Date.parse(node.dataset.to) : Date.now();
            if (!isNaN(from)) { node.textContent = duration((to - from) / 1000); }
        });
        if (pulse && pulse.longest) {
            var value = document.querySelector('.stat.longest .value');
            if (value) { value.textContent = since(pulse.longest.since); }
        }
    }

    function render() {
        frameRequested = false;
        renderPulse();
        renderFabric();
        renderRuns();
        tick();
        firstPaint = false;
    }

    function schedule() {
        if (!frameRequested) {
            frameRequested = true;
            requestAnimationFrame(render);
        }
    }

    function setConnection(state, text) {
        var node = document.getElementById('connection');
        node.dataset.state = state;
        node.textContent = text;
    }

    function connect() {
        if (!token) {
            setConnection('reconnecting', 'no board token');
            var section = document.getElementById('runs');
            section.replaceChildren();
            var empty = element('div', 'empty');
            empty.appendChild(document.createTextNode('This page needs its board token after the #. Get the address with '));
            empty.appendChild(element('code', null, 'loom board'));
            empty.appendChild(document.createTextNode('.'));
            section.appendChild(empty);
            return;
        }
        var opened = false;
        var socket;
        try {
            var scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
            socket = new WebSocket(scheme + '//' + location.host + '/board/stream', ['loom', 'token.' + token]);
        }
        catch (error) {
            poll('stream refused: ' + error.message);
            return;
        }
        // A browser that never finishes the handshake (one stalled here on Oct 8) falls back to the snapshot.
        var stalled = setTimeout(function () {
            if (!opened) {
                socket.close();
                poll('stream stalled');
            }
        }, 5000);
        socket.addEventListener('open', function () { opened = true; clearTimeout(stalled); attempt = 0; setConnection('live', 'live'); });
        socket.addEventListener('message', function (message) {
            var frame;
            try { frame = JSON.parse(message.data); } catch (error) { return; }
            apply(frame);
        });
        socket.addEventListener('close', function () {
            clearTimeout(stalled);
            if (!opened || polling) {
                if (!polling) { poll('stream closed'); }
                return;
            }
            attempt++;
            setConnection('reconnecting', 'reconnecting');
            setTimeout(connect, Math.min(15000, 500 * Math.pow(2, attempt)));
        });
        clearInterval(keepAlive);
        keepAlive = setInterval(function () { if (socket.readyState === 1) { socket.send('ping'); } }, 30000);
    }

    function apply(frame) {
        if (frame.kind === 'snapshot') {
            runs = new Map();
            (frame.runs || []).forEach(function (run) { runs.set(run.run, run); });
            machines = frame.machines || [];
            pulse = frame.pulse || null;
            gate = frame.gate || null;
        }
        else if (frame.kind === 'gate') { gate = frame.gate; }
        else if (frame.kind === 'run' && frame.run) { runs.set(frame.run.run, frame.run); }
        else if (frame.kind === 'machines') { machines = frame.machines || []; }
        else if (frame.kind === 'pulse') { pulse = frame; }
        schedule();
    }

    // The fallback: the same snapshot over plain HTTPS every two seconds.
    var polling = false;
    function poll(why) {
        if (polling) { return; }
        polling = true;
        var failures = 0;
        function once() {
            fetch('/board/snapshot', { headers: { Authorization: 'Bearer ' + token }, cache: 'no-store' })
                .then(function (response) { return response.ok ? response.json() : Promise.reject(new Error('the snapshot answered ' + response.status)); })
                .then(function (snapshot) {
                    failures = 0;
                    snapshot.kind = 'snapshot';
                    apply(snapshot);
                    setConnection('live', 'live, every 2 s (' + why + ')');
                })
                .catch(function (error) {
                    failures++;
                    setConnection('reconnecting', error.message);
                })
                .finally(function () { setTimeout(once, failures > 0 ? Math.min(15000, 2000 * failures) : 2000); });
        }
        once();
    }

    renderPulse();
    setInterval(tick, 1000);
    connect();
})();
</script>
</body>
</html>
`;
}
