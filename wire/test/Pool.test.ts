import { env } from 'cloudflare:workers';
import { runInDurableObject } from 'cloudflare:test';
import { describe, expect, it } from 'vitest';
import { PoolWaitMilliseconds, WorkerWindowMilliseconds, type PoolWorker } from '../source/Pool';
import { boardToken, call, freshRun, token } from './Helpers';

function freshPool(): string {
    return 'pool-' + crypto.randomUUID();
}

// A whole unit the way the coordinator sends one: the wire reads its run and id, and hands the rest on untouched.
function poolUnit(run: string, unit: string): Record<string, unknown> {
    return {
        run: run,
        unit: unit,
        argv: ['bash', '-c', `echo ${unit} <&>`],
        environment: { SHARD: unit },
        timeoutSeconds: 600,
        resources: { cpus: 4, memoryMegabytes: 2048 },
        store: { url: `https://wire.test/runs/${run}/blobs` },
        wire: { url: `https://wire.test/runs/${run}/events` },
        token: 'a-run-token',
    };
}

function postUnits(pool: string, bearer: string, units: unknown): Promise<Response> {
    return call(`/pools/${pool}/units`, {
        method: 'POST',
        bearer: bearer,
        body: typeof units === 'string' ? units : JSON.stringify({ units: units }),
    });
}

function next(pool: string, bearer: string, worker: string, cpus = 8): Promise<Response> {
    return call(`/pools/${pool}/next`, { method: 'POST', bearer: bearer, body: JSON.stringify({ worker: worker, cpus: cpus }) });
}

async function poolState(pool: string, bearer: string): Promise<{ queued: number; workers: PoolWorker[] }> {
    const response = await call(`/pools/${pool}`, { bearer: bearer });
    expect(response.status, await response.clone().text()).toBe(200);
    return (await response.json()) as { queued: number; workers: PoolWorker[] };
}

// A worker is recorded the moment its ask lands, before it waits, so once the pool lists this many workers each of
// their asks is waiting in the order they were listed.
async function waitForWorkers(pool: string, bearer: string, count: number): Promise<void> {
    const deadline = Date.now() + 3000;
    while ((await poolState(pool, bearer)).workers.length < count) {
        if (Date.now() > deadline) {
            throw new Error(`timed out waiting for ${count} workers to ask`);
        }
        await new Promise(function (resolve) {
            setTimeout(resolve, 10);
        });
    }
}

// Shortens how long a next waits, on this pool's object only; production keeps 20 s.
async function waitOf(pool: string, milliseconds: number): Promise<void> {
    await runInDurableObject(env.Pools.get(env.Pools.idFromName(pool)), function (instance) {
        instance.waitMilliseconds = milliseconds;
    });
}

describe('a pool', function () {
    it('waits 20 s in production', function () {
        expect(PoolWaitMilliseconds).toBe(20_000);
        expect(WorkerWindowMilliseconds).toBe(4 * 60 * 60 * 1000);
    });

    it('queues units in the order given and hands them out first in, first out, exactly as given', async function () {
        const pool = freshPool();
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        const run = freshRun();
        const units = [poolUnit(run, 'tests[shard=2]'), poolUnit(run, 'tests[shard=0]'), poolUnit(run, 'build')];
        const first = await postUnits(pool, coordinator, units.slice(0, 2));
        expect(first.status, await first.clone().text()).toBe(200);
        expect(await first.json()).toEqual({ queued: 2 });
        expect(await (await postUnits(pool, coordinator, units.slice(2))).json()).toEqual({ queued: 3 });
        expect(await (await postUnits(pool, coordinator, [])).json()).toEqual({ queued: 3 });
        expect(await (await postUnits(pool, coordinator, '{"units":null}')).json()).toEqual({ queued: 3 });
        for (const unit of units) {
            const taken = await next(pool, member, 'instance-1');
            expect(taken.status).toBe(200);
            const text = await taken.text();
            expect(JSON.parse(text)).toEqual(unit);
            expect(text).toBe(JSON.stringify(unit) + '\n');
        }
        expect((await poolState(pool, coordinator)).queued).toBe(0);
    });

    it('hands out the highest priority first, and first in, first out within one priority', async function () {
        const pool = freshPool();
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        const side = freshRun();
        const main = freshRun();
        const star = freshRun();
        // A batch without a priority is 0, as Go's omitempty leaves a zero off; a null batch is still none.
        await postUnits(pool, coordinator, [poolUnit(side, 'a'), poolUnit(side, 'b')]);
        await postUnits(pool, coordinator, JSON.stringify({ units: [poolUnit(main, 'a')], priority: 20 }));
        await postUnits(pool, coordinator, JSON.stringify({ units: [poolUnit(star, 'a'), poolUnit(star, 'b')], priority: 30 }));
        await postUnits(pool, coordinator, JSON.stringify({ units: [poolUnit(main, 'b')], priority: 20 }));
        expect(await (await postUnits(pool, coordinator, '{"units":null,"priority":30}')).json()).toEqual({ queued: 6 });
        const order: string[] = [];
        for (let index = 0; index < 4; index++) {
            const taken = (await (await next(pool, member, 'instance-1')).json()) as { run: string; unit: string };
            order.push(`${[side, main, star].indexOf(taken.run)}:${taken.unit}`);
        }
        expect(order).toEqual(['2:a', '2:b', '1:a', '1:b']);
        // A later star unit passes the side units still waiting.
        await postUnits(pool, coordinator, JSON.stringify({ units: [poolUnit(star, 'c')], priority: 30 }));
        for (const expected of [[star, 'c'], [side, 'a'], [side, 'b']]) {
            expect(await (await next(pool, member, 'instance-1')).json()).toMatchObject({ run: expected[0], unit: expected[1] });
        }
    });

    it('adds the priority column to a pool made before priorities, its queued units at 0', async function () {
        const pool = freshPool();
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        const old = freshRun();
        const star = freshRun();
        const unit = poolUnit(old, 'a');
        await runInDurableObject(env.Pools.get(env.Pools.idFromName(pool)), function (instance, state) {
            state.storage.sql.exec(`
                DROP TABLE units;
                CREATE TABLE units (position INTEGER PRIMARY KEY, run TEXT NOT NULL, unit TEXT NOT NULL, json TEXT NOT NULL);
            `);
            state.storage.sql.exec('INSERT INTO units (run, unit, json) VALUES (?, ?, ?)', old, 'a', JSON.stringify(unit));
            // The constructor is what a deploy runs on the live object.
            new (instance.constructor as new (state: DurableObjectState, environment: Env) => unknown)(state, env);
            const columns = state.storage.sql.exec<{ name: string }>('PRAGMA table_info(units)').toArray();
            expect(
                columns.map(function (column) {
                    return column.name;
                }),
            ).toContain('priority');
        });
        await postUnits(pool, coordinator, JSON.stringify({ units: [poolUnit(star, 'a')], priority: 30 }));
        expect(await (await next(pool, member, 'instance-1')).json()).toMatchObject({ run: star, unit: 'a' });
        expect(await (await next(pool, member, 'instance-1')).json()).toEqual(unit);
    });

    it('answers 204 when no unit arrives within the wait', async function () {
        const pool = freshPool();
        await waitOf(pool, 200);
        const started = Date.now();
        const response = await next(pool, await token(pool, 'pool'), 'idle-instance');
        expect(response.status).toBe(204);
        expect(await response.text()).toBe('');
        expect(Date.now() - started).toBeGreaterThanOrEqual(190);
    });

    it('wakes a waiting next when units arrive, the oldest waiter first', async function () {
        const pool = freshPool();
        await waitOf(pool, 4000);
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        const run = freshRun();
        const started = Date.now();
        const older = next(pool, member, 'instance-old', 16);
        await waitForWorkers(pool, coordinator, 1);
        const newer = next(pool, member, 'instance-new', 4);
        await waitForWorkers(pool, coordinator, 2);
        expect(await (await postUnits(pool, coordinator, [poolUnit(run, 'first')])).json()).toEqual({ queued: 0 });
        const olderAnswer = await older;
        expect(olderAnswer.status).toBe(200);
        expect(await olderAnswer.json()).toMatchObject({ unit: 'first' });
        expect(Date.now() - started).toBeLessThan(4000);
        await postUnits(pool, coordinator, [poolUnit(run, 'second'), poolUnit(run, 'third')]);
        const newerAnswer = await newer;
        expect(newerAnswer.status).toBe(200);
        expect(await newerAnswer.json()).toMatchObject({ unit: 'second' });
        // The third waits in the queue for whoever asks next.
        const state = await poolState(pool, coordinator);
        expect(state.queued).toBe(1);
        expect(state.workers.map(function (worker) {
            return { worker: worker.worker, cpus: worker.cpus, took: worker.took };
        })).toEqual([
            { worker: 'instance-new', cpus: 4, took: 'second' },
            { worker: 'instance-old', cpus: 16, took: 'first' },
        ]);
    });

    it("answers a worker's earlier ask 204 when the same worker asks again", async function () {
        const pool = freshPool();
        await waitOf(pool, 4000);
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        const dropped = next(pool, member, 'instance-1');
        await waitForWorkers(pool, coordinator, 1);
        const again = next(pool, member, 'instance-1');
        expect((await dropped).status).toBe(204);
        await postUnits(pool, coordinator, [poolUnit(freshRun(), 'only')]);
        expect(await (await again).json()).toMatchObject({ unit: 'only' });
    });

    it("drops one run's queued units on cancel", async function () {
        const pool = freshPool();
        await waitOf(pool, 100);
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        const cancelled = freshRun();
        const kept = freshRun();
        await postUnits(pool, coordinator, [poolUnit(cancelled, 'a'), poolUnit(kept, 'a'), poolUnit(cancelled, 'b')]);
        const cancel = function (body: string): Promise<Response> {
            return call(`/pools/${pool}/cancel`, { method: 'POST', bearer: coordinator, body: body });
        };
        const response = await cancel(JSON.stringify({ run: cancelled }));
        expect(response.status).toBe(200);
        expect(await response.json()).toEqual({ dropped: 2 });
        expect(await (await cancel(JSON.stringify({ run: cancelled }))).json()).toEqual({ dropped: 0 });
        for (const body of ['nope', '{}', '{"run":""}', '{"run":"-bad"}', `{"run":"${kept}","extra":1}`, `{"Run":"${kept}"}`]) {
            expect((await cancel(body)).status, body).toBe(400);
        }
        expect((await poolState(pool, coordinator)).queued).toBe(1);
        expect(await (await next(pool, member, 'instance-1')).json()).toMatchObject({ run: kept, unit: 'a' });
        expect((await next(pool, member, 'instance-1')).status).toBe(204);
    });

    it('refuses a retired worker with 403 and hands its units to others, until it is restored', async function () {
        const pool = freshPool();
        await waitOf(pool, 100);
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        const run = freshRun();
        const operation = function (name: string, body: string, bearer = coordinator): Promise<Response> {
            return call(`/pools/${pool}/${name}`, { method: 'POST', bearer: bearer, body: body });
        };
        const retired = await operation('retire', JSON.stringify({ worker: 'full-disk', reason: 'its /tmp is full' }));
        expect(retired.status).toBe(200);
        expect(await retired.json()).toEqual({ retired: ['full-disk'] });
        await postUnits(pool, coordinator, [poolUnit(run, 'a'), poolUnit(run, 'b')]);
        const refused = await next(pool, member, 'full-disk');
        expect(refused.status).toBe(403);
        expect(await refused.json()).toEqual({ error: 'worker full-disk is retired from this pool: its /tmp is full' });
        expect((await poolState(pool, coordinator)).queued).toBe(2);
        expect(await (await next(pool, member, 'healthy')).json()).toMatchObject({ run: run, unit: 'a' });
        // A retired worker is no capacity: the listing leaves it out, though it asked.
        expect((await poolState(pool, coordinator)).workers.map(function (worker) {
            return worker.worker;
        })).toEqual(['healthy']);
        for (const body of ['nope', '{}', '{"worker":"x"}', '{"worker":"","reason":"r"}', '{"worker":"x","reason":""}', '{"worker":"x","reason":"r","extra":1}']) {
            expect((await operation('retire', body)).status, body).toBe(400);
        }
        expect((await operation('restore', '{"worker":"x","reason":"r"}')).status).toBe(400);
        expect((await operation('retire', JSON.stringify({ worker: 'x', reason: 'r' }), member)).status).toBe(403);
        const restored = await operation('restore', JSON.stringify({ worker: 'full-disk' }));
        expect(await restored.json()).toEqual({ retired: [] });
        expect(await (await next(pool, member, 'full-disk')).json()).toMatchObject({ run: run, unit: 'b' });
        expect((await poolState(pool, coordinator)).workers.map(function (worker) {
            return worker.worker;
        })).toEqual(['full-disk', 'healthy']);
    });

    it("lists one run's units still queued, oldest first, and none once a worker takes them", async function () {
        const pool = freshPool();
        await waitOf(pool, 100);
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        const listed = freshRun();
        const other = freshRun();
        await postUnits(pool, coordinator, [poolUnit(listed, 'b'), poolUnit(other, 'a'), poolUnit(listed, 'a')]);
        const queued = function (body: string): Promise<Response> {
            return call(`/pools/${pool}/queued`, { method: 'POST', bearer: coordinator, body: body });
        };
        expect(await (await queued(JSON.stringify({ run: listed }))).json()).toEqual({ units: ['b', 'a'] });
        expect(await (await next(pool, member, 'instance-1')).json()).toMatchObject({ run: listed, unit: 'b' });
        expect(await (await queued(JSON.stringify({ run: listed }))).json()).toEqual({ units: ['a'] });
        expect(await (await queued(JSON.stringify({ run: freshRun() }))).json()).toEqual({ units: [] });
        for (const body of ['nope', '{}', '{"run":"-bad"}', `{"run":"${listed}","extra":1}`]) {
            expect((await queued(body)).status, body).toBe(400);
        }
    });

    it('lists the workers seen in the last four hours, to a coordinator or a board token', async function () {
        const pool = freshPool();
        await waitOf(pool, 50);
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        expect(await poolState(pool, coordinator)).toEqual({ queued: 0, workers: [], live: [] });
        await postUnits(pool, coordinator, [poolUnit(freshRun(), 'a'), poolUnit(freshRun(), 'b')]);
        const before = Date.now();
        await (await next(pool, member, 'instance-1', 32)).body?.cancel();
        await (await next(pool, member, 'instance-2')).body?.cancel();
        await (await next(pool, member, 'instance-3', 2)).body?.cancel(); // finds the queue empty: took nothing
        await runInDurableObject(env.Pools.get(env.Pools.idFromName(pool)), function (_, state) {
            state.storage.sql.exec('UPDATE workers SET seenAt = ? WHERE worker = ?', Date.now() - WorkerWindowMilliseconds - 1000, 'instance-2');
        });
        const state = await poolState(pool, await boardToken());
        expect(state.queued).toBe(0);
        expect(state.workers.map(function (worker) {
            return { worker: worker.worker, cpus: worker.cpus, took: worker.took };
        })).toEqual([
            { worker: 'instance-1', cpus: 32, took: 'a' },
            { worker: 'instance-3', cpus: 2, took: '' },
        ]);
        for (const worker of state.workers) {
            expect(Object.keys(worker)).toEqual(['worker', 'cpus', 'seenAt', 'took']);
            expect(Date.parse(worker.seenAt)).toBeGreaterThanOrEqual(before - 1000);
        }
        // A zero cpus may be left off, as Go's omitempty leaves it.
        const omitted = await call(`/pools/${pool}/next`, { method: 'POST', bearer: member, body: '{"worker":"instance-4"}' });
        expect(omitted.status).toBe(204);
        expect((await poolState(pool, coordinator)).workers.find(function (worker) {
            return worker.worker === 'instance-4';
        })?.cpus).toBe(0);
    });

    it('refuses malformed units and asks, and queues nothing from a refused batch', async function () {
        const pool = freshPool();
        const coordinator = await token(freshRun(), 'coordinator');
        const member = await token(pool, 'pool');
        const run = freshRun();
        const good = poolUnit(run, 'a');
        for (const body of [
            'nope',
            '[]',
            '{}',
            JSON.stringify({ units: [good], extra: 1 }),
            JSON.stringify({ Units: [good] }),
            JSON.stringify({ units: good }),
            JSON.stringify({ units: [good, 'b'] }),
            JSON.stringify({ units: [good, [good]] }),
            JSON.stringify({ units: [good, { ...good, run: '-bad' }] }),
            JSON.stringify({ units: [good, { ...good, run: 7 }] }),
            JSON.stringify({ units: [good, { unit: 'b' }] }),
            JSON.stringify({ units: [good, { ...good, unit: '' }] }),
            JSON.stringify({ units: [good, { ...good, unit: 'u'.repeat(257) }] }),
            JSON.stringify({ units: [good, { run: run }] }),
            JSON.stringify({ priority: 3 }),
            JSON.stringify({ units: [good], priority: -1 }),
            JSON.stringify({ units: [good], priority: 1.5 }),
            JSON.stringify({ units: [good], priority: '30' }),
            JSON.stringify({ units: [good], priority: 1001 }),
        ]) {
            const response = await postUnits(pool, coordinator, body);
            expect(response.status, body).toBe(400);
        }
        expect((await postUnits(pool, coordinator, [{ ...good, argv: ['x'.repeat(1024 * 1024)] }])).status).toBe(400);
        expect((await poolState(pool, coordinator)).queued).toBe(0);
        for (const body of ['nope', '{}', '{"worker":""}', '{"worker":7}', '{"worker":"w","cpus":-1}', '{"worker":"w","cpus":1.5}', '{"worker":"w","cpus":"8"}', '{"worker":"w","cpus":8,"extra":1}', '{"Worker":"w","cpus":8}']) {
            const response = await call(`/pools/${pool}/next`, { method: 'POST', bearer: member, body: body });
            expect(response.status, body).toBe(400);
        }
        expect((await call(`/pools/-bad/next`, { method: 'POST', bearer: member, body: '{"worker":"w"}' })).status).toBe(400);
        expect((await call(`/pools/${pool}/next`, { bearer: member })).status).toBe(405);
        expect((await call(`/pools/${pool}`, { method: 'POST', bearer: coordinator })).status).toBe(405);
        expect((await call(`/pools/${pool}/elsewhere`, { method: 'POST', bearer: coordinator })).status).toBe(404);
    });

    it("takes only the pool's own pool token for next, and only a coordinator's units and cancel", async function () {
        const pool = freshPool();
        const run = freshRun();
        const coordinator = await token(run, 'coordinator');
        const member = await token(pool, 'pool');
        const ask = JSON.stringify({ worker: 'w', cpus: 1 });
        expect((await next(pool, await token(freshPool(), 'pool'), 'w')).status).toBe(403);
        for (const scope of ['runner', 'viewer', 'coordinator', 'board'] as const) {
            expect((await next(pool, await token(pool, scope), 'w')).status, scope).toBe(403);
        }
        expect((await call(`/pools/${pool}/next?token=${encodeURIComponent(member)}`, { method: 'POST', body: ask })).status).toBe(401);
        expect((await call(`/pools/${pool}/next`, { method: 'POST', body: ask })).status).toBe(401);
        for (const bearer of [member, await token(run, 'runner'), await token(run, 'viewer'), await boardToken()]) {
            expect((await postUnits(pool, bearer, [poolUnit(run, 'a')])).status).toBe(403);
            expect((await call(`/pools/${pool}/cancel`, { method: 'POST', bearer: bearer, body: JSON.stringify({ run: run }) })).status).toBe(403);
            expect((await call(`/pools/${pool}/queued`, { method: 'POST', bearer: bearer, body: JSON.stringify({ run: run }) })).status).toBe(403);
        }
        for (const bearer of [member, await token(run, 'runner'), await token(run, 'viewer')]) {
            expect((await call(`/pools/${pool}`, { bearer: bearer })).status).toBe(403);
        }
        expect((await poolState(pool, coordinator)).queued).toBe(0);
    });

    // A worker's live status (#yk0q0kj): the pool's own pool token posts it, the newest per worker replaces the last,
    // and a board or coordinator token reads them beside the workers. Mutants: another pool's token or a board token
    // posting; an unknown field or a negative count kept; the older status kept over the newer.
    it("keeps each worker's newest live status from its own pool token, for the board to read", async function () {
        const pool = freshPool();
        const member = await token(pool, 'pool');
        const status = (units: number) => ({
            worker: 'Cloud-7b29b4',
            release: 'v2026.10.10-1',
            runner: 'a'.repeat(64),
            startedAt: '2026-10-10T20:00:00.000Z',
            units: Array.from({ length: units }, (_, index) => ({ run: 'future-f-1', unit: 'u' + index, package: 'bridge/tsgo', phase: 'testing',
                startedAt: '2026-10-10T20:01:00.000Z', deadline: '2026-10-10T20:11:00.000Z', cpus: 8, memoryMegabytes: 16384 })),
            slots: { units: 8, cpus: 64, memoryMegabytes: 115000, heldCpus: 8 * units, heldMemoryMegabytes: 16384 * units },
            disk: { freeMegabytes: 300000, floorMegabytes: 1500 },
            cache: { blobBytes: 1 << 30, blobLimitBytes: 4 << 30 },
            totals: { units: 12, passed: 11, failed: 1, broken: 0 },
        });
        const post = (bearer: string, body: unknown) => call(`/pools/${pool}/live`, { method: 'POST', bearer: bearer, body: typeof body === 'string' ? body : JSON.stringify(body) });
        expect((await post(member, status(2))).status).toBe(200);
        expect((await post(member, status(3))).status).toBe(200);
        expect((await post(member, { worker: 'Home-f3279c', startedAt: '2026-10-10T19:00:00.000Z' })).status).toBe(200);
        for (const bearer of [await boardToken(), await token(freshRun(), 'coordinator')]) {
            const state = (await (await call(`/pools/${pool}`, { bearer: bearer })).json()) as { live: { worker: string; at: string; status: { units: unknown[] } }[] };
            expect(state.live.map((row) => row.worker)).toEqual(['Cloud-7b29b4', 'Home-f3279c']);
            expect(state.live[0]!.status.units.length).toBe(3);
            expect(Date.parse(state.live[0]!.at)).toBeGreaterThan(0);
        }
        for (const bearer of [await token(freshPool(), 'pool'), await boardToken(), await token(freshRun(), 'coordinator'), await token(freshRun(), 'runner')]) {
            expect((await post(bearer, status(1))).status).toBe(403);
        }
        const broken: unknown[] = [
            { ...status(1), surprise: true },
            { ...status(1), worker: '' },
            { ...status(1), startedAt: '2026-10-10 20:00' },
            { release: 'x', startedAt: '2026-10-10T20:00:00.000Z' },
            { ...status(1), runner: 'v0-dev' },
            { ...status(1), slots: { units: -1 } },
            { ...status(1), disk: { freeMegabytes: 1.5 } },
            { ...status(1), cache: { blobBytes: 1, extra: 2 } },
            { ...status(1), units: [{ run: 'future-f-1', unit: 'u', startedAt: '2026-10-10T20:01:00.000Z', surprise: 1 }] },
            { ...status(1), units: [{ run: 'future-f-1', startedAt: '2026-10-10T20:01:00.000Z' }] },
            { ...status(1), units: 'many' },
            status(65),
            'not json',
        ];
        for (const body of broken) {
            const response = await post(member, body);
            expect([400, 413], JSON.stringify(body).slice(0, 120)).toContain(response.status);
        }
        const state = (await (await call(`/pools/${pool}`, { bearer: await boardToken() })).json()) as { live: { status: { units: unknown[] } }[] };
        expect(state.live[0]!.status.units.length).toBe(3);
    });
});
