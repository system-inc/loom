// loom-pipeline's board of changes: Queue's pushes in, one line per change out, to a board token only.

import { env } from 'cloudflare:workers';
import { runInDurableObject } from 'cloudflare:test';
import { describe, expect, it } from 'vitest';
import { checkChangeSummary, FinishedChangeMilliseconds, type ChangeSummary } from '../../source/ChangeBoard';
import { boardToken, call, token } from '../Helpers';

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
        expect(html).not.toContain('chg_');
    });
});
