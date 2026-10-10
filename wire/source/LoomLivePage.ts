// Loom Live: loom-pipeline's board, the cycle as it happens (designed with Kirk on claude.ai, Oct 10; #9v317cf). It
// reads only the ChangeBoard's projection: a snapshot and then each change as Queue pushes it, over the stream at
// /board/stream, or the same snapshot every two seconds when the stream can't open. Plain HTML and inline script under
// the response's CSP nonce, nothing loaded from elsewhere. The board token rides after the # and goes to the stream
// only as a subprotocol. A landing gets a celebration and, once the viewer turns sound on, a chime made in the page.

import { loomMark } from './LoomMark';

export function renderLoomLivePage(nonce: string): string {
    return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Loom</title>
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<style nonce="${nonce}">
:root {
    color-scheme: dark;
    --ground: #0d0b1f; --surface: #15122e; --sunk: #1d1a36; --border: #2a2550; --text: #f4ecd8; --muted: #b8afd6;
    --gold: #f2c14e; --cream: #fff4d6; --violet: #b69cff; --running: #79a6ff; --passed: #4fd68a; --failed: #ff7a70; --void: #f0b34e;
    --mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
* { box-sizing: border-box; }
html, body { margin: 0; }
body { background: var(--ground); color: var(--text); font: 14px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif; overflow-x: hidden; }
main { max-width: 1920px; margin: 0 auto; padding: 18px 24px 28px; display: flex; flex-direction: column; gap: 16px; min-height: 100vh; }
header { display: flex; align-items: center; gap: 14px; flex-wrap: wrap; }
.brand { display: flex; align-items: center; gap: 10px; font-weight: 700; font-size: 20px; letter-spacing: .01em; }
.connection { font-size: 12px; color: var(--muted); display: inline-flex; align-items: center; gap: 6px; }
.connection::before { content: ""; width: 8px; height: 8px; border-radius: 50%; background: #4a4570; }
.connection[data-state="live"]::before { background: var(--passed); box-shadow: 0 0 0 3px rgba(79,214,138,.18); }
.connection[data-state="reconnecting"]::before { background: var(--void); }
.controls { margin-left: auto; display: flex; gap: 8px; }
button.toggle { font: 600 13px/1 system-ui, sans-serif; color: var(--text); background: var(--sunk); border: 1px solid var(--border); border-radius: 10px; padding: 0 14px; min-height: 44px; cursor: pointer; }
button.toggle[aria-pressed="true"] { border-color: var(--gold); color: var(--gold); }
.label { font: 600 11px/1 system-ui, sans-serif; letter-spacing: .1em; text-transform: uppercase; color: var(--muted); }
.mono { font-family: var(--mono); font-variant-numeric: tabular-nums; }
.track { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 10px; }
.step { display: flex; flex-direction: column; gap: 6px; padding: 12px 14px; border-radius: 12px; border: 1px solid var(--border); background: #120f29; min-width: 0; transition: all .4s; }
.step .head { display: flex; align-items: center; gap: 8px; font-weight: 650; }
.step .dot { width: 10px; height: 10px; border-radius: 50%; background: #3a3466; flex: none; }
.step .sub { font-size: 12px; color: var(--muted); }
.step[data-state="done"] .dot { background: var(--passed); }
.step[data-state="active"] { border-color: var(--gold); background: #231b3a; }
.step[data-state="active"] .dot { background: var(--gold); box-shadow: 0 0 0 4px rgba(242,193,78,.25); animation: beat 1s ease-in-out infinite; }
.step[data-state="red"] { border-color: var(--failed); background: #2a1420; }
.step[data-state="red"] .dot { background: var(--failed); }
.cols { display: grid; grid-template-columns: minmax(0, 3fr) minmax(0, 6fr) minmax(0, 3fr); gap: 14px; flex: 1; align-items: start; }
.panel { background: var(--surface); border: 1px solid var(--border); border-radius: 14px; padding: 16px 18px; min-width: 0; display: flex; flex-direction: column; gap: 12px; }
.stack { display: flex; flex-direction: column; gap: 8px; }
.foot { margin-top: auto; padding-top: 12px; border-top: 1px solid var(--border); display: flex; flex-direction: column; gap: 6px; }
.item { display: flex; flex-direction: column; gap: 2px; padding: 10px 12px; border-radius: 10px; background: var(--sunk); border: 1px solid var(--border); }
.item.new { animation: arrive .5s ease-out both; }
.item .top { display: flex; gap: 8px; align-items: baseline; }
.item b { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.item .meta { font-family: var(--mono); font-size: 12px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.chip { margin-left: auto; font: 600 10px/1 system-ui, sans-serif; letter-spacing: .06em; text-transform: uppercase; padding: 4px 7px; border-radius: 999px; background: #2a2550; color: var(--muted); white-space: nowrap; }
.chip[data-state="testing"], .chip[data-state="building"] { color: var(--running); }
.chip[data-state="landed"] { color: var(--passed); }
.chip[data-state="red"] { color: var(--failed); }
.chip[data-state="parked"], .chip[data-state="refused"] { color: var(--void); }
.empty { color: var(--muted); font-size: 13px; }
.focus-head { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; }
.focus-head b { font-size: 22px; }
.tiles { display: grid; grid-template-columns: repeat(auto-fill, minmax(18px, 1fr)); gap: 5px; }
.tile { aspect-ratio: 1; border-radius: 4px; border: 1px solid #2f2a58; background: #120f29; transition: background .3s, border-color .3s; }
.tile[data-state="passed"] { background: #2f9e5c; border-color: var(--passed); }
.tile[data-state="failed"] { background: #c8261d; border-color: var(--failed); }
.tile[data-state="void"] { background: #6b4a12; border-color: var(--void); }
.counts { display: flex; gap: 14px; flex-wrap: wrap; font-family: var(--mono); font-size: 13px; color: var(--muted); }
.counts b { color: var(--text); }
.big { font: 700 30px/1 var(--mono); }
.landed-row { display: grid; grid-template-columns: 12px minmax(0, 1fr) auto; gap: 10px; align-items: center; font-size: 13px; }
.landed-row .pip { width: 10px; height: 10px; border-radius: 50%; background: var(--passed); }
.landed-row span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.overlay { position: fixed; inset: 0; display: grid; place-items: center; background: radial-gradient(circle at 50% 42%, rgba(242,193,78,.22), rgba(13,11,31,.86) 62%); pointer-events: none; z-index: 10; animation: fade 5.2s ease-in-out both; }
.overlay .card { display: flex; flex-direction: column; align-items: center; gap: 14px; text-align: center; padding: 32px; animation: burst 1.2s ease-out both; }
.overlay .word { font: 800 76px/1 system-ui, sans-serif; color: var(--gold); letter-spacing: -.02em; text-shadow: 0 0 40px rgba(242,193,78,.5); }
.overlay .who { font-size: 20px; }
.overlay .what { font-family: var(--mono); font-size: 16px; color: var(--muted); }
.piece { position: fixed; top: -24px; width: 12px; height: 12px; border-radius: 2px; pointer-events: none; z-index: 11; animation-name: fall; animation-timing-function: cubic-bezier(.3,.6,.5,1); animation-fill-mode: both; }
.piece.round { border-radius: 50%; }
.piece.thread { width: 3px; height: 22px; border-radius: 2px; }
@keyframes beat { 50% { opacity: .55; } }
@keyframes arrive { from { opacity: 0; transform: translateX(-12px); } to { opacity: 1; transform: none; } }
@keyframes fall { 0% { transform: translateY(0) rotate(0); } 100% { transform: translateY(110vh) rotate(720deg); } }
@keyframes burst { 0% { transform: scale(.6); opacity: 0; } 30% { transform: scale(1.06); opacity: 1; } 100% { transform: scale(1); opacity: 1; } }
@keyframes fade { 0% { opacity: 0; } 8% { opacity: 1; } 85% { opacity: 1; } 100% { opacity: 0; } }
@media (prefers-reduced-motion: reduce) { *, *::before, *::after { animation: none !important; transition: none !important; } .piece { display: none; } }
@media (max-width: 960px) { .cols { grid-template-columns: 1fr; } .track { grid-template-columns: repeat(3, minmax(0, 1fr)); } .overlay .word { font-size: 52px; } }
</style>
</head>
<body>
<main>
<header>
    <div class="brand">${loomMark(30)}Loom</div>
    <span class="connection" id="connection" data-state="connecting">connecting</span>
    <div class="controls">
        <button type="button" class="toggle" id="sound" aria-pressed="false">Sound off</button>
        <button type="button" class="toggle" id="hear">Hear a landing</button>
    </div>
</header>
<section class="track" id="track" aria-label="Where the change is"></section>
<section class="cols">
    <article class="panel"><span class="label">On their way</span><div id="waiting" class="stack"></div></article>
    <article class="panel" id="focus" aria-live="polite"></article>
    <article class="panel">
        <span class="label">Landed today</span>
        <div id="landed" class="stack"></div>
        <div class="foot">
            <span class="label">Arrival to main, last landing</span>
            <span class="big" id="clock">&ndash;</span>
        </div>
    </article>
</section>
</main>
<script nonce="${nonce}">
(function () {
    'use strict';
    var token = decodeURIComponent(location.hash.slice(1));
    var changes = new Map();
    var focusId = null;
    // Changes this page has drawn, so only a newcomer slides in and a redraw never replays the arrival.
    var drawn = new Set();
    var frameRequested = false;
    var attempt = 0;
    var polling = false;
    var audio = null;
    var soundOn = false;
    try { soundOn = localStorage.getItem('loom.sound') === 'on'; } catch (error) { soundOn = false; }
    var live = ['queued', 'building', 'testing'];
    var stages = [['Posted', 'submitted to the line'], ['Build', 'products, once each'], ['Test', 'only what changed'], ['Verdict', 'by written rule'], ['Landed', 'main moves']];

    function element(tag, className, text) {
        var node = document.createElement(tag);
        if (className) { node.className = className; }
        if (text !== undefined && text !== null) { node.textContent = String(text); }
        return node;
    }

    function duration(seconds) {
        seconds = Math.max(0, Math.round(seconds));
        if (seconds >= 3600) { return Math.floor(seconds / 3600) + 'h ' + String(Math.floor((seconds % 3600) / 60)).padStart(2, '0') + 'm'; }
        return Math.floor(seconds / 60) + ':' + String(seconds % 60).padStart(2, '0');
    }

    function took(line) {
        var from = Date.parse(line.firstSeenAt);
        var to = line.finishedAt ? Date.parse(line.finishedAt) : Date.now();
        return isNaN(from) ? '' : duration((to - from) / 1000);
    }

    function sorted(filter) {
        return Array.from(changes.values()).filter(filter);
    }

    // The change the page centres on: the last one that moved while on its way, else the last that finished.
    function focusLine() {
        var moving = sorted(function (line) { return live.indexOf(line.state) >= 0; });
        moving.sort(function (left, right) { return Date.parse(right.updatedAt) - Date.parse(left.updatedAt); });
        if (moving.length > 0) { return moving[0]; }
        var all = sorted(function () { return true; });
        all.sort(function (left, right) { return Date.parse(right.updatedAt) - Date.parse(left.updatedAt); });
        return focusId && changes.get(focusId) ? changes.get(focusId) : (all[0] || null);
    }

    function stageOf(line) {
        if (!line) { return -1; }
        var decided = line.units.planned > 0 && line.units.passed + line.units.failed + line.units.void >= line.units.planned;
        return { queued: 0, building: 1, testing: decided ? 3 : 2, landed: 4, red: 4, parked: 0, refused: 0 }[line.state];
    }

    function renderTrack(line) {
        var section = document.getElementById('track');
        section.replaceChildren();
        var current = stageOf(line);
        stages.forEach(function (stage, index) {
            var step = element('div', 'step');
            var red = line && line.state === 'red' && index >= 3;
            step.dataset.state = red ? 'red' : index < current || (line && line.state === 'landed') ? 'done' : index === current ? 'active' : '';
            var head = element('div', 'head');
            head.appendChild(element('span', 'dot'));
            head.appendChild(element('span', null, index === 4 && line && line.state === 'red' ? 'Red' : stage[0]));
            step.appendChild(head);
            step.appendChild(element('span', 'sub', stage[1]));
            section.appendChild(step);
        });
    }

    function renderFocus(line) {
        var panel = document.getElementById('focus');
        panel.replaceChildren();
        panel.appendChild(element('span', 'label', 'Now'));
        if (!line) {
            panel.appendChild(element('span', 'empty', 'No change on its way to main. Submit one with POST /changes and it appears here.'));
            return;
        }
        var head = element('div', 'focus-head');
        head.appendChild(element('b', null, line.owner));
        head.appendChild(element('span', 'mono', line.sha.slice(0, 12) + (line.future ? ' on ' + line.future.slice(0, 12) : '')));
        var chip = element('span', 'chip', line.state);
        chip.dataset.state = line.state;
        head.appendChild(chip);
        panel.appendChild(head);
        var units = line.units;
        var counts = element('div', 'counts');
        [['passed', units.passed], ['failed', units.failed], ['void', units.void], ['waiting', Math.max(0, units.planned - units.passed - units.failed - units.void)]].forEach(function (pair) {
            var item = element('span');
            item.appendChild(element('b', null, pair[1]));
            item.appendChild(document.createTextNode(' ' + pair[0]));
            counts.appendChild(item);
        });
        counts.appendChild(element('span', null, units.planned + ' units planned'));
        panel.appendChild(counts);
        var tiles = element('div', 'tiles');
        tiles.setAttribute('role', 'img');
        tiles.setAttribute('aria-label', units.passed + ' passed, ' + units.failed + ' failed, ' + units.void + ' void of ' + units.planned);
        var shown = Math.min(units.planned, 400);
        for (var index = 0; index < shown; index++) {
            var tile = element('span', 'tile');
            tile.dataset.state = index < units.passed ? 'passed' : index < units.passed + units.failed ? 'failed' : index < units.passed + units.failed + units.void ? 'void' : '';
            tiles.appendChild(tile);
        }
        panel.appendChild(tiles);
        if (units.planned === 0) { panel.appendChild(element('span', 'empty', 'No units planned yet.')); }
        var since = element('span', 'mono', '');
        since.dataset.from = line.firstSeenAt;
        if (line.finishedAt) { since.dataset.to = line.finishedAt; }
        var row = element('div', 'counts');
        row.appendChild(document.createTextNode('on the board for '));
        row.appendChild(since);
        panel.appendChild(row);
    }

    function renderLists() {
        var waiting = document.getElementById('waiting');
        waiting.replaceChildren();
        var moving = sorted(function (line) { return live.indexOf(line.state) >= 0 || line.state === 'parked'; });
        moving.sort(function (left, right) { return Date.parse(left.firstSeenAt) - Date.parse(right.firstSeenAt); });
        if (moving.length === 0) { waiting.appendChild(element('span', 'empty', 'Nothing in the line.')); }
        moving.forEach(function (line) {
            var item = element('div', drawn.has(line.change) ? 'item' : 'item new');
            drawn.add(line.change);
            var top = element('div', 'top');
            top.appendChild(element('b', null, line.owner));
            var chip = element('span', 'chip', line.state);
            chip.dataset.state = line.state;
            top.appendChild(chip);
            item.appendChild(top);
            item.appendChild(element('span', 'meta', line.sha.slice(0, 12) + ' \\u00B7 ' + line.units.planned + ' units'));
            waiting.appendChild(item);
        });
        var landed = document.getElementById('landed');
        landed.replaceChildren();
        var done = sorted(function (line) { return line.state === 'landed'; });
        done.sort(function (left, right) { return Date.parse(right.finishedAt || right.updatedAt) - Date.parse(left.finishedAt || left.updatedAt); });
        if (done.length === 0) { landed.appendChild(element('span', 'empty', 'Nothing has landed yet today.')); }
        done.slice(0, 12).forEach(function (line) {
            var row = element('div', 'landed-row');
            row.appendChild(element('span', 'pip'));
            var who = element('span');
            who.appendChild(element('b', null, line.owner));
            who.appendChild(document.createTextNode(' '));
            who.appendChild(element('span', 'mono', line.sha.slice(0, 10)));
            row.appendChild(who);
            row.appendChild(element('span', 'mono', took(line)));
            landed.appendChild(row);
        });
        document.getElementById('clock').textContent = done.length > 0 ? took(done[0]) : '\\u2013';
    }

    function render() {
        frameRequested = false;
        var line = focusLine();
        renderTrack(line);
        renderFocus(line);
        renderLists();
        tick();
    }

    function schedule() {
        if (!frameRequested) { frameRequested = true; requestAnimationFrame(render); }
    }

    function tick() {
        document.querySelectorAll('[data-from]').forEach(function (node) {
            var from = Date.parse(node.dataset.from);
            var to = node.dataset.to ? Date.parse(node.dataset.to) : Date.now();
            if (!isNaN(from)) { node.textContent = duration((to - from) / 1000); }
        });
    }

    // ---------- The landing ----------

    function chime() {
        if (!soundOn || !audio) { return; }
        var now = audio.currentTime + 0.02;
        var master = audio.createGain();
        master.gain.value = 0.5;
        master.connect(audio.destination);
        // A rising major arpeggio, a fifth above to finish, each note a soft sine with a long tail.
        [523.25, 659.25, 783.99, 1046.5, 1567.98].forEach(function (frequency, index) {
            var start = now + index * 0.11;
            var oscillator = audio.createOscillator();
            var gain = audio.createGain();
            oscillator.type = index === 4 ? 'triangle' : 'sine';
            oscillator.frequency.value = frequency;
            gain.gain.setValueAtTime(0.0001, start);
            gain.gain.exponentialRampToValueAtTime(index === 4 ? 0.08 : 0.2, start + 0.03);
            gain.gain.exponentialRampToValueAtTime(0.0001, start + 1.6);
            oscillator.connect(gain);
            gain.connect(master);
            oscillator.start(start);
            oscillator.stop(start + 1.7);
        });
    }

    function celebrate(line, test) {
        chime();
        var overlay = element('div', 'overlay');
        overlay.setAttribute('role', 'status');
        var card = element('div', 'card');
        var mark = element('span');
        mark.innerHTML = ${JSON.stringify(loomMark(104))};
        card.appendChild(mark);
        card.appendChild(element('span', 'word', 'Landed'));
        card.appendChild(element('span', 'who', test ? 'A test landing: this is what main moving looks like' : line.owner + '\\u2019s change is on main'));
        if (!test) { card.appendChild(element('span', 'what', line.sha.slice(0, 12) + ' \\u00B7 ' + line.units.passed + ' of ' + line.units.planned + ' passed \\u00B7 ' + took(line) + ' from arrival')); }
        overlay.appendChild(card);
        document.body.appendChild(overlay);
        var colors = ['#f2c14e', '#fff4d6', '#b69cff', '#ffd98a', '#4fd68a'];
        var pieces = [];
        for (var index = 0; index < 44; index++) {
            var piece = element('span', 'piece' + (index % 3 === 0 ? ' thread' : index % 3 === 1 ? ' round' : ''));
            piece.style.left = ((index * 37) % 100) + 'vw';
            piece.style.background = colors[index % colors.length];
            piece.style.animationDuration = (2.2 + (index % 5) * 0.35) + 's';
            piece.style.animationDelay = ((index % 11) * 0.08) + 's';
            document.body.appendChild(piece);
            pieces.push(piece);
        }
        setTimeout(function () {
            overlay.remove();
            pieces.forEach(function (piece) { piece.remove(); });
        }, 5400);
    }

    function setSound(on) {
        soundOn = on;
        try { localStorage.setItem('loom.sound', on ? 'on' : 'off'); } catch (error) { /* a private window keeps it for this page only */ }
        var button = document.getElementById('sound');
        button.setAttribute('aria-pressed', String(on));
        button.textContent = on ? 'Sound on' : 'Sound off';
        if (on) {
            // Browsers start audio only from a click, so the context is made, or woken, here.
            if (!audio) { audio = new (window.AudioContext || window.webkitAudioContext)(); }
            if (audio.state === 'suspended') { audio.resume(); }
        }
    }

    document.getElementById('sound').addEventListener('click', function () { setSound(!soundOn); });
    document.getElementById('hear').addEventListener('click', function () {
        if (!audio && soundOn) { setSound(true); }
        celebrate(null, true);
    });
    document.getElementById('sound').setAttribute('aria-pressed', String(soundOn));
    document.getElementById('sound').textContent = soundOn ? 'Sound on' : 'Sound off';

    // ---------- The feed ----------

    function take(line) {
        var before = changes.get(line.change);
        changes.set(line.change, line);
        if (before && before.state !== 'landed' && line.state === 'landed') {
            focusId = line.change;
            celebrate(line, false);
        }
    }

    function apply(frame) {
        if (frame.kind === 'snapshot') {
            changes = new Map();
            (frame.changes || []).forEach(function (line) { changes.set(line.change, line); });
        }
        else if (frame.kind === 'change' && frame.change) {
            take(frame.change);
        }
        schedule();
    }

    function setConnection(state, text) {
        var node = document.getElementById('connection');
        node.dataset.state = state;
        node.textContent = text;
    }

    function connect() {
        if (!token) {
            setConnection('reconnecting', 'this page needs its board token after the #');
            return;
        }
        var opened = false;
        var socket;
        try {
            socket = new WebSocket((location.protocol === 'https:' ? 'wss:' : 'ws:') + '//' + location.host + '/board/stream', ['loom', 'token.' + token]);
        }
        catch (error) {
            poll('stream refused');
            return;
        }
        var stalled = setTimeout(function () { if (!opened) { socket.close(); poll('stream stalled'); } }, 5000);
        socket.addEventListener('open', function () { opened = true; clearTimeout(stalled); attempt = 0; setConnection('live', 'live'); });
        socket.addEventListener('message', function (message) {
            var frame;
            try { frame = JSON.parse(message.data); } catch (error) { return; }
            apply(frame);
        });
        socket.addEventListener('close', function () {
            clearTimeout(stalled);
            if (!opened || polling) { if (!polling) { poll('stream closed'); } return; }
            attempt++;
            setConnection('reconnecting', 'reconnecting');
            setTimeout(connect, Math.min(15000, 500 * Math.pow(2, attempt)));
        });
        setInterval(function () { if (socket.readyState === 1) { socket.send('ping'); } }, 30000);
    }

    // The fallback: the same snapshot over plain HTTPS every two seconds, still celebrating what lands between reads.
    function poll(why) {
        if (polling) { return; }
        polling = true;
        var first = true;
        function once() {
            fetch('/board/changes', { headers: { Authorization: 'Bearer ' + token }, cache: 'no-store' })
                .then(function (response) { return response.ok ? response.json() : Promise.reject(new Error('the board answered ' + response.status)); })
                .then(function (body) {
                    if (first) { apply({ kind: 'snapshot', changes: body.changes }); first = false; }
                    else { (body.changes || []).forEach(take); schedule(); }
                    setConnection('live', 'live, every 2 s (' + why + ')');
                })
                .catch(function (error) { setConnection('reconnecting', error.message); })
                .finally(function () { setTimeout(once, 2000); });
        }
        once();
    }

    setInterval(tick, 1000);
    render();
    connect();
})();
</script>
</body>
</html>
`;
}
