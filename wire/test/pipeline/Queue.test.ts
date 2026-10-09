import { env } from 'cloudflare:workers';
import { runDurableObjectAlarm, runInDurableObject } from 'cloudflare:test';
import { describe, expect, it } from 'vitest';
import { call, token } from '../Helpers';
import { canonical, futureLandable, GenesisHash, MaximumStackDepth, replay, sha256Text, unitKeyOf, type FutureEntry, type GitFacts, type Queue, type QueueEvent, type UnitVerdict } from '../../source/Queue';

const main = 'a'.repeat(40);

function sha(seed: number): string {
    return seed.toString(16).padStart(40, '0');
}

// A stand-in for GitHub: every sha exists, descends from main, and touches the paths it's given, unless told otherwise.
function facts(overrides: Partial<GitFacts> = {}, diffPaths = ['internal/lower/a.go', 'internal/lower/a_test.go']): GitFacts {
    return { shaExists: true, baseIsAncestor: true, baseOnMain: true, diffPaths: diffPaths, ...overrides };
}

// A fresh queue object per test, with git answered from a table keyed by sha and a pinned clock.
async function freshQueue(answers: Record<string, GitFacts> = {}): Promise<DurableObjectStub<Queue>> {
    // The pipeline's binding (pipeline.jsonc); the generated Env type knows only loom-wire's.
    const namespace = (env as unknown as { Queue: DurableObjectNamespace<Queue> }).Queue;
    const stub = namespace.get(namespace.idFromName('queue-' + crypto.randomUUID()));
    await runInDurableObject(stub, function (instance: Queue) {
        instance.history = {
            // A base other than the main these tests start from is one main has moved past: nothing descends from it here.
            async facts(asked: string, base: string): Promise<GitFacts> {
                return answers[asked] ?? (base === main ? facts() : facts({ baseIsAncestor: false }));
            },
        };
        let tick = 0;
        instance.now = function () {
            tick++;
            return new Date(1791590000000 + tick * 1000).toISOString();
        };
    });
    return stub;
}

function submit(queue: DurableObjectStub<Queue>, change: Record<string, unknown>): Promise<Response> {
    return queue.fetch('https://queue/changes', { method: 'POST', body: JSON.stringify(change) });
}

function change(seed: number, fields: Record<string, unknown> = {}): Record<string, unknown> {
    return { sha: sha(seed), base: main, owner: 'system_adamic_compiler', paths: ['internal/lower/a.go'], ...fields };
}

async function logOf(queue: DurableObjectStub<Queue>): Promise<QueueEvent[]> {
    return runInDurableObject(queue, function (instance: Queue) {
        return instance.log();
    });
}

describe('the queue', function () {
    it('takes a change, names it, and gives it the next position in submit order', async function () {
        const queue = await freshQueue();
        const first = await submit(queue, change(1));
        expect(first.status, await first.clone().text()).toBe(201);
        const firstBody = (await first.json()) as { change: string; state: string; position: number };
        expect(firstBody.change).toMatch(/^chg_[0-9a-z]{26}$/);
        expect(firstBody).toMatchObject({ state: 'queued', position: 0 });
        const second = (await (await submit(queue, change(2))).json()) as { change: string; position: number };
        expect(second.position).toBe(1);
        expect(second.change).not.toBe(firstBody.change);
        const read = await queue.fetch(`https://queue/changes/${firstBody.change}`);
        expect(read.status).toBe(200);
        expect(await read.json()).toMatchObject({
            record: { change: firstBody.change, sha: sha(1), base: main, owner: 'system_adamic_compiler', paths: ['internal/lower/a.go'], parent: null, fixesRed: null },
            state: 'queued',
            position: 0,
            landed: null,
        });
    });

    it('refuses a change git rules out, by reason, and logs the refusal', async function () {
        const queue = await freshQueue({
            [sha(10)]: facts({ shaExists: false }),
            [sha(11)]: facts({ baseIsAncestor: false }),
            [sha(12)]: facts({ baseOnMain: false }),
        });
        const cases: [Record<string, unknown>, string][] = [
            [change(10), 'is not on GitHub'],
            [change(11), 'is not an ancestor of sha'],
            [change(12), 'is not on main'],
            [change(13, { paths: ['internal/lower/a.go', 'cloud/elsewhere.sh'] }), 'paths outside the diff base..sha: cloud/elsewhere.sh'],
            [change(14, { parent: 'chg_' + 'z'.repeat(26) }), 'is not a change this queue holds'],
        ];
        for (const [request, reason] of cases) {
            const response = await submit(queue, request);
            expect(response.status).toBe(422);
            expect(((await response.json()) as { reason: string }).reason).toContain(reason);
        }
        const log = await logOf(queue);
        expect(
            log.map(function (event) {
                return event.type;
            }),
        ).toEqual(['change.refused', 'change.refused', 'change.refused', 'change.refused', 'change.refused']);
        expect(log[0]?.data).toMatchObject({ facts: { shaExists: false }, reason: `sha ${sha(10)} is not on GitHub` });
    });

    it('refuses a malformed change before asking git', async function () {
        let asked = 0;
        const queue = await freshQueue();
        await runInDurableObject(queue, function (instance: Queue) {
            instance.history = {
                async facts(): Promise<GitFacts> {
                    asked++;
                    return facts();
                },
            };
        });
        for (const bad of [{ ...change(1), sha: 'nope' }, { ...change(1), paths: [] }, { ...change(1), paths: ['../escape'] }, { ...change(1), extra: 1 }]) {
            expect((await submit(queue, bad)).status).toBe(422);
        }
        expect(asked).toBe(0);
        expect(await logOf(queue)).toEqual([]);
    });

    it('takes stacks up to five deep and refuses a sixth', async function () {
        const queue = await freshQueue();
        let parent: string | null = null;
        for (let depth = 1; depth <= MaximumStackDepth; depth++) {
            const response = await submit(queue, change(100 + depth, { parent: parent }));
            expect(response.status, await response.clone().text()).toBe(201);
            parent = ((await response.json()) as { change: string }).change;
        }
        const sixth = await submit(queue, change(106, { parent: parent }));
        expect(sixth.status).toBe(422);
        expect(((await sixth.json()) as { reason: string }).reason).toBe(`a stack is at most ${MaximumStackDepth} deep`);
    });

    it('chains every event to the one before it, from 64 zeros', async function () {
        const queue = await freshQueue({ [sha(3)]: facts({ baseOnMain: false }) });
        await submit(queue, change(1));
        await submit(queue, change(3));
        await submit(queue, change(2));
        const log = await logOf(queue);
        expect(
            log.map(function (event) {
                return event.seq;
            }),
        ).toEqual([1, 2, 3, 4, 5]);
        expect(log[0]?.prev).toBe(GenesisHash);
        for (let index = 1; index < log.length; index++) {
            expect(log[index]?.prev).toBe(await sha256Text(canonical(log[index - 1])));
        }
    });

    it('replays its log to the same state, and refuses a log that was changed', async function () {
        const queue = await freshQueue({ [sha(4)]: facts({ shaExists: false }) });
        const submitted: string[] = [];
        for (const seed of [1, 4, 2, 3]) {
            const response = await submit(queue, change(seed));
            if (response.status === 201) {
                submitted.push(((await response.json()) as { change: string }).change);
            }
        }
        const log = await logOf(queue);
        const replayed = await replay(log);
        expect(replayed.line).toEqual(submitted);
        expect(replayed.refused).toBe(1);
        for (const [index, id] of submitted.entries()) {
            const live = (await (await queue.fetch(`https://queue/changes/${id}`)).json()) as { position: number; record: unknown };
            expect(replayed.changes.get(id)?.position).toBe(index);
            expect(live.position).toBe(index);
            expect(replayed.changes.get(id)?.record).toEqual(live.record);
        }
        const tampered = log.map(function (event) {
            return structuredClone(event);
        });
        (tampered[1]?.data as { reason: string }).reason = 'edited';
        await expect(replay(tampered)).rejects.toThrow(/prev is .* not the hash of the event before it/);
        await expect(replay(log.slice(1))).rejects.toThrow(/the log has a gap/);
    });

    it('serves a change its own events, after a sequence number, and 404 for a change it never took', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        await submit(queue, change(2));
        const lines = (await (await queue.fetch(`https://queue/changes/${id}/events`)).text()).trim().split('\n');
        expect(lines).toHaveLength(2);
        expect(JSON.parse(lines[0] ?? '')).toMatchObject({ seq: 1, type: 'change.submitted', subject: { change: id } });
        // Slice 1: a change is its own future, its tree its sha.
        expect(JSON.parse(lines[1] ?? '')).toMatchObject({ seq: 2, type: 'future.built', subject: { change: id, future: sha(1) }, data: { base: main, changes: [id] } });
        expect((await (await queue.fetch(`https://queue/changes/${id}/events?after=2`)).text()).trim()).toBe('');
        expect((await queue.fetch(`https://queue/changes/chg_${'q'.repeat(26)}`)).status).toBe(404);
        // The owners' feed carries only landed, red and parked; a submit is none of them. The whole log carries all.
        expect((await (await queue.fetch('https://queue/events?after=0&owners=1')).text()).trim()).toBe('');
        expect((await (await queue.fetch('https://queue/events?after=1')).text()).trim().split('\n')).toHaveLength(3);
    });

    it('lands nothing itself: a change whose whole verdict passed is a landing order until the pusher reports exactly its tree', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        expect(await landings(queue)).toEqual([]);
        // A verdict for a tree the change isn't tested in decides nothing, whatever it says.
        expect((await postWhole(queue, id, sha(9), 'passed', null)).status).toBe(409);
        expect(await landings(queue)).toEqual([]);
        expect((await postWhole(queue, id, sha(1), 'passed', null)).status).toBe(200);
        expect(await landings(queue)).toEqual([{ change: id, future: sha(1), base: main, owner: 'system_adamic_compiler', run: 'gate-logs/x/fast' }]);
        // The same run again is an answer, not a second verdict.
        expect((await postWhole(queue, id, sha(1), 'passed', null)).status).toBe(200);
        // The pusher landed some other commit: refused, nothing logged.
        expect((await report(queue, id, { main: sha(50), from: main, landed: sha(2) })).status).toBe(409);
        const landed = await report(queue, id, { main: sha(50), from: main, landed: sha(1) });
        expect(landed.status, await landed.clone().text()).toBe(200);
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ state: 'landed', landed: sha(50), future: sha(1) });
        expect(await landings(queue)).toEqual([]);
        // A second report of the same landing is an answer, not a second event.
        expect((await report(queue, id, { main: sha(50), from: main, landed: sha(1) })).status).toBe(200);
        expect(
            (await logOf(queue)).map(function (event) {
                return event.type;
            }),
        ).toEqual(['change.submitted', 'future.built', 'verdict.decided', 'change.landed']);
        const feed = (await (await queue.fetch('https://queue/events?owners=1')).text()).trim().split('\n');
        expect(feed).toHaveLength(1);
        expect(JSON.parse(feed[0] ?? '')).toMatchObject({ type: 'change.landed', subject: { change: id }, data: { main: sha(50), from: main, landed: sha(1) } });
        expect((await replay(await logOf(queue))).changes.get(id)?.state).toBe('landed');
    });

    it('gives a failed or void verdict no landing order, sends only the change-caused red to the owner, and parks a refused push', async function () {
        const queue = await freshQueue();
        const ids: string[] = [];
        for (const seed of [1, 2, 3, 4]) {
            ids.push(((await (await submit(queue, change(seed))).json()) as { change: string }).change);
        }
        expect((await postWhole(queue, ids[0] ?? '', sha(1), 'failed', 'change')).status).toBe(200);
        expect((await postWhole(queue, ids[1] ?? '', sha(2), 'failed', 'mainRed')).status).toBe(200);
        expect((await postWhole(queue, ids[2] ?? '', sha(3), 'void', 'infra')).status).toBe(200);
        expect((await postWhole(queue, ids[3] ?? '', sha(4), 'passed', null)).status).toBe(200);
        expect(
            (await landings(queue)).map(function (order) {
                return order.change;
            }),
        ).toEqual([ids[3]]);
        expect((await report(queue, ids[1] ?? '', { main: sha(50), from: main, landed: sha(2) })).status).toBe(409);
        // A void lets the next run decide; a red is final.
        expect((await postWhole(queue, ids[2] ?? '', sha(3), 'passed', null, 'second')).status).toBe(200);
        expect((await postWhole(queue, ids[0] ?? '', sha(1), 'passed', null, 'second')).status).toBe(409);
        // main moved past change 4's base, so the fast-forward was refused: parked, its owner told.
        expect((await report(queue, ids[3] ?? '', { main: sha(77), refused: 'not a fast-forward' })).status).toBe(200);
        const feed = (await (await queue.fetch('https://queue/events?owners=1')).text())
            .trim()
            .split('\n')
            .map(function (line) {
                return JSON.parse(line) as QueueEvent;
            });
        expect(
            feed.map(function (event) {
                return [event.type, event.subject.change];
            }),
        ).toEqual([
            ['change.red', ids[0]],
            ['change.parked', ids[3]],
        ]);
        expect(await (await queue.fetch(`https://queue/changes/${ids[0]}`)).json()).toMatchObject({ state: 'red' });
        expect(
            (await landings(queue)).map(function (order) {
                return order.change;
            }),
        ).toEqual([ids[2]]);
    });

    it('lists unplanned futures for the planner and takes its plan only with keys computed from their parts', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        const listed = (await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: unknown[] };
        expect(listed.futures).toEqual([{ future: sha(1), tree: sha(1), base: main, changes: [id] }]);
        const units = await planOf(['a', 'b']);
        const forged = [{ ...units[0], unitKey: 'f'.repeat(64) }, units[1]];
        const refused = await postPlan(queue, sha(1), forged);
        expect(refused.status).toBe(422);
        expect(((await refused.json()) as { error: string }).error).toContain('is not its keyParts');
        // A reuse needs a passed verdict in the index for its exact key; the index is empty.
        expect((await postPlan(queue, sha(1), [{ ...units[0], decision: 'reuse' }, units[1]])).status).toBe(422);
        expect((await postPlan(queue, sha(1), units)).status).toBe(200);
        expect((await postPlan(queue, sha(1), units)).status).toBe(200);
        expect((await postPlan(queue, sha(1), [units[0]])).status).toBe(409);
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: unknown[] }).futures).toEqual([]);
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ state: 'testing', units: { planned: 2, passed: 0 } });
        // A planned future is the judge's: today's gate's whole verdict no longer decides it.
        expect((await postWhole(queue, id, sha(1), 'passed', null)).status).toBe(409);
        expect(
            (await logOf(queue)).filter(function (event) {
                return event.type === 'unit.planned';
            }),
        ).toHaveLength(2);
    });

    it("lands a planned future only on the judge's green, recomputed, and serves each unit's latest verdict from the index", async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        const units = await planOf(['a', 'b']);
        await postPlan(queue, sha(1), units);
        const keys = units.map(function (unit) {
            return unit.unitKey;
        });
        expect((await queue.fetch(`https://queue/verdicts/${keys[0]}`)).status).toBe(404);
        // A batch that claims green over a void unit is refused, and logs nothing.
        const lying = batch(id, sha(1), 'run-1', [record(id, keys[0] ?? '', 'run-1', 'passed', null), record(id, keys[1] ?? '', 'run-1', 'void', 'infra')], 'green');
        expect((await postBatch(queue, sha(1), lying)).status).toBe(422);
        // A batch green over only part of the plan is refused too: the plan is the planner's, not the judge's.
        expect((await postBatch(queue, sha(1), batch(id, sha(1), 'run-1', [record(id, keys[0] ?? '', 'run-1', 'passed', null)], 'green'))).status).toBe(422);
        const voided = batch(id, sha(1), 'run-1', [record(id, keys[0] ?? '', 'run-1', 'passed', null), record(id, keys[1] ?? '', 'run-1', 'void', 'infra')], 'void');
        expect((await postBatch(queue, sha(1), voided)).status).toBe(200);
        expect(await landings(queue)).toEqual([]);
        expect(await (await queue.fetch(`https://queue/verdicts/${keys[1]}`)).json()).toMatchObject({ unitKey: keys[1], status: 'void', run: 'run-1' });
        // The rerun is green, with one unit failed as main's red, which the judge excuses.
        const green = batch(id, sha(1), 'run-2', [record(id, keys[0] ?? '', 'run-2', 'passed', null), record(id, keys[1] ?? '', 'run-2', 'failed', 'mainRed')], 'green', [keys[1] ?? '']);
        const decided = await postBatch(queue, sha(1), green);
        expect(decided.status, await decided.clone().text()).toBe(200);
        expect((await postBatch(queue, sha(1), green)).status).toBe(200);
        expect((await postBatch(queue, sha(1), { ...green, run: 'run-3' })).status).toBe(422);
        expect(await (await queue.fetch(`https://queue/verdicts/${keys[1]}`)).json()).toMatchObject({ status: 'failed', cause: 'mainRed', run: 'run-2' });
        expect(await landings(queue)).toEqual([{ change: id, future: sha(1), base: main, owner: 'system_adamic_compiler', run: 'run-2' }]);
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ units: { planned: 2, passed: 1, failed: 1 } });
        // Replay rebuilds the index and the landing order from the log alone.
        const replayed = await replay(await logOf(queue));
        expect(replayed.verdicts.get(keys[1] ?? '')?.record).toMatchObject({ status: 'failed', run: 'run-2' });
        expect(replayed.futures.get(sha(1))?.decided).toEqual({ run: 'run-2', status: 'green' });
    });

    it("sends a red batch's kicks to the owner, and lets a later future reuse a passed key", async function () {
        const queue = await freshQueue();
        const first = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        const units = await planOf(['a', 'b']);
        const keys = units.map(function (unit) {
            return unit.unitKey;
        });
        await postPlan(queue, sha(1), units);
        const red = batch(first, sha(1), 'run-1', [record(first, keys[0] ?? '', 'run-1', 'passed', null), record(first, keys[1] ?? '', 'run-1', 'failed', 'change')], 'red', [], [keys[1] ?? '']);
        const kicks = { [keys[1] ?? '']: { test: 'TestB', repro: `loom repro ${keys[1]}` } };
        expect((await postBatch(queue, sha(1), { ...red, decision: { ...red.decision, kicks: kicks } })).status).toBe(200);
        const feed = (await (await queue.fetch('https://queue/events?owners=1')).text()).trim().split('\n');
        expect(JSON.parse(feed[0] ?? '')).toMatchObject({ type: 'change.red', subject: { change: first }, data: { kicks: kicks } });
        expect(await landings(queue)).toEqual([]);
        // The next change reuses unit a, which passed, and runs b again.
        const second = ((await (await submit(queue, change(2))).json()) as { change: string }).change;
        expect((await postPlan(queue, sha(2), [{ ...units[0], decision: 'reuse', reused: 'run-1' }, units[1]])).status).toBe(200);
        // It can't reuse b, whose latest verdict is red.
        const third = ((await (await submit(queue, change(3))).json()) as { change: string }).change;
        expect((await postPlan(queue, sha(3), [units[0], { ...units[1], decision: 'reuse' }])).status).toBe(422);
        const green = batch(second, sha(2), 'run-2', [record(second, keys[0] ?? '', 'run-2', 'passed', null), record(second, keys[1] ?? '', 'run-2', 'passed', null)], 'green');
        expect((await postBatch(queue, sha(2), green)).status).toBe(200);
        expect(
            (await landings(queue)).map(function (order) {
                return order.change;
            }),
        ).toEqual([second]);
        expect(third).toMatch(/^chg_/);
    });
});

describe("today's gate on a newer main", function () {
    it('makes the gate merge the change\'s future, the tree that lands, only while its own future is undecided', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        const merged = sha(60);
        const post = function (gateMerge: unknown, status = 'passed', run = 'gate-logs/m/fast'): Promise<Response> {
            return queue.fetch('https://queue/verdicts', {
                method: 'POST',
                body: JSON.stringify({ change: id, verdict: { future: merged, run: run, status: status, cause: null, rule: 'todays-gate-v0' }, gateMerge: gateMerge }),
            });
        };
        // Without saying it's a gate merge of this change, another tree decides nothing.
        expect((await post(undefined)).status).toBe(409);
        const decided = await post({ base: sha(61) });
        expect(decided.status, await decided.clone().text()).toBe(200);
        expect(await landings(queue)).toEqual([{ change: id, future: merged, base: sha(61), owner: 'system_adamic_compiler', run: 'gate-logs/m/fast' }]);
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: unknown[] }).futures).toEqual([]);
        // The replaced future decides nothing now.
        expect((await postWhole(queue, id, sha(1), 'passed', null)).status).toBe(409);
        expect((await report(queue, id, { main: sha(70), from: sha(61), landed: sha(1) })).status).toBe(409);
        expect((await report(queue, id, { main: sha(70), from: sha(61), landed: merged })).status).toBe(200);
        // A change whose own future already passed can't be moved to another tree.
        const second = ((await (await submit(queue, change(2))).json()) as { change: string }).change;
        await postWhole(queue, second, sha(2), 'passed', null);
        const moved = await queue.fetch('https://queue/verdicts', {
            method: 'POST',
            body: JSON.stringify({ change: second, verdict: { future: sha(62), run: 'r2', status: 'passed', cause: null, rule: 'todays-gate-v0' }, gateMerge: { base: sha(61) } }),
        });
        expect(moved.status).toBe(409);
    });
});

describe('a queue with no GitHub credential', function () {
    it('takes a change unchecked, and only git facts from the bridge clear it into a future or refuse it', async function () {
        const queue = await freshQueue();
        await runInDurableObject(queue, function (instance: Queue) {
            instance.history = null;
        });
        const first = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        const second = ((await (await submit(queue, change(2))).json()) as { change: string }).change;
        // The line's own checks need no git: the same sha twice is refused at once.
        expect((await submit(queue, change(1))).status).toBe(422);
        const unchecked = (await (await queue.fetch('https://queue/submissions?state=unchecked')).json()) as { changes: { change: string; sha: string }[] };
        expect(unchecked.changes).toEqual([
            { change: first, sha: sha(1), base: main, paths: ['internal/lower/a.go'] },
            { change: second, sha: sha(2), base: main, paths: ['internal/lower/a.go'] },
        ]);
        // Nothing is planned or decided before git clears it.
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: unknown[] }).futures).toEqual([]);
        expect((await postWhole(queue, first, sha(1), 'passed', null)).status).toBe(409);
        const post = function (id: string, body: unknown): Promise<Response> {
            return queue.fetch(`https://queue/submissions/${id}/facts`, { method: 'POST', body: JSON.stringify(body) });
        };
        expect((await post(first, { shaExists: true })).status).toBe(400);
        expect(await (await post(first, facts())).json()).toMatchObject({ change: first, state: 'queued', future: sha(1) });
        expect((await post(first, facts())).status).toBe(409);
        expect(await (await post(second, facts({ baseOnMain: false }))).json()).toMatchObject({ change: second, state: 'refused' });
        expect(await (await queue.fetch(`https://queue/changes/${second}`)).json()).toMatchObject({ state: 'refused', verdict: { reason: `base ${main} is not on main` } });
        expect(((await (await queue.fetch('https://queue/submissions?state=unchecked')).json()) as { changes: unknown[] }).changes).toEqual([]);
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: { future: string }[] }).futures.map(function (future) {
            return future.future;
        })).toEqual([sha(1)]);
        expect((await postWhole(queue, first, sha(1), 'passed', null)).status).toBe(200);
        const replayed = await replay(await logOf(queue));
        expect(replayed.changes.get(second)?.state).toBe('refused');
        expect(replayed.changes.get(first)?.checked).toBe(true);
        expect(replayed.line).toEqual([first]);
    });
});

describe("the queue's board pushes", function () {
    it('push each moved change as its summary from an alarm, and nothing twice once pushed', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(41))).json()) as { change: string }).change;
        await postWhole(queue, id, sha(41), 'passed', null);
        const board = (env as unknown as { ChangeBoard: DurableObjectNamespace }).ChangeBoard;
        const lineOf = async function (): Promise<Record<string, unknown> | undefined> {
            const changes = ((await (await board.get(board.idFromName('board')).fetch('https://board/changes')).json()) as { changes: Record<string, unknown>[] }).changes;
            return changes.find(function (line) {
                return line.change === id;
            });
        };
        expect(await lineOf()).toBeUndefined();
        expect(await runDurableObjectAlarm(queue)).toBe(true);
        expect(await lineOf()).toMatchObject({ change: id, owner: 'system_adamic_compiler', sha: sha(41), state: 'testing', future: sha(41), units: { planned: 0, passed: 0, failed: 0, void: 0 } });
        await report(queue, id, { main: sha(50), from: main, landed: sha(41) });
        expect(await runDurableObjectAlarm(queue)).toBe(true);
        expect(await lineOf()).toMatchObject({ change: id, state: 'landed' });
        const pushed = await runInDurableObject(queue, function (_instance: Queue, context: DurableObjectState) {
            return context.storage.sql.exec<{ value: string }>("SELECT value FROM facts WHERE name = 'boardSeq'").toArray()[0]?.value;
        });
        expect(pushed).toBe(String((await logOf(queue)).length));
    });
});

describe('a landing order', function () {
    it("needs the future's own verdicts to say green, not only a decision that did", function () {
        const verdictOf = function (unitKey: string, status: UnitVerdict['status'], cause: UnitVerdict['cause'], future = sha(1)): UnitVerdict {
            return { unitKey: unitKey, change: 'chg_' + 'a'.repeat(26), future: future, run: 'r', status: status, cause: cause };
        };
        const futureWith = function (verdicts: (UnitVerdict | null)[]): FutureEntry {
            const units = new Map(
                verdicts.map(function (verdict, index) {
                    const key = String(index).repeat(64);
                    return [key, { unitKey: key, name: `u${index}`, decision: 'run' as const, verdict: verdict === null ? null : { ...verdict, unitKey: key } }];
                }),
            );
            return { tree: sha(1), base: main, changes: [], units: units, whole: null, decided: { run: 'r', status: 'green' } };
        };
        expect(futureLandable(futureWith([verdictOf('', 'passed', null), verdictOf('', 'failed', 'mainRed')]))).toBe(true);
        expect(futureLandable(futureWith([verdictOf('', 'passed', null), verdictOf('', 'void', 'infra')]))).toBe(false);
        expect(futureLandable(futureWith([verdictOf('', 'passed', null), null]))).toBe(false);
        expect(futureLandable(futureWith([verdictOf('', 'passed', null), verdictOf('', 'passed', null, sha(2))]))).toBe(false);
        expect(futureLandable(futureWith([]))).toBe(false);
        expect(futureLandable({ ...futureWith([verdictOf('', 'passed', null)]), decided: { run: 'r', status: 'void' } })).toBe(false);
    });
});

describe("loom-pipeline's queue seams", function () {
    it('open to a coordinator token only, and reach the Queue object past it', async function () {
        const coordinator = await token('system_adamic_loom', 'coordinator');
        const routes: [string, string][] = [
            ['GET', '/landings'],
            ['GET', `/verdicts/${'a'.repeat(64)}`],
            ['GET', '/futures?state=unplanned'],
            ['POST', `/futures/${sha(1)}/plan`],
            ['POST', `/futures/${sha(1)}/verdicts`],
            ['POST', '/verdicts'],
            ['POST', `/landings/chg_${'q'.repeat(26)}`],
            ['GET', '/submissions?state=unchecked'],
            ['POST', `/submissions/chg_${'q'.repeat(26)}/facts`],
        ];
        for (const [method, path] of routes) {
            const body = method === 'POST' ? '{}' : undefined;
            expect((await call(path, { method: method, body: body })).status, path).toBe(401);
            for (const scope of ['submit', 'board', 'runner', 'pool', 'build'] as const) {
                expect((await call(path, { method: method, body: body, bearer: await token('system_adamic_loom', scope) })).status, `${scope} ${path}`).toBe(403);
            }
        }
        expect(await (await call('/landings', { bearer: coordinator })).json()).toEqual({ landings: [] });
        expect((await call(`/verdicts/${'a'.repeat(64)}`, { bearer: coordinator })).status).toBe(404);
        expect(await (await call('/futures?state=unplanned', { bearer: coordinator })).json()).toEqual({ futures: [] });
        const plan = await call(`/futures/${sha(1)}/plan`, { method: 'POST', bearer: coordinator, body: JSON.stringify(await planOf(['a'])) });
        expect(plan.status).toBe(404);
        expect(await plan.json()).toEqual({ error: `no future ${sha(1)}` });
    });
});

function landings(queue: DurableObjectStub<Queue>): Promise<{ change: string; future: string; base: string; owner: string }[]> {
    return queue
        .fetch('https://queue/landings')
        .then(function (response) {
            return response.json();
        })
        .then(function (body) {
            return (body as { landings: { change: string; future: string; base: string; owner: string }[] }).landings;
        });
}

function report(queue: DurableObjectStub<Queue>, id: string, body: Record<string, unknown>): Promise<Response> {
    return queue.fetch(`https://queue/landings/${id}`, { method: 'POST', body: JSON.stringify(body) });
}

function postWhole(queue: DurableObjectStub<Queue>, id: string, future: string, status: string, cause: string | null, run = 'gate-logs/x/fast'): Promise<Response> {
    return queue.fetch('https://queue/verdicts', {
        method: 'POST',
        body: JSON.stringify({ change: id, verdict: { future: future, run: run, status: status, cause: cause, rule: 'todays-gate-v0' } }),
    });
}

interface TestUnit {
    name: string;
    unitKey: string;
    keyParts: Record<string, unknown>;
    decision: string;
    reason: string;
    reused?: string;
}

// One test unit per named package, keyed from its parts the way the planner keys them.
async function planOf(packages: string[]): Promise<TestUnit[]> {
    const units: TestUnit[] = [];
    for (const name of packages) {
        const keyParts = {
            kind: 'test',
            package: `github.com/system-inc/adamic/internal/${name}`,
            select: { run: '', skip: '' },
            closure: 'c'.repeat(64),
            reads: 'd'.repeat(64),
            products: [],
            tools: { runner: 'e'.repeat(64), go: 'go1.27.2', clang: '', node: '', wasiSdk: '' },
            env: { ADAMIC_GATE_UNCACHED: '1' },
            gateInputs: '',
        };
        units.push({ name: keyParts.package, unitKey: await unitKeyOf(keyParts), keyParts: keyParts, decision: 'run', reason: 'new key' });
    }
    return units;
}

function postPlan(queue: DurableObjectStub<Queue>, tree: string, units: unknown[]): Promise<Response> {
    return queue.fetch(`https://queue/futures/${tree}/plan`, { method: 'POST', body: JSON.stringify(units) });
}

function record(id: string, unitKey: string, run: string, status: string, cause: string | null): Record<string, unknown> {
    return {
        unitKey: unitKey,
        change: id,
        future: '',
        run: run,
        status: status,
        cause: cause,
        infra: status === 'void' ? 'kill' : null,
        attempts: [],
        tests: [],
        outputs: [],
        rule: 'judge-v1',
        decidedAt: '2026-10-09T23:40:00Z',
    };
}

function batch(id: string, tree: string, run: string, records: Record<string, unknown>[], status: string, excused: string[] = [], red: string[] = []): { change: string; run: string; rule: string; plan: string[]; verdicts: Record<string, unknown>[]; decision: Record<string, unknown>; quarantine: unknown[] } {
    return {
        change: id,
        run: run,
        rule: 'judge-v1',
        plan: records.map(function (item) {
            return item.unitKey as string;
        }),
        verdicts: records.map(function (item) {
            return { ...item, future: tree };
        }),
        decision: { status: status, red: red, excused: excused, problems: [], kicks: {} },
        quarantine: [],
    };
}

function postBatch(queue: DurableObjectStub<Queue>, tree: string, body: Record<string, unknown>): Promise<Response> {
    return queue.fetch(`https://queue/futures/${tree}/verdicts`, { method: 'POST', body: JSON.stringify(body) });
}
