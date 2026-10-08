import { env } from 'cloudflare:workers';
import { runInDurableObject } from 'cloudflare:test';
import { describe, expect, it } from 'vitest';
import { checkMachines, jobOfRun, RetentionMilliseconds } from '../source/Board';
import { verifyToken } from '../source/Token';
import {
    boardRunWhen,
    boardSnapshot,
    boardStream,
    boardToken,
    call,
    datedRun,
    event,
    freshRun,
    openBoard,
    openViewer,
    postEvents,
    postPlan,
    postVerdict,
    runOf,
    snapshotWhen,
    TestSecret,
    token,
    unitEvents,
    verdictBody,
    waitFor,
} from './Helpers';

const keyHash = 'a'.repeat(64);
const eventsHash = 'b'.repeat(64);

function boardStub(): DurableObjectStub {
    return env.Board.get(env.Board.idFromName('board'));
}

function postMachines(bearer: string, body: unknown): Promise<Response> {
    return call('/board/machines', { method: 'POST', bearer: bearer, body: typeof body === 'string' ? body : JSON.stringify(body) });
}

describe("a run's summary on the board", function () {
    it('follows the plan, the events and the verdict', async function () {
        const board = await boardToken();
        const run = datedRun('board-summary');
        const coordinator = await token(run, 'coordinator');
        const runner = await token(run, 'runner');
        expect((await postPlan(run, coordinator, ['build', 'tests[shard=0]', 'tests[shard=1]', 'reused', 'refused'])).status).toBe(201);

        const planned = await boardRunWhen(board, run, function () {
            return true;
        });
        expect(planned).toMatchObject({
            run: run,
            job: 'board-summary',
            units: 5,
            queued: 5,
            running: 0,
            passed: 0,
            failed: 0,
            broken: 0,
            cached: 0,
            verdict: null,
            active: [],
            failures: [],
        });
        expect(typeof planned.plannedAt).toBe('string');
        expect(Number.isNaN(Date.parse(planned.updatedAt))).toBe(false);

        const buildStarted = event(run, 'build', 0, 'started', { machine: 'home', runnerVersion: '0.0.0' });
        const failing = [event(run, 'tests[shard=0]', 0, 'started', { machine: 'away' })];
        for (let line = 0; line < 7; line++) {
            failing.push(event(run, 'tests[shard=0]', failing.length, 'output', { stream: line % 2 === 0 ? 'stdout' : 'stderr', text: `line ${line}` }));
        }
        failing.push(event(run, 'tests[shard=0]', failing.length, 'exit', { code: 1 }));
        const failedAt = event(run, 'tests[shard=0]', failing.length, 'finished', { status: 'failed' });
        failing.push(failedAt);
        const response = await postEvents(run, runner, [
            buildStarted,
            event(run, 'build', 1, 'output', { stream: 'stdout', text: 'compiling' }),
            event(run, 'build', 2, 'output', { stream: 'stdout' }), // an empty line, its text left off
            ...failing,
            // Served from the cache, its finished still to come: it never runs, so it is still queued.
            event(run, 'reused', 0, 'cached', { key: keyHash, fromRun: 'r-earlier', events: eventsHash }),
            event(run, 'refused', 0, 'error', { phase: 'start', message: 'no such directory' }),
            event(run, 'refused', 1, 'finished', { status: 'broken' }),
        ]);
        expect(response.status, await response.clone().text()).toBe(200);

        const running = await boardRunWhen(board, run, function (summary) {
            return summary.broken === 1;
        });
        expect(running).toMatchObject({
            units: 5,
            queued: 2,
            running: 1,
            passed: 0,
            failed: 1,
            broken: 1,
            cached: 1,
            verdict: null,
            active: [{ unit: 'build', machine: 'home', since: buildStarted.time }],
        });
        // Newest first, each with its last five lines, whichever stream they came on.
        expect(running.failures).toEqual([
            { unit: 'refused', at: (event(run, 'refused', 1, 'finished') as { time: string }).time, status: 'broken', lines: [] },
            { unit: 'tests[shard=0]', at: failedAt.time, status: 'failed', lines: ['line 2', 'line 3', 'line 4', 'line 5', 'line 6'] },
        ]);
        expect(Date.parse(running.updatedAt)).toBeGreaterThanOrEqual(Date.parse(planned.updatedAt));

        await postEvents(run, runner, [
            event(run, 'reused', 1, 'finished', { status: 'passed' }),
            event(run, 'build', 3, 'exit', { code: 0 }),
            event(run, 'build', 4, 'finished', { status: 'passed' }),
        ]);
        expect((await postVerdict(run, coordinator, verdictBody('red', ['tests[shard=0]'], ['tests[shard=1] never ran'], ['reused']))).status).toBe(201);
        const decided = await boardRunWhen(board, run, function (summary) {
            return summary.verdict !== null;
        });
        expect(decided).toMatchObject({ verdict: 'red', queued: 1, running: 0, passed: 2, failed: 1, broken: 1, cached: 1, active: [] });
        expect(decided.failures).toHaveLength(2);
    });

    it('keeps the last 20 failures, and a unit placed again waits as queued', async function () {
        const board = await boardToken();
        const run = freshRun();
        const runner = await token(run, 'runner');
        const units = Array.from({ length: 23 }, function (_, index) {
            return `unit-${index}`;
        });
        await postPlan(run, await token(run, 'coordinator'), [...units, 'dropped']);
        for (const unit of units) {
            await postEvents(run, runner, unitEvents(run, unit, unit === 'unit-5' ? 'broken' : 'failed', 1));
        }
        await postEvents(run, runner, [
            event(run, 'dropped', 0, 'started', { machine: 'box-1' }),
            event(run, 'dropped', 1, 'error', { phase: 'place', message: 'box-1 dropped the unit; placed on box-2' }),
        ]);
        const summary = await boardRunWhen(board, run, function (candidate) {
            return candidate.failed + candidate.broken === 23 && candidate.queued === 1;
        });
        // A run id without the coordinator's time and suffix is its own job.
        expect(summary.job).toBe(run);
        expect(summary.running).toBe(0);
        expect(summary.failures.map(function (failure) {
            return failure.unit;
        })).toEqual(units.slice(3).reverse());
        expect(summary.failures.find(function (failure) {
            return failure.unit === 'unit-5';
        })).toMatchObject({ status: 'broken', lines: ['unit-5 line 0'] });
    });

    it('names the job without the time and random suffix', function () {
        expect(jobOfRun('adamic-gate-20261008T200212-20fe227d')).toBe('adamic-gate');
        expect(jobOfRun('a-b.c-20261008T200212-0123abcd')).toBe('a-b.c');
        expect(jobOfRun('adamic-gate')).toBe('adamic-gate');
        expect(jobOfRun('adamic-gate-20261008T200212')).toBe('adamic-gate-20261008T200212');
        expect(jobOfRun('adamic-gate-20261008T200212-20FE227D')).toBe('adamic-gate-20261008T200212-20FE227D');
    });
});

describe("the board's machines", function () {
    it('takes the coordinator of any run, replaces each by name, and counts what runs on it', async function () {
        const board = await boardToken();
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const north = 'north-' + crypto.randomUUID();
        const south = 'south-' + crypto.randomUUID();
        const posted = await postMachines(coordinator, { machines: [{ name: north, cores: 64, slots: 3 }, { name: south, cores: 8, slots: 1 }] });
        expect(posted.status, await posted.clone().text()).toBe(200);
        // Another coordinator, another run: it replaces north and leaves south alone.
        const other = freshRun();
        expect((await postMachines(await token(other, 'coordinator'), { machines: [{ name: north, cores: 64, slots: 4 }] })).status).toBe(200);

        await postPlan(run, coordinator, ['a', 'b', 'c']);
        const runner = await token(run, 'runner');
        await postEvents(run, runner, [
            event(run, 'a', 0, 'started', { machine: north }),
            // A runner names its machine by hostname, which may differ in case from the coordinator's name for it.
            event(run, 'b', 0, 'started', { machine: north.toUpperCase() }),
            event(run, 'c', 0, 'started', { machine: south }),
            event(run, 'c', 1, 'finished', { status: 'passed' }),
        ]);
        const snapshot = await snapshotWhen(board, function (candidate) {
            return runOf(candidate, run)?.running === 2;
        });
        const machines = snapshot.machines.filter(function (machine) {
            return machine.name === north || machine.name === south;
        });
        expect(machines).toEqual([
            { name: north, cores: 64, slots: 4, running: 2 },
            { name: south, cores: 8, slots: 1, running: 0 },
        ]);
        // Once the run has its verdict its units run nowhere.
        await postVerdict(run, coordinator, verdictBody('void', [], ['a and b never finished']));
        const after = await snapshotWhen(board, function (candidate) {
            return runOf(candidate, run)?.verdict === 'void';
        });
        expect(after.machines.find(function (machine) {
            return machine.name === north;
        })?.running).toBe(0);
    });

    it('refuses other scopes and malformed bodies', async function () {
        const run = freshRun();
        const good = { machines: [{ name: 'box', cores: 4, slots: 1 }] };
        expect((await postMachines(await boardToken(), good)).status).toBe(403);
        expect((await postMachines(await token(run, 'runner'), good)).status).toBe(403);
        expect((await postMachines(await token(run, 'viewer'), good)).status).toBe(403);
        expect((await call('/board/machines', { method: 'POST', body: JSON.stringify(good) })).status).toBe(401);
        const coordinator = await token(run, 'coordinator');
        expect((await call('/board/machines', { bearer: coordinator })).status).toBe(405);
        for (const body of [
            'nope',
            [],
            {},
            { machines: [], extra: 1 },
            { Machines: [] },
            { machines: 'box' },
            { machines: [{ name: 'box', cores: 4 }] },
            { machines: [{ name: 'box', cores: 4, slots: 1, running: 0 }] },
            { machines: [{ name: '', cores: 4, slots: 1 }] },
            { machines: [{ name: 'box', cores: -1, slots: 1 }] },
            { machines: [{ name: 'box', cores: 4, slots: 1.5 }] },
            { machines: [{ name: 'box', cores: '4', slots: 1 }] },
            { machines: [{ name: 'box', cores: 4, slots: 1 }, { name: 'box', cores: 8, slots: 2 }] },
        ]) {
            const response = await postMachines(coordinator, body);
            expect(response.status, JSON.stringify(body)).toBe(400);
        }
        expect((await postMachines(coordinator, good)).status).toBe(200);
        // What Go marshals for a coordinator with no machines.
        expect(checkMachines('{"machines":null}')).toEqual([]);
    });
});

describe("the board's stream", function () {
    it('takes the token only as a subprotocol beside loom, and only a board token', async function () {
        const board = await boardToken();
        expect((await boardStream(null)).status).toBe(401);
        expect((await boardStream('loom')).status).toBe(401);
        expect((await boardStream('loom', { bearer: board })).status).toBe(401);
        expect((await call(`/board/stream?token=${encodeURIComponent(board)}`, { headers: { Upgrade: 'websocket', 'Sec-WebSocket-Protocol': 'loom' } })).status).toBe(401);
        expect((await boardStream(`loom, token.${board}x`)).status).toBe(401);
        expect((await boardStream(`loom, token.${await token('board', 'viewer')}`)).status).toBe(403);
        expect((await boardStream(`loom, token.${await token('board', 'coordinator')}`)).status).toBe(403);
        expect((await boardStream(`loom, token.${await token('elsewhere', 'board')}`)).status).toBe(403);
        expect((await boardStream(`token.${board}`)).status).toBe(400);
        expect((await call('/board/stream', { headers: { 'Sec-WebSocket-Protocol': `loom, token.${board}` } })).status).toBe(426);
    });

    it('sends the snapshot, then each change and a pulse after it', async function () {
        const board = await boardToken();
        const viewer = await openBoard(board);
        expect(viewer.response.headers.get('Sec-WebSocket-Protocol')).toBe('loom');
        await waitFor(function () {
            return viewer.frames.length > 0;
        });
        const snapshot = viewer.frames[0] as { kind: string; runs: unknown[]; machines: unknown[]; pulse: Record<string, unknown> };
        expect(snapshot.kind).toBe('snapshot');
        expect(Array.isArray(snapshot.runs)).toBe(true);
        expect(Array.isArray(snapshot.machines)).toBe(true);
        expect(Object.keys(snapshot.pulse).sort()).toEqual(['finishedLastMinute', 'longest', 'queued', 'running']);

        const run = datedRun('board-stream');
        await postPlan(run, await token(run, 'coordinator'), ['a', 'b']);
        await postEvents(run, await token(run, 'runner'), [event(run, 'a', 0, 'started', { machine: 'live-box' })]);
        let index = -1;
        await waitFor(function () {
            index = viewer.frames.findIndex(function (frame) {
                const summary = frame.run as { run: string; running: number } | undefined;
                return frame.kind === 'run' && summary?.run === run && summary.running === 1;
            });
            return index >= 0 && viewer.frames.slice(index + 1).some(function (frame) {
                return frame.kind === 'pulse';
            });
        });
        expect(viewer.frames[index]?.run).toMatchObject({ run: run, job: 'board-stream', units: 2, queued: 1, running: 1 });
        // Between the change and its pulse come only other changes.
        const following = viewer.frames.slice(index + 1);
        const pulseAt = following.findIndex(function (frame) {
            return frame.kind === 'pulse';
        });
        for (const frame of following.slice(0, pulseAt)) {
            expect(['run', 'machines']).toContain(frame.kind);
        }
        expect(Object.keys(following[pulseAt] ?? {}).sort()).toEqual(['finishedLastMinute', 'kind', 'longest', 'queued', 'running']);
        expect((following[pulseAt]?.running as number) >= 1).toBe(true);
        viewer.socket.close();
    });
});

describe('the board snapshot', function () {
    it('takes a board token in the Authorization header and nothing else', async function () {
        const board = await boardToken();
        const snapshot = await boardSnapshot(board);
        expect(snapshot.kind).toBe('snapshot');
        expect((await call('/board/snapshot')).status).toBe(401);
        expect((await call(`/board/snapshot?token=${encodeURIComponent(board)}`)).status).toBe(401);
        const run = freshRun();
        for (const scope of ['runner', 'viewer', 'coordinator'] as const) {
            expect((await call('/board/snapshot', { bearer: await token(run, scope) })).status, scope).toBe(403);
        }
        expect((await call('/board/snapshot', { bearer: await token(run, 'board') })).status).toBe(403);
        expect((await call('/board/snapshot', { method: 'POST', bearer: board })).status).toBe(405);
        expect((await call('/board/nothing', { bearer: board })).status).toBe(404);
    });
});

describe("the board's viewer tokens", function () {
    it("mints a viewer token for one run that opens that run's stream, expiring with the board token", async function () {
        const expires = Math.floor(Date.now() / 1000) + 600;
        const board = await boardToken(600);
        const run = freshRun();
        await postPlan(run, await token(run, 'coordinator'), ['a']);
        const response = await call(`/board/runs/${run}/viewer`, { method: 'POST', bearer: board });
        expect(response.status).toBe(200);
        const body = (await response.json()) as { token: string };
        expect(Object.keys(body)).toEqual(['token']);
        const verification = await verifyToken(TestSecret, body.token, Math.floor(Date.now() / 1000));
        expect(verification.valid).toBe(true);
        if (verification.valid) {
            expect(verification.claims.run).toBe(run);
            expect(verification.claims.scope).toBe('viewer');
            expect(Math.abs(verification.claims.expires - expires)).toBeLessThanOrEqual(1);
        }
        const viewer = await openViewer(run, body.token);
        await waitFor(function () {
            return viewer.frames.some(function (frame) {
                return frame.kind === 'caughtUp';
            });
        });
        expect(viewer.frames[0]).toEqual({ kind: 'plan', units: ['a'] });
        viewer.socket.close();
        // A viewer of that run and nothing more.
        expect((await postEvents(run, body.token, unitEvents(run, 'a'))).status).toBe(403);
        const other = freshRun();
        expect((await call(`/runs/${other}/stream?token=${encodeURIComponent(body.token)}`, { headers: { Upgrade: 'websocket' } })).status).toBe(403);
        expect((await call(`/runs/${run}?token=${encodeURIComponent(body.token)}`)).status).toBe(200);
    });

    it('refuses other scopes, a board token in the query, and a bad run id', async function () {
        const run = freshRun();
        const board = await boardToken();
        for (const scope of ['runner', 'viewer', 'coordinator'] as const) {
            expect((await call(`/board/runs/${run}/viewer`, { method: 'POST', bearer: await token(run, scope) })).status, scope).toBe(403);
        }
        expect((await call(`/board/runs/${run}/viewer`, { method: 'POST', bearer: await token(run, 'board') })).status).toBe(403);
        expect((await call(`/board/runs/${run}/viewer?token=${encodeURIComponent(board)}`, { method: 'POST' })).status).toBe(401);
        expect((await call(`/board/runs/${run}/viewer`, { bearer: board })).status).toBe(405);
        expect((await call('/board/runs/-bad/viewer', { method: 'POST', bearer: board })).status).toBe(400);
    });
});

describe("the board's retention", function () {
    it('drops a run 24 hours after its verdict, or after 24 hours with no event', async function () {
        const board = await boardToken();
        const decided = freshRun();
        const quiet = freshRun();
        const recent = freshRun();
        for (const run of [decided, quiet, recent]) {
            await postPlan(run, await token(run, 'coordinator'), ['a']);
        }
        await postVerdict(decided, await token(decided, 'coordinator'), verdictBody('void', [], ['a never ran']));
        await snapshotWhen(board, function (snapshot) {
            return runOf(snapshot, decided)?.verdict === 'void' && runOf(snapshot, quiet) !== undefined && runOf(snapshot, recent) !== undefined;
        });
        const now = Date.now();
        await runInDurableObject(boardStub(), function (_, state) {
            const sql = state.storage.sql;
            sql.exec('UPDATE runs SET verdictAt = ?, touchedAt = ? WHERE run = ?', now - RetentionMilliseconds - 1000, now, decided);
            sql.exec('UPDATE runs SET touchedAt = ? WHERE run = ?', now - RetentionMilliseconds - 1000, quiet);
            sql.exec('UPDATE runs SET touchedAt = ? WHERE run = ?', now - RetentionMilliseconds + 60_000, recent);
        });
        const snapshot = await boardSnapshot(board);
        expect(runOf(snapshot, decided)).toBeUndefined();
        expect(runOf(snapshot, quiet)).toBeUndefined();
        expect(runOf(snapshot, recent)).toBeDefined();
        // A verdict younger than a day keeps a run on the board even when its last push was long ago.
        const kept = freshRun();
        await postPlan(kept, await token(kept, 'coordinator'), ['a']);
        await postVerdict(kept, await token(kept, 'coordinator'), verdictBody('void', [], ['a never ran']));
        await snapshotWhen(board, function (candidate) {
            return runOf(candidate, kept)?.verdict === 'void';
        });
        await runInDurableObject(boardStub(), function (_, state) {
            state.storage.sql.exec('UPDATE runs SET touchedAt = ?, verdictAt = ? WHERE run = ?', now - RetentionMilliseconds - 1000, now - 1000, kept);
        });
        expect(runOf(await boardSnapshot(board), kept)).toBeDefined();
    });
});

describe('the board page', function () {
    it('is served without a token, under a nonce, and holds no data of its own', async function () {
        const response = await call('/board');
        expect(response.status).toBe(200);
        const policy = response.headers.get('Content-Security-Policy') ?? '';
        expect(policy).toContain("default-src 'none'");
        const html = await response.text();
        const nonce = /script-src 'nonce-([0-9a-f]+)'/.exec(policy)?.[1] ?? 'missing';
        expect(html).toContain(`<script nonce="${nonce}">`);
        expect(html).toContain("['loom', 'token.' + token]");
        expect((await call('/board', { method: 'POST' })).status).toBe(405);
    });
});
