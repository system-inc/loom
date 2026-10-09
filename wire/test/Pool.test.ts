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
        expect(await poolState(pool, coordinator)).toEqual({ queued: 0, workers: [] });
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
});
