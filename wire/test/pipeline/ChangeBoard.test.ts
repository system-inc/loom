// loom-pipeline's board of changes: Queue's pushes in, one line per change out, to a board token only.

import { env } from 'cloudflare:workers';
import { runInDurableObject } from 'cloudflare:test';
import { describe, expect, it } from 'vitest';
import { checkChangeSummary, FinishedChangeMilliseconds, type BoardLine, type ChangeSummary } from '../../source/ChangeBoard';
import { boardStream, boardToken, call, openBoard, token, waitFor } from '../Helpers';

const board = (env as unknown as { ChangeBoard: DurableObjectNamespace }).ChangeBoard;

function boardStub(): DurableObjectStub {
    return board.get(board.idFromName('board'));
}

let next = 0;
function changeId(): string {
    next++;
    return 'chg_' + next.toString(32).padStart(26, '0');
}

function summary(change: string, fields: Partial<ChangeSummary> = {}): ChangeSummary {
    return {
        change: change,
        owner: 'system_adamic_loom_web',
        sha: 'a'.repeat(40),
        state: 'testing',
        future: 'f'.repeat(40),
        units: { planned: 10, passed: 3, failed: 0, void: 0 },
        updatedAt: new Date().toISOString(),
        ...fields,
    };
}

async function push(value: unknown): Promise<Response> {
    return boardStub().fetch('https://board/change', { method: 'POST', body: JSON.stringify(value) });
}

async function lines(): Promise<ChangeSummary[]> {
    const response = await call('/board/changes', { bearer: await boardToken() });
    expect(response.status).toBe(200);
    return ((await response.json()) as { changes: ChangeSummary[] }).changes;
}

describe('the board of changes', function () {
    it('takes exactly the summary Queue pushes, and shows each change once, newest move first', async function () {
        const first = changeId();
        const second = changeId();
        expect((await push(summary(first, { updatedAt: '2026-10-09T23:40:00.000Z' }))).status).toBe(200);
        expect((await push(summary(second, { updatedAt: '2026-10-09T23:41:00.000Z', state: 'queued', future: null }))).status).toBe(200);
        expect((await push(summary(first, { updatedAt: '2026-10-09T23:42:00.000Z', units: { planned: 10, passed: 10, failed: 0, void: 0 } }))).status).toBe(200);
        // A push that arrives after a newer one is dropped.
        expect((await push(summary(first, { updatedAt: '2026-10-09T23:39:00.000Z', state: 'queued' }))).status).toBe(200);
        const shown = (await lines()).filter(function (line) {
            return line.change === first || line.change === second;
        });
        expect(shown.map(function (line) {
            return [line.change, line.state, line.units.passed];
        })).toEqual([[first, 'testing', 10], [second, 'queued', 3]]);
    });

    it('refuses a summary that is not exactly the seven fields, each well formed', async function () {
        const change = changeId();
        const refused: [unknown, string][] = [
            [{ ...summary(change), extra: 1 }, 'the summary is exactly change, owner, sha, state, future, units and updatedAt'],
            [summary('chg_X'), 'change is a change id'],
            [summary(change, { state: 'merged' as ChangeSummary['state'] }), 'state is one of queued, building, testing, landed, red, parked, refused'],
            [summary(change, { sha: 'abc' }), 'sha is a commit'],
            [summary(change, { units: { planned: 1, passed: -1, failed: 0, void: 0 } }), 'units is exactly planned, passed, failed and void, each a count'],
            [summary(change, { updatedAt: 'yesterday' }), 'updatedAt is an RFC 3339 UTC time'],
        ];
        for (const [value, reason] of refused) {
            const response = await push(value);
            expect(response.status, reason).toBe(400);
            expect(await response.json()).toEqual({ error: reason });
        }
        expect(checkChangeSummary(JSON.stringify(summary(change)))).toMatchObject({ change: change });
    });

    it('keeps a finished change for a day after it finished, and a change on its way for good', async function () {
        const landed = changeId();
        const waiting = changeId();
        await push(summary(landed, { state: 'landed' }));
        await push(summary(waiting, { state: 'queued', updatedAt: '2026-10-01T00:00:00.000Z' }));
        await runInDurableObject(boardStub(), function (_, state) {
            state.storage.sql.exec('UPDATE changes SET finishedAt = ? WHERE change = ?', Date.now() - FinishedChangeMilliseconds - 1000, landed);
        });
        const kept = (await lines()).map(function (line) {
            return line.change;
        });
        expect(kept).not.toContain(landed);
        expect(kept).toContain(waiting);
    });

    it('serves its line to a board token only, and its page with no data and no token', async function () {
        expect((await call('/board/changes')).status).toBe(401);
        for (const scope of ['submit', 'coordinator', 'viewer'] as const) {
            expect((await call('/board/changes', { bearer: await token('board', scope) })).status, scope).toBe(403);
        }
        expect((await call('/board/change', { method: 'POST', bearer: await boardToken(), body: '{}' })).status).toBe(404);
        const page = await call('/board');
        expect(page.status).toBe(200);
        const policy = page.headers.get('Content-Security-Policy') ?? '';
        const nonce = /script-src 'nonce-([0-9a-f]+)'/.exec(policy)?.[1] ?? 'missing';
        const html = await page.text();
        expect(html).toContain(`<script nonce="${nonce}">`);
        expect(html).toContain("fetch('/board/changes', { headers: { Authorization: 'Bearer ' + token }");
        expect(html).toContain("['loom', 'token.' + token]");
        expect(html).not.toContain('chg_');
        // The policy forbids inline style attributes, so the page carries none: every style is under its nonce.
        expect(html).not.toMatch(/ style="/);
        expect(html).toContain('<link rel="icon" href="/favicon.svg" type="image/svg+xml">');
    });
});

// Loom Live's feed: the board's own projection, streamed, never the log.
describe('the board of changes, live', function () {
    it('streams a snapshot, then each recorded change with when the board first saw it, and nothing for a late push', async function () {
        const held = changeId();
        await push(summary(held, { state: 'queued', future: null, updatedAt: '2026-10-10T00:00:00.000Z' }));
        const viewer = await openBoard(await boardToken());
        await waitFor(function () {
            return viewer.frames.length > 0;
        });
        const snapshot = viewer.frames[0] as { kind: string; changes: BoardLine[] };
        expect(snapshot.kind).toBe('snapshot');
        const first = snapshot.changes.find(function (line) {
            return line.change === held;
        });
        expect(first?.firstSeenAt).toMatch(/^\d{4}-\d{2}-\d{2}T/);
        expect(first?.finishedAt).toBeNull();
        await push(summary(held, { state: 'testing', updatedAt: '2026-10-10T00:01:00.000Z' }));
        await push(summary(held, { state: 'queued', updatedAt: '2026-10-09T23:59:00.000Z' }));
        await push(summary(held, { state: 'landed', updatedAt: '2026-10-10T00:02:00.000Z' }));
        await waitFor(function () {
            return viewer.frames.length >= 3;
        });
        await new Promise(function (resolve) {
            setTimeout(resolve, 50);
        });
        const moved = viewer.frames.slice(1) as { kind: string; change: BoardLine }[];
        expect(moved.map(function (frame) {
            return [frame.kind, frame.change.state];
        })).toEqual([['change', 'testing'], ['change', 'landed']]);
        expect(moved.every(function (frame) {
            return frame.change.firstSeenAt === first?.firstSeenAt;
        })).toBe(true);
        expect(moved[1]?.change.finishedAt).toMatch(/^\d{4}-/);
        viewer.socket.close();
    });

    it('forgets the finish of a change that went back on its way', async function () {
        const change = changeId();
        await push(summary(change, { state: 'parked', updatedAt: '2026-10-10T00:00:00.000Z' }));
        await push(summary(change, { state: 'queued', updatedAt: '2026-10-10T00:01:00.000Z' }));
        const line = ((await (await call('/board/changes', { bearer: await boardToken() })).json()) as { changes: BoardLine[] }).changes.find(function (held) {
            return held.change === change;
        });
        expect(line).toMatchObject({ state: 'queued', finishedAt: null });
    });

    it('opens its stream to a board token offered as a subprotocol beside loom, and to nothing else', async function () {
        const bearer = await boardToken();
        expect((await boardStream('loom')).status).toBe(401);
        expect((await boardStream(`loom, token.${await token('board', 'submit')}`)).status).toBe(403);
        expect((await boardStream(`token.${bearer}`)).status).toBe(400);
        expect((await call('/board/stream', { headers: { 'Sec-WebSocket-Protocol': `loom, token.${bearer}` } })).status).toBe(426);
        const viewer = await openBoard(bearer);
        expect(viewer.response.headers.get('Sec-WebSocket-Protocol')).toBe('loom');
        viewer.socket.close();
    });
});

describe("Loom's mark", function () {
    it('is served as the favicon, an SVG on its indigo tile, to anyone', async function () {
        const response = await call('/favicon.svg');
        expect(response.status).toBe(200);
        expect(response.headers.get('Content-Type')).toBe('image/svg+xml');
        const svg = await response.text();
        expect(svg.startsWith('<svg xmlns="http://www.w3.org/2000/svg"')).toBe(true);
        expect(svg).toContain('#1a1540');
        expect(svg).toContain('#f2c14e');
        expect(svg).not.toContain('<script');
    });
});
