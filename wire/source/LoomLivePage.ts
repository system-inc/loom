// Loom Live: loom-pipeline's board, the cycle as it happens, drawn to the design Kirk approved on claude.ai (Oct 10,
// "The cycle, live" on https://claude.ai/artifact/L7WZSJ5Lj2kBP3nPXtJVzw; #9v317cf). It reads only the ChangeBoard's
// projection: a snapshot and then each change as Queue pushes it, over the stream at /board/stream, or the same
// snapshot every two seconds when the stream can't open. Plain HTML and inline script under the response's CSP nonce,
// nothing loaded from elsewhere. The board token rides after the # once; the page keeps it in this browser and takes
// it out of the address bar, and it goes to the stream only as a subprotocol. Parts the log can't feed yet (the
// block, the build) keep their place and say what they wait on. A landing gets a celebration and, once the viewer
// turns sound on, a chime made in the page.

import { loomMark } from './LoomMark';

const celebrationGold = '#ffd166';

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
    --ground: #0b0c0e; --panel: #141519; --step: #121317; --card: #1a1b20; --border: #25272d; --edge: #2c2e35; --rule: #22242a;
    --text: #ecedef; --soft: #c9cbd1; --muted: #9b9ea6; --faint: #6b6e76; --off: #3a3c43;
    --violet: #b69cff; --violet-deep: #5b4a99; --running: #79a6ff; --passed: #4fd68a; --failed: #ff7a70; --void: #f0b34e; --gold: ${celebrationGold};
    --mono: ui-monospace, SFMono-Regular, Menlo, monospace;
}
* { box-sizing: border-box; }
html, body { margin: 0; }
body { background: var(--ground); color: var(--text); font: 14px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif; overflow-x: hidden; }
main { min-height: 100vh; padding: 18px 24px 28px; display: flex; flex-direction: column; gap: 16px; max-width: 1920px; margin: 0 auto; }
header { display: flex; align-items: center; gap: 16px; flex-wrap: wrap; }
.brand { display: flex; align-items: center; gap: 10px; font-weight: 700; font-size: 19px; }
.headline { font-size: 13px; color: var(--muted); }
.headline b { color: var(--text); }
.connection { font-size: 12px; color: var(--muted); border: 1px dashed var(--off); border-radius: 999px; padding: 4px 10px; display: inline-flex; align-items: center; gap: 6px; }
.connection::before { content: ""; width: 7px; height: 7px; border-radius: 50%; background: var(--off); }
.connection[data-state="live"]::before { background: var(--passed); }
.connection[data-state="reconnecting"]::before { background: var(--void); }
.controls { margin-left: auto; display: flex; gap: 8px; align-items: center; }
.controls .label { margin-right: 4px; }
.toggle { font: 600 13px/1 system-ui, sans-serif; color: var(--soft); background: #15161a; border: 1px solid var(--edge); border-radius: 10px; padding: 0 14px; min-height: 44px; cursor: pointer; }
.toggle[aria-pressed="true"] { background: #231b3d; border-color: var(--violet); color: var(--text); }
.label { font: 600 11px/1 system-ui, -apple-system, "Segoe UI", sans-serif; letter-spacing: .1em; text-transform: uppercase; color: var(--muted); }
.mono { font-family: var(--mono); font-variant-numeric: tabular-nums; }
.note { font-size: 12px; color: var(--muted); }
.track { display: grid; grid-template-columns: repeat(6, minmax(0, 1fr)); gap: 10px; }
.step { display: flex; flex-direction: column; gap: 6px; padding: 12px 14px; border-radius: 12px; border: 1px solid var(--border); background: var(--step); min-width: 0; transition: all .4s; }
.step .head { display: flex; align-items: center; gap: 8px; }
.step .name { font-weight: 650; }
.step .time { margin-left: auto; font-size: 12px; color: var(--muted); }
.step .dot { width: 10px; height: 10px; border-radius: 50%; background: var(--off); flex: none; }
.step[data-state="done"] .dot { background: var(--passed); }
.step[data-state="active"] { border-color: var(--violet); background: #1a1630; }
.step[data-state="active"] .dot { background: var(--violet); box-shadow: 0 0 0 4px rgba(182,156,255,.25); animation: beat 1s ease-in-out infinite; }
.step[data-state="red"] { border-color: var(--failed); background: #2a1414; }
.step[data-state="red"] .dot { background: var(--failed); }
.cols { display: grid; grid-template-columns: minmax(0, 3fr) minmax(0, 6fr) minmax(0, 3fr); gap: 14px; flex: 1; }
.middle { display: flex; flex-direction: column; gap: 14px; min-width: 0; }
.pair { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: 14px; }
.panel { background: var(--panel); border: 1px solid var(--border); border-radius: 14px; padding: 16px 18px; min-width: 0; display: flex; flex-direction: column; gap: 12px; position: relative; transition: border-color .4s, box-shadow .4s; }
.panel.on { border-color: var(--violet-deep); box-shadow: 0 0 0 1px var(--violet-deep), 0 0 32px rgba(182,156,255,.18); }
.panel.grow { flex: 1; }
.posted { display: flex; flex-direction: column; gap: 2px; padding: 10px 12px; border-radius: 10px; background: var(--card); border: 1px solid var(--edge); }
.posted.new { animation: arrive .5s ease-out both; }
.posted .top { display: flex; gap: 8px; align-items: baseline; }
.posted b { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.posted .place { margin-left: auto; font-size: 12px; color: var(--violet); }
.posted .meta { font-size: 12px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.list { display: flex; flex-direction: column; gap: 8px; }
.empty { color: var(--faint); font-size: 13px; }
.block { display: flex; align-items: center; gap: 18px; }
.block .words { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.block .title { font-size: 18px; }
.block .closes { font-size: 13px; color: var(--muted); }
.block .reason { font-size: 12px; color: var(--violet); }
.built { display: flex; align-items: baseline; gap: 10px; }
.built .count { font-size: 34px; font-weight: 700; }
.built .of { color: var(--muted); }
.bar { height: 10px; border-radius: 999px; background: var(--rule); overflow: hidden; }
.bar span { display: block; height: 100%; width: 0; background: linear-gradient(90deg, #6d5bd0, var(--violet)); border-radius: 999px; transition: width .2s; }
.tests-head { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; }
.tests-head .counts { font-size: 13px; }
.tests-head .planned { margin-left: auto; font-size: 12px; color: var(--muted); }
.counts .passed { color: var(--passed); }
.counts .running { color: var(--running); }
.counts .void { color: var(--void); }
.counts .failed { color: var(--failed); }
.tiles { display: grid; grid-template-columns: repeat(16, minmax(0, 1fr)); gap: 6px; }
.tile { aspect-ratio: 1; border-radius: 5px; border: 1px solid var(--edge); background: var(--panel); transition: background .25s, border-color .25s; }
.tile[data-state="running"] { background: #1d2c4d; border-color: var(--running); animation: beat .8s ease-in-out infinite; }
.tile[data-state="passed"] { background: #2f9e5c; border-color: var(--passed); }
.tile[data-state="void"] { background: #4a3812; border-color: var(--void); }
.tile[data-state="failed"] { background: #c8261d; border-color: var(--failed); box-shadow: 0 0 12px rgba(255,122,112,.6); }
.tiles .empty { grid-column: 1 / -1; }
.legend { display: flex; gap: 16px; font-size: 12px; color: var(--muted); flex-wrap: wrap; }
.legend .tile { display: inline-block; width: 10px; vertical-align: middle; margin-right: 6px; }
.verdict { flex-direction: row; align-items: center; gap: 18px; }
.verdict .word { font-size: 26px; color: var(--muted); }
.verdict .word[data-state="green"] { color: var(--passed); }
.verdict .word[data-state="red"] { color: var(--failed); }
.verdict .word[data-state="held"] { color: var(--void); }
.verdict .why { color: var(--muted); font-size: 13px; }
.landed-row { display: grid; grid-template-columns: 14px minmax(0, 1fr) auto; gap: 10px; align-items: center; font-size: 13px; }
.landed-row .pip { width: 10px; height: 10px; border-radius: 50%; background: var(--passed); }
.landed-row .who { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.landed-row .sha, .landed-row .took { color: var(--muted); }
.landed-row .took { font-size: 12px; }
.foot { margin-top: auto; padding-top: 12px; border-top: 1px solid var(--border); display: flex; flex-direction: column; gap: 4px; }
.clock { font-size: 28px; font-weight: 700; }
.overlay { position: fixed; inset: 0; display: grid; place-items: center; background: radial-gradient(circle at 50% 45%, rgba(255,209,102,.20), rgba(11,12,14,.82) 60%); pointer-events: none; z-index: 10; animation: fade 5.2s ease-in-out both; }
.overlay .card { display: flex; flex-direction: column; align-items: center; gap: 14px; text-align: center; padding: 32px; animation: burst 1.2s ease-out both; }
.overlay .mark { width: 96px; height: 96px; border: 5px solid var(--gold); border-radius: 6px; display: grid; place-items: center; box-shadow: 0 0 60px rgba(255,209,102,.55); }
.overlay .word { font-size: 72px; font-weight: 700; line-height: 1; color: var(--gold); letter-spacing: -.02em; }
.overlay .who { font-size: 20px; }
.overlay .what { font-size: 18px; color: var(--soft); }
.overlay .what b { color: var(--gold); }
.piece { position: fixed; top: -24px; width: 14px; height: 14px; border-radius: 3px; pointer-events: none; z-index: 11; animation-name: fall; animation-timing-function: cubic-bezier(.3,.6,.5,1); animation-iteration-count: 1; animation-fill-mode: both; }
.piece.phi { border: 2px solid currentColor; background: transparent; border-radius: 2px; display: grid; place-items: center; }
.piece.phi::after { content: ""; width: 7px; height: 7px; border: 2px solid currentColor; border-radius: 50%; }
@keyframes beat { 50% { opacity: .55; } }
@keyframes arrive { from { opacity: 0; transform: translateX(-14px); } to { opacity: 1; transform: none; } }
@keyframes fall { 0% { transform: translateY(0) rotate(0); } 100% { transform: translateY(105vh) rotate(720deg); } }
@keyframes burst { 0% { transform: scale(.6); opacity: 0; } 30% { transform: scale(1.06); opacity: 1; } 100% { transform: scale(1); opacity: 1; } }
@keyframes fade { 0% { opacity: 0; } 8% { opacity: 1; } 85% { opacity: 1; } 100% { opacity: 0; } }
@media (prefers-reduced-motion: reduce) { *, *::before, *::after { animation: none !important; transition: none !important; } .piece { display: none; } }
@media (max-width: 900px) { .cols, .pair { grid-template-columns: 1fr; } .track { grid-template-columns: repeat(3, minmax(0, 1fr)); } .overlay .word { font-size: 52px; } }
</style>
</head>
<body>
<main>
<header>
    <div class="brand">${loomMark(24)}Loom</div>
    <span class="mono headline" id="headline">change <b>&ndash;</b></span>
    <span class="connection" id="connection" data-state="connecting">connecting</span>
    <div class="controls">
        <span class="label">sound</span>
        <button type="button" class="toggle" id="sound" aria-pressed="false">Off</button>
        <button type="button" class="toggle" id="hear">Hear a landing</button>
    </div>
</header>
<section class="track" id="track" aria-label="Where the change is"></section>
<section class="cols">
    <article class="panel" id="panel-posted">
        <span class="label">Posted</span>
        <span class="note"><span class="mono">POST /changes</span>, in submit order</span>
        <div class="list" id="posted"></div>
    </article>
    <div class="middle">
        <div class="pair">
            <article class="panel" id="panel-block">
                <span class="label">The next block</span>
                <div class="block">
                    <svg width="128" height="128" viewBox="0 0 128 128" role="img" aria-label="No block is forming">
                        <circle cx="64" cy="64" r="54" fill="none" stroke="#25272d" stroke-width="10"></circle>
                        <circle cx="64" cy="64" r="54" fill="none" stroke="#b69cff" stroke-width="10" stroke-linecap="round" stroke-dasharray="339.3" stroke-dashoffset="339.3" transform="rotate(-90 64 64)"></circle>
                        <text x="64" y="70" text-anchor="middle" fill="#ecedef" font-family="ui-monospace, Menlo, monospace" font-size="24" font-weight="700">&ndash;</text>
                    </svg>
                    <div class="words">
                        <b class="title">No block forming</b>
                        <span class="closes">closes on the first of: a free slot, a full budget, the oldest change waiting 2 min</span>
                        <span class="mono reason">each change is its own future until Queue logs block.opened</span>
                    </div>
                </div>
            </article>
            <article class="panel" id="panel-build">
                <span class="label">Build on workshop</span>
                <div class="built"><span class="mono count">&ndash;</span><span class="of">products &middot; waits on product.built</span></div>
                <div class="bar"><span></span></div>
                <span class="mono note">each product once, by its key, into refs/action/&lt;productKey&gt;</span>
            </article>
        </div>
        <article class="panel grow" id="panel-test">
            <div class="tests-head">
                <span class="label">Tests, unit by unit</span>
                <span class="mono counts" id="counts"></span>
                <span class="mono planned" id="planned"></span>
            </div>
            <div class="tiles" id="tiles" role="img"></div>
            <div class="legend">
                <span><span class="tile" data-state="passed"></span>passed</span>
                <span><span class="tile" data-state="running"></span>running on the farm</span>
                <span><span class="tile" data-state="void"></span>void, run again, never green</span>
                <span><span class="tile" data-state="failed"></span>failed, rerun alone to blame it</span>
            </div>
        </article>
        <article class="panel verdict" id="panel-verdict">
            <span class="label">Verdict</span>
            <b class="word" id="verdict-word">deciding</b>
            <span class="why" id="verdict-why"></span>
        </article>
    </div>
    <article class="panel" id="panel-landed">
        <span class="label">Landed today</span>
        <div class="list" id="landed"></div>
        <div class="foot">
            <span class="label">Submit to main</span>
            <span class="mono clock" id="clock">&ndash;</span>
            <span class="note" id="clock-note">the change in view, from its arrival to its landing</span>
        </div>
    </article>
</section>
</main>
<script nonce="${nonce}">
(function () {
    'use strict';
    // The token comes after the # once. The page keeps it in this browser, so the bare address works from then on,
    // and takes it out of the address bar, so a shared screen or a copied link never carries it.
    var token = decodeURIComponent(location.hash.slice(1));
    try {
        if (token) { localStorage.setItem('loom.boardToken', token); }
        else { token = localStorage.getItem('loom.boardToken') || ''; }
    }
    catch (error) { /* a private window keeps it for this page only */ }
    if (location.hash) { history.replaceState(null, '', location.pathname + location.search); }
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
    var onTheWay = ['queued', 'building', 'testing', 'parked'];
    var stages = [['Posted', 'owners submit'], ['Block', 'the next block forms'], ['Build', 'products, once each'], ['Test', 'only what changed'], ['Verdict', 'by written rule'], ['Landed', 'main moves']];
    var panels = ['panel-posted', 'panel-block', 'panel-build', 'panel-test', 'panel-verdict', 'panel-landed'];

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

    // A change the board first saw already finished has no trip it can time.
    function took(line) {
        var from = Date.parse(line.firstSeenAt);
        var to = line.finishedAt ? Date.parse(line.finishedAt) : Date.now();
        return isNaN(from) || (line.finishedAt && to - from < 1000) ? '\u2013' : duration((to - from) / 1000);
    }

    function sorted(filter) {
        return Array.from(changes.values()).filter(filter);
    }

    function finishedUnits(units) {
        return units.passed + units.failed + units.void;
    }

    // An attempt that finished with void units decides nothing: a void is never a verdict, and the queue lists the
    // next attempt, so the change is still testing.
    function voided(line) {
        return line.state === 'testing' && line.units.void > 0 && line.units.planned > 0 && finishedUnits(line.units) >= line.units.planned;
    }

    // The change the page centres on: the last one that moved while on its way, else the last that finished.
    function focusLine() {
        var moving = sorted(function (line) { return ['queued', 'building', 'testing'].indexOf(line.state) >= 0; });
        moving.sort(function (left, right) { return Date.parse(right.updatedAt) - Date.parse(left.updatedAt); });
        if (moving.length > 0) { return moving[0]; }
        if (focusId && changes.get(focusId)) { return changes.get(focusId); }
        var all = sorted(function () { return true; });
        all.sort(function (left, right) { return Date.parse(right.updatedAt) - Date.parse(left.updatedAt); });
        return all[0] || null;
    }

    // Where a change stands on the track. Blocks aren't in the log yet, so a change goes from the line to its build.
    function stageOf(line) {
        if (!line) { return -1; }
        var decided = line.units.planned > 0 && finishedUnits(line.units) >= line.units.planned && !voided(line);
        return { queued: 0, parked: 0, refused: 0, building: 2, testing: decided ? 4 : 3, red: 5, landed: 6 }[line.state];
    }

    function renderTrack(line) {
        var section = document.getElementById('track');
        section.replaceChildren();
        var current = stageOf(line);
        stages.forEach(function (stage, index) {
            var step = element('div', 'step');
            var red = line && ((line.state === 'red' && index >= 4) || (line.state === 'refused' && index === 0));
            step.dataset.state = red ? 'red' : index < current ? 'done' : index === current ? 'active' : '';
            var head = element('div', 'head');
            head.appendChild(element('span', 'dot'));
            var name = stage[0];
            var sub = stage[1];
            if (index === 5 && line && line.state === 'red') { name = 'Red'; sub = 'to its owner, with a repro'; }
            if (index === 0 && line && line.state === 'parked') { sub = 'parked, waits to restack'; }
            if (index === 0 && line && line.state === 'refused') { sub = 'refused at the door'; }
            head.appendChild(element('span', 'name', name));
            var time = element('span', 'mono time', '');
            // The step it's on counts up from its arrival; the last step, once it's there, holds the whole trip.
            if (index === current || (index === 5 && current >= 5)) {
                time.dataset.from = line.firstSeenAt;
                if (line.finishedAt) { time.dataset.to = line.finishedAt; }
            }
            head.appendChild(time);
            step.appendChild(head);
            step.appendChild(element('span', 'note', sub));
            section.appendChild(step);
        });
        panels.forEach(function (id, index) {
            document.getElementById(id).classList.toggle('on', index === Math.min(current, 5) && current < 6);
        });
    }

    function renderHeadline(line) {
        var headline = document.getElementById('headline');
        headline.replaceChildren();
        headline.appendChild(document.createTextNode('change '));
        headline.appendChild(element('b', null, line ? line.sha.slice(0, 12) : '\\u2013'));
        if (line) { headline.appendChild(document.createTextNode(' \\u00B7 ' + line.owner)); }
    }

    function renderPosted() {
        var list = document.getElementById('posted');
        list.replaceChildren();
        var line = sorted(function (change) { return onTheWay.indexOf(change.state) >= 0; });
        line.sort(function (left, right) { return Date.parse(left.firstSeenAt) - Date.parse(right.firstSeenAt); });
        if (line.length === 0) { list.appendChild(element('span', 'empty', 'Waiting for the first change.')); }
        line.forEach(function (change, index) {
            var card = element('div', drawn.has(change.change) ? 'posted' : 'posted new');
            drawn.add(change.change);
            var top = element('span', 'top');
            top.appendChild(element('b', null, change.owner));
            top.appendChild(element('span', 'mono place', '#' + (index + 1)));
            card.appendChild(top);
            card.appendChild(element('span', 'mono meta', change.sha.slice(0, 12) + ' \\u00B7 ' + change.state + (change.units.planned > 0 ? ' \\u00B7 ' + change.units.planned + ' units' : '')));
            list.appendChild(card);
        });
    }

    function renderTests(line) {
        var units = line ? line.units : { planned: 0, passed: 0, failed: 0, void: 0 };
        var open = Math.max(0, units.planned - finishedUnits(units));
        // Units still out are on the farm while the change is testing, and waiting before that.
        var running = line && line.state === 'testing' ? open : 0;
        var waiting = open - running;
        var counts = document.getElementById('counts');
        counts.replaceChildren();
        [['passed', units.passed, 'passed'], ['running', running, 'running'], ['void', units.void, 'void'], ['failed', units.failed, 'failed']].forEach(function (part) {
            counts.appendChild(element('b', part[0], part[1]));
            counts.appendChild(document.createTextNode(' ' + part[2] + ' \\u00B7 '));
        });
        counts.appendChild(document.createTextNode(waiting + ' waiting'));
        document.getElementById('planned').textContent = line ? units.planned + ' units, the plan for ' + line.sha.slice(0, 12) : 'no change in view';
        var tiles = document.getElementById('tiles');
        tiles.replaceChildren();
        var shown = Math.min(units.planned, 1024);
        tiles.style.gridTemplateColumns = 'repeat(' + (shown <= 64 ? 16 : shown <= 256 ? 32 : 48) + ', minmax(0, 1fr))';
        tiles.setAttribute('aria-label', units.passed + ' passed, ' + running + ' running, ' + units.void + ' void, ' + units.failed + ' failed of ' + units.planned);
        for (var index = 0; index < shown; index++) {
            var tile = element('span', 'tile');
            var state = index < units.passed ? 'passed' : index < units.passed + units.failed ? 'failed' : index < finishedUnits(units) ? 'void' : running > 0 ? 'running' : '';
            if (state) { tile.dataset.state = state; }
            tiles.appendChild(tile);
        }
        if (shown === 0) { tiles.appendChild(element('span', 'empty', line ? 'No units planned yet.' : 'Waiting for the first change.')); }
    }

    function renderVerdict(line) {
        var word = document.getElementById('verdict-word');
        var why = document.getElementById('verdict-why');
        var verdict = ['deciding', '', 'waits for every planned unit; a missing one is void, never green'];
        if (line && line.state === 'landed') { verdict = ['Green', 'green', 'every planned unit passed, and the units equal the plan']; }
        else if (line && line.state === 'red') { verdict = ['Red', 'red', line.units.failed + ' of ' + line.units.planned + ' units failed; its owner has the failing test and a repro']; }
        else if (line && line.state === 'parked') { verdict = ['Parked', 'held', 'held behind a change it stacks on; it restacks by itself']; }
        else if (line && voided(line)) { verdict = ['Void', 'held', line.units.void + ' of ' + line.units.planned + ' units never finished, so nothing is decided; the next attempt runs them again']; }
        else if (line && line.state === 'refused') { verdict = ['Refused', 'red', 'the queue refused it at the door']; }
        word.textContent = verdict[0];
        if (verdict[1]) { word.dataset.state = verdict[1]; } else { delete word.dataset.state; }
        why.textContent = verdict[2];
    }

    function renderLanded(line) {
        var list = document.getElementById('landed');
        list.replaceChildren();
        var done = sorted(function (change) { return change.state === 'landed'; });
        done.sort(function (left, right) { return Date.parse(right.finishedAt || right.updatedAt) - Date.parse(left.finishedAt || left.updatedAt); });
        if (done.length === 0) { list.appendChild(element('span', 'empty', 'Nothing has landed yet today.')); }
        done.slice(0, 14).forEach(function (change) {
            var row = element('div', 'landed-row');
            row.appendChild(element('span', 'pip'));
            var who = element('span', 'who');
            who.appendChild(element('b', null, change.owner));
            who.appendChild(document.createTextNode(' '));
            who.appendChild(element('span', 'mono sha', change.sha.slice(0, 8)));
            row.appendChild(who);
            row.appendChild(element('span', 'mono took', took(change)));
            list.appendChild(row);
        });
        var clock = document.getElementById('clock');
        delete clock.dataset.from;
        delete clock.dataset.to;
        if (line) {
            clock.dataset.from = line.firstSeenAt;
            if (line.finishedAt) { clock.dataset.to = line.finishedAt; }
        }
        else { clock.textContent = '\\u2013'; }
    }

    function render() {
        frameRequested = false;
        var line = focusLine();
        renderHeadline(line);
        renderTrack(line);
        renderPosted();
        renderTests(line);
        renderVerdict(line);
        renderLanded(line);
        tick();
    }

    function schedule() {
        if (!frameRequested) { frameRequested = true; requestAnimationFrame(render); }
    }

    function tick() {
        document.querySelectorAll('[data-from]').forEach(function (node) {
            var from = Date.parse(node.dataset.from);
            var to = node.dataset.to ? Date.parse(node.dataset.to) : Date.now();
            if (!isNaN(from)) { node.textContent = node.dataset.to && to - from < 1000 ? '\u2013' : duration((to - from) / 1000); }
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
        var mark = element('span', 'mark');
        mark.innerHTML = ${JSON.stringify(loomMark(64, celebrationGold, celebrationGold))};
        card.appendChild(mark);
        card.appendChild(element('b', 'word', 'Landed'));
        card.appendChild(element('span', 'who', test ? 'A test landing: this is what main moving looks like' : line.owner + '\\u2019s change is on main'));
        if (!test) {
            var what = element('span', 'mono what');
            what.appendChild(element('b', null, line.sha.slice(0, 12)));
            what.appendChild(document.createTextNode(' \\u00B7 ' + line.units.passed + ' of ' + line.units.planned + ' green \\u00B7 ' + took(line) + ' from submit'));
            card.appendChild(what);
        }
        overlay.appendChild(card);
        document.body.appendChild(overlay);
        var palette = ['${celebrationGold}', '#b69cff', '#4fd68a', '#79a6ff', '#ff9ec7', '#ecedef'];
        var pieces = [];
        for (var index = 0; index < 40; index++) {
            var color = palette[index % palette.length];
            var phi = index % 4 === 0;
            var piece = element('span', phi ? 'piece phi' : 'piece');
            piece.style.left = ((index * 37) % 100) + 'vw';
            piece.style.color = color;
            piece.style.backgroundColor = phi ? 'transparent' : color;
            piece.style.animationDuration = (2.2 + (index % 5) * 0.35) + 's';
            piece.style.animationDelay = ((index % 10) * 0.09) + 's';
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
        button.textContent = on ? 'On' : 'Off';
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
    document.getElementById('sound').textContent = soundOn ? 'On' : 'Off';

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
            setConnection('reconnecting', 'open this page once with its board token after the #');
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
    // A token the board refuses is forgotten here, so the next visit asks for a fresh one rather than failing quietly.
    function poll(why) {
        if (polling) { return; }
        polling = true;
        var first = true;
        function once() {
            fetch('/board/changes', { headers: { Authorization: 'Bearer ' + token }, cache: 'no-store' })
                .then(function (response) {
                    if (response.status === 401 || response.status === 403) {
                        try { localStorage.removeItem('loom.boardToken'); } catch (error) { /* nothing kept */ }
                        return Promise.reject(new Error('the board refused this token; open the page with a fresh one after the #'));
                    }
                    return response.ok ? response.json() : Promise.reject(new Error('the board answered ' + response.status));
                })
                .then(function (body) {
                    if (first) { apply({ kind: 'snapshot', changes: body.changes }); first = false; }
                    else { (body.changes || []).forEach(take); schedule(); }
                    setConnection('live', 'live, every 2 s (' + why + ')');
                    setTimeout(once, 2000);
                })
                .catch(function (error) {
                    setConnection('reconnecting', error.message);
                    if (token && error.message.indexOf('refused this token') < 0) { setTimeout(once, 2000); }
                });
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
