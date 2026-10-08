import { env } from 'cloudflare:workers';
import { describe, expect, it } from 'vitest';
import {
    call,
    event,
    freshRun,
    openViewer,
    postEvents,
    postPlan,
    postVerdict,
    token,
    unitEvents,
    verdictBody,
    waitFor,
} from './Helpers';

const inputA = 'a'.repeat(64);
const inputB = 'b'.repeat(64);

type Frame = { kind: string; [field: string]: unknown };

function loggedEvents(frames: Frame[]): { unit: string; sequence: number }[] {
    return frames
        .filter(function (frame) {
            return frame.kind === 'event';
        })
        .map(function (frame) {
            const logged = frame.event as { unit: string; sequence: number };
            return { unit: logged.unit, sequence: logged.sequence };
        });
}

describe('the plan', function () {
    it('is set once: the same plan again is fine, a different one is refused', async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        expect((await postPlan(run, coordinator, ['a', 'b'])).status).toBe(201);
        expect((await postPlan(run, coordinator, ['a', 'b'])).status).toBe(200);
        expect((await postPlan(run, coordinator, ['a', 'b', 'c'])).status).toBe(409);
        expect((await postPlan(run, coordinator, ['b', 'a'])).status).toBe(409);
    });

    it('compares the inputs too when set once', async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const created = await postPlan(run, coordinator, ['a'], [inputA, inputB]);
        expect(created.status).toBe(201);
        expect(await created.json()).toMatchObject({ plan: 'set', units: 1, inputs: 2 });
        expect((await postPlan(run, coordinator, ['a'], [inputA, inputB])).status).toBe(200);
        expect((await postPlan(run, coordinator, ['a'], [inputA])).status).toBe(409);
        expect((await postPlan(run, coordinator, ['a'], [])).status).toBe(409);
        expect((await postPlan(run, coordinator, ['a'], [inputB, inputA])).status).toBe(409);
        // A plan with no inputs is set once like any other.
        const bare = freshRun();
        const bareCoordinator = await token(bare, 'coordinator');
        expect((await postPlan(bare, bareCoordinator, ['a'])).status).toBe(201);
        expect((await postPlan(bare, bareCoordinator, ['a'], [inputA])).status).toBe(409);
    });

    it('refuses a malformed plan', async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        for (const body of [
            '{"units":[],"inputs":[]}',
            '{"units":["a","a"],"inputs":[]}',
            '{"units":["a"],"inputs":[],"extra":1}',
            '{"units":[1],"inputs":[]}',
            '{"units":["a"]}', // v0.1's plan: inputs is required
            '{"inputs":[]}',
            '{"units":["a"],"inputs":null}',
            '{"units":["a"],"inputs":"' + inputA + '"}',
            '{"units":["a"],"inputs":["' + inputA.toUpperCase() + '"]}',
            '{"units":["a"],"inputs":["' + inputA.slice(1) + '"]}',
            '{"units":["a"],"inputs":["' + inputA + '","' + inputA + '"]}',
            '{"units":["a"],"inputs":[1]}',
            '{"Units":["a"],"Inputs":[]}',
            'nope',
        ]) {
            const response = await call(`/runs/${run}/plan`, { method: 'POST', bearer: coordinator, body: body });
            expect(response.status, body).toBe(400);
        }
        // Nothing malformed was set: the first good plan is still new.
        expect((await postPlan(run, coordinator, ['a'], [inputA])).status).toBe(201);
    });

    it('sends viewers the units, not the inputs', async function () {
        const run = freshRun();
        await postPlan(run, await token(run, 'coordinator'), ['a', 'b'], [inputA]);
        const viewer = await openViewer(run, await token(run, 'viewer'));
        await waitFor(function () {
            return viewer.frames.some(function (frame) {
                return frame.kind === 'caughtUp';
            });
        });
        expect(viewer.frames[0]).toEqual({ kind: 'plan', units: ['a', 'b'] });
        viewer.socket.close();
    });
});

describe('events', function () {
    it('drops an exact replay and keeps the first of a conflicting pair', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const events = unitEvents(run, 'a');
        const first = await postEvents(run, runner, events);
        expect(await first.json()).toMatchObject({ accepted: events.length, released: events.length, replays: 0 });
        // The whole batch again, and one line twice inside a batch.
        const again = await postEvents(run, runner, [...events, events[1] as Record<string, unknown>]);
        expect(again.status).toBe(200);
        expect(await again.json()).toMatchObject({ accepted: 0, released: 0, replays: events.length + 1 });
        // Same unit and sequence, different content: not a replay.
        const conflicting = await postEvents(run, runner, [event(run, 'a', 1, 'output', { stream: 'stdout', text: 'other' })]);
        expect(conflicting.status).toBe(409);
        const viewer = await openViewer(run, await token(run, 'viewer'));
        await waitFor(function () {
            return viewer.frames.some(function (frame) {
                return frame.kind === 'caughtUp';
            });
        });
        const logged = viewer.frames.filter(function (frame) {
            return frame.kind === 'event';
        });
        expect(logged).toHaveLength(events.length);
        expect((logged[1]?.event as { text: string }).text).toBe('a line 0');
        viewer.socket.close();
    });

    it('holds out-of-order events until the gap fills, then releases them in order', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const events = unitEvents(run, 'a', 'passed', 4); // sequences 0 to 6
        const pick = function (...sequences: number[]) {
            return sequences.map(function (sequence) {
                return events[sequence] as Record<string, unknown>;
            });
        };
        expect(await (await postEvents(run, runner, pick(3, 5, 6))).json()).toMatchObject({ accepted: 3, released: 0, holding: 3 });
        expect(await (await postEvents(run, runner, pick(0, 2))).json()).toMatchObject({ accepted: 2, released: 1, holding: 4 });
        expect(await (await postEvents(run, runner, pick(1))).json()).toMatchObject({ accepted: 1, released: 3, holding: 2 });
        expect(await (await postEvents(run, runner, pick(4))).json()).toMatchObject({ accepted: 1, released: 3, holding: 0 });
        const viewer = await openViewer(run, await token(run, 'viewer'));
        await waitFor(function () {
            return viewer.frames.some(function (frame) {
                return frame.kind === 'caughtUp';
            });
        });
        expect(loggedEvents(viewer.frames).map(function (logged) {
            return logged.sequence;
        })).toEqual([0, 1, 2, 3, 4, 5, 6]);
        viewer.socket.close();
    });

    it('refuses an event of another run, and the whole batch with it', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const good = unitEvents(run, 'a');
        const stray = event('r-other', 'a', good.length, 'output', { stream: 'stdout', text: 'x' });
        const response = await postEvents(run, runner, [...good, stray]);
        expect(response.status).toBe(400);
        expect(((await response.json()) as { error: string }).error).toContain('r-other');
        // Nothing from the refused batch landed.
        expect(await (await postEvents(run, runner, good)).json()).toMatchObject({ accepted: good.length });
    });

    it('refuses events that break the schema', async function () {
        const run = freshRun();
        const runner = await token(run, 'runner');
        const broken = [
            { ...event(run, 'a', 0, 'started'), surprise: true },
            event(run, 'a', 0, 'nonsense'),
            event(run, 'a', -1, 'started'),
            { ...event(run, 'a', 0, 'started'), time: '2026-10-08 12:00:00' },
            { ...event(run, 'a', 0, 'started'), time: '2026-10-08T12:00:00+02:00' },
            event(run, 'a', 0, 'output', { text: 'no stream' }),
            event(run, 'a', 0, 'finished', { status: 'fine' }),
            event(run, 'a', 0, 'uploaded', { path: 'x', sha256: 'abc' }),
            event(run, '', 0, 'started'),
            event(run, 'a', 0, 'error', { phase: 'elsewhere' }),
            event(run, 'a', 0, 'cached', { fromRun: 'r-earlier', events: inputB }),
            event(run, 'a', 0, 'cached', { key: inputA, events: inputB }),
            event(run, 'a', 0, 'cached', { key: inputA, fromRun: 'r-earlier' }),
            event(run, 'a', 0, 'cached', { key: inputA.toUpperCase(), fromRun: 'r-earlier', events: inputB }),
            event(run, 'a', 0, 'cached', { key: inputA, fromRun: '', events: inputB }),
            event(run, 'a', 0, 'cached', { key: inputA, fromRun: 7, events: inputB }),
            event(run, 'a', 0, 'cached', { key: inputA, fromRun: 'r-earlier', events: 'abc' }),
            event(run, 'a', 0, 'cached', { key: inputA, fromRun: 'r-earlier', events: inputB, status: 'passed' }),
        ];
        for (const line of broken) {
            const response = await postEvents(run, runner, [line]);
            expect(response.status, JSON.stringify(line)).toBe(400);
        }
    });

    it('takes a cached unit: cached, then finished passed', async function () {
        const run = freshRun();
        // On a hit the coordinator posts the unit's stream itself.
        const coordinator = await token(run, 'coordinator');
        const stream = [
            event(run, 'a', 0, 'cached', { key: inputA, fromRun: 'r-earlier', events: inputB }),
            event(run, 'a', 1, 'finished', { status: 'passed' }),
        ];
        const response = await postEvents(run, coordinator, stream);
        expect(response.status, await response.clone().text()).toBe(200);
        expect(await response.json()).toMatchObject({ accepted: 2, released: 2 });
    });

    it("takes the coordinator's place error", async function () {
        const run = freshRun();
        const placed = event(run, 'a', 0, 'error', { phase: 'place', message: 'box-1 dropped the unit; placed on box-2' });
        expect((await postEvents(run, await token(run, 'coordinator'), [placed])).status).toBe(200);
    });

    it('takes the verdict from the coordinator, archives the log to R2, then takes no more events', async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const runner = await token(run, 'runner');
        const verdict = { status: 'green', failed: [], problems: [], cached: [] };
        // No verdict before a plan, and never from a runner.
        expect((await call(`/runs/${run}/verdict`, { method: 'POST', bearer: coordinator, body: JSON.stringify(verdict) })).status).toBe(409);
        await postPlan(run, coordinator, ['a', 'b']);
        await postEvents(run, runner, [...unitEvents(run, 'b'), ...unitEvents(run, 'a')]);
        expect((await call(`/runs/${run}/verdict`, { method: 'POST', bearer: runner, body: JSON.stringify(verdict) })).status).toBe(403);
        // Exactly the lowercase keys protocol.Verdict marshals, with [] for an empty list.
        for (const body of [
            { Status: 'green', Failed: [], Problems: [], Cached: [] },
            { status: 'green', failed: null, problems: null, cached: null },
            { status: 'green', failed: [] },
            { status: 'green', failed: [], problems: [] }, // v0.1's verdict: cached is required
            { status: 'green', failed: [], problems: [], cached: null },
            { status: 'green', failed: [], problems: [], cached: [1] },
            { status: 'green', failed: [], problems: [], cached: 'a' },
            { status: 'green', failed: [], problems: [], cached: [], extra: 1 },
            { status: 'fine', failed: [], problems: [], cached: [] },
            { status: 'red', failed: [1], problems: [], cached: [] },
        ]) {
            const response = await call(`/runs/${run}/verdict`, { method: 'POST', bearer: coordinator, body: JSON.stringify(body) });
            expect(response.status, JSON.stringify(body)).toBe(400);
        }
        expect((await call(`/runs/${run}/verdict`, { method: 'POST', bearer: coordinator, body: JSON.stringify(verdict) })).status).toBe(201);
        expect((await call(`/runs/${run}/verdict`, { method: 'POST', bearer: coordinator, body: JSON.stringify(verdict) })).status).toBe(200);
        const different = { status: 'void', failed: [], problems: ['x'], cached: [] };
        expect((await call(`/runs/${run}/verdict`, { method: 'POST', bearer: coordinator, body: JSON.stringify(different) })).status).toBe(409);
        // The same status with different cached units is a different verdict.
        expect((await postVerdict(run, coordinator, verdictBody('green', [], [], ['a']))).status).toBe(409);
        const archive = await env.Store.get(`runs/${run}/events.jsonl`);
        expect(archive).not.toBeNull();
        const lines = (await archive?.text())?.trim().split('\n') ?? [];
        expect(lines).toHaveLength(10);
        expect(JSON.parse(lines[0] ?? '{}')).toMatchObject({ unit: 'b', sequence: 0 });
        // A run that uploaded nothing still gets its uploads list, empty.
        expect(await (await env.Store.get(`runs/${run}/blobs.jsonl`))?.text()).toBe('');
        expect((await postEvents(run, runner, [event(run, 'c', 0, 'started')])).status).toBe(409);
    });
});

describe('the stream', function () {
    it('gives a late joiner the plan, every event so far in order, then live events, then the verdict', async function () {
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const runner = await token(run, 'runner');
        await postPlan(run, coordinator, ['a', 'b']);
        const a = unitEvents(run, 'a');
        const b = unitEvents(run, 'b', 'failed');
        // Before anyone watches: all of a, and b out of order with its first event missing.
        await postEvents(run, runner, a);
        await postEvents(run, runner, b.slice(1, 3));

        const early = await openViewer(run, await token(run, 'viewer'));
        await waitFor(function () {
            return early.frames.some(function (frame) {
                return frame.kind === 'caughtUp';
            });
        });
        // The held events of b are not in the tail yet.
        expect(early.frames.map(function (frame) {
            return frame.kind;
        })).toEqual(['plan', ...a.map(function () {
            return 'event';
        }), 'caughtUp']);

        await postEvents(run, runner, b.slice(0, 1)); // fills the gap, releasing b 0, 1 and 2
        await postEvents(run, runner, b.slice(3));
        await call(`/runs/${run}/verdict`, {
            method: 'POST',
            bearer: coordinator,
            body: verdictBody('red', ['b'], [], ['a']),
        });
        await waitFor(function () {
            return early.frames.some(function (frame) {
                return frame.kind === 'verdict';
            });
        });
        const expected = [...a, ...b].map(function (logged) {
            return { unit: logged.unit as string, sequence: logged.sequence as number };
        });
        expect(loggedEvents(early.frames)).toEqual(expected);
        expect(early.frames.at(-1)).toEqual({ kind: 'verdict', verdict: { status: 'red', failed: ['b'], problems: [], cached: ['a'] } });

        // Someone who joins after the verdict gets the same story in the same order, with positions counting up.
        const late = await openViewer(run, await token(run, 'viewer'));
        await waitFor(function () {
            return late.frames.some(function (frame) {
                return frame.kind === 'verdict';
            });
        });
        expect(late.frames[0]).toMatchObject({ kind: 'plan', units: ['a', 'b'] });
        expect(loggedEvents(late.frames)).toEqual(expected);
        const positions = late.frames
            .filter(function (frame) {
                return frame.kind === 'event';
            })
            .map(function (frame) {
                return frame.position as number;
            });
        expect(positions).toEqual(expected.map(function (_, index) {
            return index + 1;
        }));
        expect(late.frames.map(function (frame) {
            return frame.kind;
        }).slice(-2)).toEqual(['caughtUp', 'verdict']);

        // A viewer resuming after position 5 gets only what came later.
        const resumed = await call(`/runs/${run}/stream?after=5&token=${encodeURIComponent(await token(run, 'viewer'))}`, {
            headers: { Upgrade: 'websocket' },
        });
        expect(resumed.status).toBe(101);
        early.socket.close();
        late.socket.close();
        resumed.webSocket?.accept();
        resumed.webSocket?.close();
    });

    it('needs a WebSocket upgrade', async function () {
        const run = freshRun();
        const response = await call(`/runs/${run}/stream?token=${encodeURIComponent(await token(run, 'viewer'))}`);
        expect(response.status).toBe(426);
    });
});
