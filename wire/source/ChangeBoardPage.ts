// loom-pipeline's board of changes: one line per change on its way to main, read from /board/changes every two
// seconds. Plain HTML and inline script under the response's CSP nonce, nothing loaded from elsewhere. The board
// token rides after the # in the address, which a browser never sends, and goes only in an Authorization header.

export function renderChangeBoardPage(nonce: string): string {
    return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>Loom changes</title>
<style nonce="${nonce}">
:root {
    color-scheme: light dark;
    --background: #f6f6f3; --surface: #ffffff; --sunk: #efefea; --border: #e3e3dc;
    --text: #1b1b19; --muted: #6d6d66;
    --queued: #8d8d85; --running: #2f6bed; --passed: #16803c; --failed: #c8261d; --void: #8a5a00;
    --monospace: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
@media (prefers-color-scheme: dark) {
    :root {
        --background: #0e0f11; --surface: #16171a; --sunk: #1d1f23; --border: #2a2c31;
        --text: #ecedef; --muted: #a3a6ae;
        --queued: #8a8d95; --running: #79a6ff; --passed: #4fd68a; --failed: #ff7a70; --void: #f0b34e;
    }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--background); color: var(--text); font: 14px/1.45 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 1440px; margin: 0 auto; padding: 18px 16px 48px; }
header { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; margin-bottom: 14px; }
h1 { font-size: 18px; margin: 0; }
.status { font-size: 12px; color: var(--muted); }
.changes { display: grid; gap: 6px; }
.change { display: grid; grid-template-columns: 90px minmax(0, 1.2fr) 110px minmax(120px, 1fr) 150px 70px; gap: 12px; align-items: center;
    background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 9px 12px; }
.state { font: 600 11px/1 system-ui, sans-serif; letter-spacing: .06em; text-transform: uppercase; }
.state[data-state="queued"], .state[data-state="parked"], .state[data-state="refused"] { color: var(--queued); }
.state[data-state="building"], .state[data-state="testing"] { color: var(--running); }
.state[data-state="landed"] { color: var(--passed); }
.state[data-state="red"] { color: var(--failed); }
.owner { font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.mono { font-family: var(--monospace); font-size: 12px; color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.bar { display: flex; height: 8px; border-radius: 999px; overflow: hidden; background: var(--sunk); }
.bar span { height: 100%; }
.bar .passed { background: var(--passed); } .bar .failed { background: var(--failed); } .bar .void { background: var(--void); }
.age { font-family: var(--monospace); font-size: 12px; text-align: right; color: var(--muted); }
.empty { color: var(--muted); border: 1px dashed var(--border); border-radius: 10px; padding: 20px; text-align: center; }
@media (max-width: 760px) {
    .change { grid-template-columns: 80px minmax(0, 1fr) 60px; }
    .change .sha, .change .tally { display: none; }
    .change .bar { grid-column: 1 / -1; }
}
</style>
</head>
<body>
<main>
<header><h1>Changes on their way to main</h1><span class="status" id="status">connecting</span></header>
<section class="changes" id="changes" aria-live="polite"></section>
</main>
<script nonce="${nonce}">
(function () {
    'use strict';
    var token = decodeURIComponent(location.hash.slice(1));
    var changes = [];
    var readAt = null;

    function element(tag, className, text) {
        var node = document.createElement(tag);
        if (className) { node.className = className; }
        if (text !== undefined && text !== null) { node.textContent = String(text); }
        return node;
    }

    function duration(seconds) {
        seconds = Math.max(0, Math.floor(seconds));
        if (seconds >= 3600) { return Math.floor(seconds / 3600) + 'h ' + String(Math.floor((seconds % 3600) / 60)).padStart(2, '0') + 'm'; }
        if (seconds >= 60) { return Math.floor(seconds / 60) + 'm ' + String(seconds % 60).padStart(2, '0') + 's'; }
        return seconds + 's';
    }

    function render() {
        var section = document.getElementById('changes');
        section.replaceChildren();
        if (changes.length === 0) {
            section.appendChild(element('div', 'empty', 'No change on its way to main.'));
        }
        changes.forEach(function (change) {
            var row = element('article', 'change');
            var state = element('span', 'state', change.state);
            state.dataset.state = change.state;
            row.appendChild(state);
            row.appendChild(element('span', 'owner', change.owner));
            row.appendChild(element('span', 'mono sha', change.sha.slice(0, 12)));
            var bar = element('div', 'bar');
            bar.setAttribute('role', 'img');
            var units = change.units;
            bar.setAttribute('aria-label', units.passed + ' passed, ' + units.failed + ' failed, ' + units.void + ' void of ' + units.planned + ' planned');
            ['passed', 'failed', 'void'].forEach(function (kind) {
                if (units[kind] > 0 && units.planned > 0) {
                    var part = element('span', kind);
                    part.style.width = (units[kind] / units.planned * 100) + '%';
                    bar.appendChild(part);
                }
            });
            row.appendChild(bar);
            row.appendChild(element('span', 'mono tally', units.passed + '/' + units.planned + ' passed' + (units.failed ? ', ' + units.failed + ' failed' : '')));
            var age = element('span', 'age', '');
            age.dataset.from = change.updatedAt;
            row.appendChild(age);
            row.title = change.change + (change.future ? ' on ' + change.future : '');
            section.appendChild(row);
        });
        tick();
    }

    function tick() {
        document.querySelectorAll('[data-from]').forEach(function (node) {
            var from = Date.parse(node.dataset.from);
            if (!isNaN(from)) { node.textContent = duration((Date.now() - from) / 1000); }
        });
        if (readAt !== null) {
            document.getElementById('status').textContent = 'read ' + duration((Date.now() - readAt) / 1000) + ' ago';
        }
    }

    function read() {
        if (!token) {
            document.getElementById('status').textContent = 'this page needs its board token after the #';
            return;
        }
        fetch('/board/changes', { headers: { Authorization: 'Bearer ' + token }, cache: 'no-store' })
            .then(function (response) { return response.ok ? response.json() : Promise.reject(new Error('the board answered ' + response.status)); })
            .then(function (body) { changes = body.changes || []; readAt = Date.now(); render(); })
            .catch(function (error) { document.getElementById('status').textContent = error.message; })
            .finally(function () { setTimeout(read, 2000); });
    }

    setInterval(tick, 1000);
    read();
})();
</script>
</body>
</html>
`;
}
