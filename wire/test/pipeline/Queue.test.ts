import { env } from 'cloudflare:workers';
import { runDurableObjectAlarm, runInDurableObject } from 'cloudflare:test';
import { describe, expect, it } from 'vitest';
import { call, token } from '../Helpers';
import { oldQueueLog } from './OldQueueLog';
import { canonical, futureLandable, GenesisHash, lineRefusalOf, MaximumStackDepth, replay, sha256Text, unitKeyOf, type FutureEntry, type GitFacts, type Queue, type QueueEvent, type UnitVerdict } from '../../source/Queue';

const main = 'a'.repeat(40);

function sha(seed: number): string {
    return seed.toString(16).padStart(40, '0');
}

// A stand-in for GitHub: every sha exists, descends from main, and touches the paths it's given, unless told otherwise.
function facts(overrides: Partial<GitFacts> = {}, diffPaths = ['internal/lower/a.go']): GitFacts {
    return { shaExists: true, baseIsAncestor: true, baseOnMain: true, diffPaths: diffPaths, historyPaths: diffPaths, gateNamed: [], pins: [], ...overrides };
}

// adamic's cohere pin, fetchable keyless from its public remote.
const cohere = { path: 'cohere', url: 'https://github.com/system-inc/cohere.git', sha: sha(0), fetchable: true };

// git's facts for a witness of a main commit, read while main's head was `head` (the commit itself, unless told).
function headOf(seed: number, head = seed): GitFacts {
    return facts({ mainHead: sha(head) }, []);
}

// A fresh queue object per test, with git answered from a table keyed by sha and a pinned clock.
async function freshQueue(answers: Record<string, GitFacts> = {}): Promise<DurableObjectStub<Queue>> {
    // The pipeline's binding (pipeline.jsonc); the generated Env type knows only loom-runs's.
    const namespace = (env as unknown as { Queue: DurableObjectNamespace<Queue> }).Queue;
    const stub = namespace.get(namespace.idFromName('queue-' + crypto.randomUUID()));
    await runInDurableObject(stub, function (instance: Queue) {
        // main's head as git last read it: an answer that names one moves it, and every other reading says the same
        // head, as git would between pushes. An answer whose mainHead is undefined is a reading that couldn't say.
        let head = main;
        instance.history = {
            // A base other than the main these tests start from is one main has moved past: nothing descends from it here.
            async facts(asked: string, base: string): Promise<GitFacts> {
                const answer = answers[asked] ?? (base === main ? facts() : facts({ baseIsAncestor: false }));
                if (typeof answer.mainHead === 'string') {
                    head = answer.mainHead;
                }
                return 'mainHead' in answer ? answer : { ...answer, mainHead: head };
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
            [sha(15)]: facts({}, ['internal/lower/a.go', 'internal/lower/b.go']),
            [sha(16)]: facts({}, ['stage3/fixtures/a.ts', 'stage3/fixtures/a_test.go']),
            [sha(17)]: facts({}, ['stage3/meter/m.py', 'stage3/meter/mutants/m-mutant.txt']),
            [sha(18)]: facts({ historyPaths: ['cloud/elsewhere.sh', 'internal/lower/a.go'] }),
            [sha(19)]: facts({ gateNamed: [{ path: 'cloud/a_test.py', users: ['cloud/run.sh'] }] }, ['cloud/a_test.py']),
            [sha(20)]: facts({ historyPaths: undefined }),
            [sha(21)]: facts({ baseOnMain: false }, []),
            [sha(22)]: facts({ pins: [cohere, { path: 'cohere/TypeScript', url: 'https://github.com/system-inc/TypeScript.git', sha: sha(98), fetchable: false, reason: 'a keyless fetch refused it: not our ref' }] }),
            [sha(23)]: facts({ pins: [{ ...cohere, url: 'git@github.com:system-inc/cohere.git', fetchable: false, reason: "its url is ssh, which needs a key a runner doesn't hold" }] }),
            [sha(24)]: facts({ pins: undefined }),
            [sha(25)]: facts({ pins: [cohere, { path: 'cohere/TypeScript', url: 'https://github.com/system-inc/TypeScript.git', sha: sha(98), fetchable: true }] }),
        });
        const cases: [Record<string, unknown>, string][] = [
            [change(10), 'is not on GitHub'],
            [change(11), 'is not an ancestor of sha'],
            [change(12), 'is not on main'],
            [change(13, { paths: ['internal/lower/a.go', 'cloud/elsewhere.sh'] }), 'paths outside the diff base..sha: cloud/elsewhere.sh'],
            [change(14, { parent: 'chg_' + 'z'.repeat(26) }), 'is not a change this queue holds'],
            // Every path of the diff is declared, so no path rule can be dodged by leaving one out.
            [change(15), 'the paths leave out 1 of the diff base..sha: internal/lower/b.go'],
            // A harness change comes with its mutant evidence; a test file or testdata there isn't harness.
            [change(16, { paths: ['stage3/fixtures/a.ts', 'stage3/fixtures/a_test.go'] }), 'it changes test harness (stage3/fixtures/a.ts) with no mutant evidence'],
            // A commit beyond base touching a path the final diff drops would be recorded as merged (#f8973gv).
            [change(18), "its commits beyond base touch paths its diff doesn't: cloud/elsewhere.sh"],
            // A Python test a non-test file names is gate logic (#xz7j9ea).
            [change(19, { paths: ['cloud/a_test.py'] }), "cloud/a_test.py is named by cloud/run.sh, which isn't a test, so it's gate logic"],
            // Facts with no history are no clearance: refused, never assumed clean.
            [change(20), "git's facts carry no history or gate-logic check"],
            // Only a witness of main can hold main red: a witness of a sha main doesn't hold is refused at the door.
            [{ sha: sha(21), base: sha(21), owner: 'system_adamic_loom_judge', paths: [], parity: true, witness: true }, 'is not on main'],
            // A pin a runner can't fetch keyless is named, each one, before any future is built (#yt2jw5q).
            [change(22), `1 submodule pin a runner can't fetch without a key: cohere/TypeScript at ${sha(98).slice(0, 12)} from https://github.com/system-inc/TypeScript.git (a keyless fetch refused it: not our ref)`],
            [change(23), "cohere at 000000000000 from git@github.com:system-inc/cohere.git (its url is ssh, which needs a key a runner doesn't hold)"],
            // Facts with no pins are no clearance either.
            [change(24), "git's facts carry no submodule pins"],
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
        ).toEqual(Array(14).fill('change.refused'));
        expect(log[0]?.data).toMatchObject({ facts: { shaExists: false }, reason: `sha ${sha(10)} is not on GitHub` });
        expect((await submit(queue, change(17, { paths: ['stage3/meter/m.py', 'stage3/meter/mutants/m-mutant.txt'] }))).status).toBe(201);
        // Every pin fetchable, at every depth: the change joins the line.
        expect((await submit(queue, change(25))).status).toBe(201);
        const refusal = log.find(function (event) {
            return event.data.reason !== undefined && String(event.data.reason).includes('submodule pin');
        });
        expect(refusal?.data).toMatchObject({ facts: { pins: [{ path: 'cohere', fetchable: true }, { path: 'cohere/TypeScript', fetchable: false }] } });
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

    it('takes a landing reported again after its report was lost, from the future itself, and only that one from equal to main', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        expect((await postWhole(queue, id, sha(1), 'passed', null)).status).toBe(200);
        // from equal to main is no landing unless main is the future itself: the pusher's report of a branch already there.
        expect((await report(queue, id, { main: sha(50), from: sha(50), landed: sha(1) })).status).toBe(400);
        expect(await landings(queue)).toHaveLength(1);
        const again = await report(queue, id, { main: sha(1), from: sha(1), landed: sha(1) });
        expect(again.status, await again.clone().text()).toBe(200);
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ state: 'landed', landed: sha(1), future: sha(1) });
        expect(await landings(queue)).toEqual([]);
        // And once more is an answer, not a second event.
        expect((await report(queue, id, { main: sha(1), from: sha(1), landed: sha(1) })).status).toBe(200);
        expect(
            (await logOf(queue)).map(function (event) {
                return event.type;
            }),
        ).toEqual(['change.submitted', 'future.built', 'verdict.decided', 'change.landed']);
    });

    it('gives a failed or void verdict no landing order, sends only the change-caused red to the owner, and parks a refused push', async function () {
        const queue = await freshQueue();
        const ids: string[] = [];
        for (const seed of [1, 2, 3, 4]) {
            ids.push(((await (await submit(queue, change(seed))).json()) as { change: string }).change);
        }
        expect((await postWhole(queue, ids[0] ?? '', sha(1), 'failed', 'change')).status).toBe(200);
        expect((await postWhole(queue, ids[1] ?? '', sha(2), 'failed', 'flake')).status).toBe(200);
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
        expect((await report(queue, ids[3] ?? '', { main: sha(77), refused: 'again' })).status).toBe(409);
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
        // A push the pusher misread: git shows change 4's tree on main after all, so the landing is recorded.
        expect((await report(queue, ids[3] ?? '', { main: sha(78), from: sha(77), landed: sha(5) })).status).toBe(409);
        expect((await report(queue, ids[3] ?? '', { main: sha(78), from: sha(77), landed: sha(4) })).status).toBe(200);
        expect(await (await queue.fetch(`https://queue/changes/${ids[3]}`)).json()).toMatchObject({ state: 'landed', landed: sha(78) });
        // A red change is never landed by a report.
        expect((await report(queue, ids[0] ?? '', { main: sha(79), from: sha(78), landed: sha(1) })).status).toBe(409);
    });

    it('lists unplanned futures for the planner and takes its plan only with keys computed from their parts', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        const listed = (await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: unknown[] };
        expect(listed.futures).toEqual([{ future: sha(1), tree: sha(1), base: main, changes: [id], parity: false }]);
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
        // The judge's listing: the planned future with its change and exactly the planned units.
        expect(await (await queue.fetch('https://queue/futures?state=planned')).json()).toEqual({
            futures: [
                {
                    future: sha(1),
                    base: main,
                    parity: false,
                    attempt: 1,
                    change: { change: id, sha: sha(1), base: main, owner: 'system_adamic_compiler' },
                    units: units.map(function (unit) {
                        return { unitKey: unit.unitKey, name: unit.name, keyParts: unit.keyParts, decision: 'run', reused: null };
                    }),
                },
            ],
        });
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
        // A void run leaves the future for the judge's next attempt.
        expect(((await (await queue.fetch('https://queue/futures?state=planned')).json()) as { futures: { attempt: number }[] }).futures.map(function (future) {
            return future.attempt;
        })).toEqual([2]);
        // The rerun is green, with one unit failed as main's red, which the judge excuses.
        const green = batch(id, sha(1), 'run-2', [record(id, keys[0] ?? '', 'run-2', 'passed', null), record(id, keys[1] ?? '', 'run-2', 'failed', 'mainRed')], 'green', [keys[1] ?? '']);
        const decided = await postBatch(queue, sha(1), green);
        expect(decided.status, await decided.clone().text()).toBe(200);
        expect((await postBatch(queue, sha(1), green)).status).toBe(200);
        expect((await postBatch(queue, sha(1), { ...green, run: 'run-3' })).status).toBe(422);
        expect(await (await queue.fetch(`https://queue/verdicts/${keys[1]}`)).json()).toMatchObject({ status: 'failed', cause: 'mainRed', run: 'run-2' });
        expect(await landings(queue)).toEqual([{ change: id, future: sha(1), base: main, owner: 'system_adamic_compiler', run: 'run-2' }]);
        expect(((await (await queue.fetch('https://queue/futures?state=planned')).json()) as { futures: unknown[] }).futures).toEqual([]);
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
        // Pins that aren't {path, url, sha, fetchable} are no facts.
        for (const pins of [{}, [{ path: 'cohere', url: '', sha: 'nope', fetchable: true }], [{ path: 'cohere', url: '', sha: sha(98) }], [{ ...cohere, reason: 7 }]]) {
            expect((await post(first, facts({ pins: pins as never }))).status).toBe(400);
        }
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
        // The alarm is due at once, so the runtime may run it before the test does; either way the line arrives.
        await runDurableObjectAlarm(queue);
        expect(await lineOf()).toMatchObject({ change: id, owner: 'system_adamic_compiler', sha: sha(41), state: 'testing', future: sha(41), units: { planned: 0, passed: 0, failed: 0, void: 0 } });
        await report(queue, id, { main: sha(50), from: main, landed: sha(41) });
        await runDurableObjectAlarm(queue);
        expect(await lineOf()).toMatchObject({ change: id, state: 'landed' });
        const pushed = await runInDurableObject(queue, function (_instance: Queue, context: DurableObjectState) {
            return context.storage.sql.exec<{ value: string }>("SELECT value FROM facts WHERE name = 'boardSeq'").toArray()[0]?.value;
        });
        expect(pushed).toBe(String((await logOf(queue)).length));
        // A push reads no events: the moved changes come from memory (Web's cost cut).
        const reads: string[] = [];
        await runInDurableObject(queue, function (instance: Queue) {
            const holder = instance as unknown as { sql: SqlStorage };
            const exec = holder.sql.exec.bind(holder.sql);
            holder.sql = new Proxy(holder.sql, {
                get(target, name) {
                    return name === 'exec'
                        ? function (query: string, ...bindings: unknown[]) {
                              if (/FROM events/.test(query)) {
                                  reads.push(query);
                              }
                              return exec(query, ...(bindings as []));
                          }
                        : Reflect.get(target, name);
                },
            });
        });
        await postWhole(queue, id, sha(41), 'passed', null, 'second');
        const another = ((await (await submit(queue, change(42))).json()) as { change: string }).change;
        reads.length = 0;
        await runDurableObjectAlarm(queue);
        expect(reads).toEqual([]);
        expect(
            ((await (await board.get(board.idFromName('board')).fetch('https://board/changes')).json()) as { changes: { change: string }[] }).changes.some(function (line) {
                return line.change === another;
            }),
        ).toBe(true);
    });
});

describe('a stack', function () {
    it('lands a dependent only after its base, and parks every dependent when its base goes red', async function () {
        const queue = await freshQueue();
        const idOf = async function (response: Promise<Response>): Promise<string> {
            return ((await (await response).json()) as { change: string }).change;
        };
        const a = await idOf(submit(queue, change(1)));
        const b = await idOf(submit(queue, change(2, { parent: a })));
        const c = await idOf(submit(queue, change(3, { parent: b })));
        const other = await idOf(submit(queue, change(4)));
        // B passes first: it waits for A, whose commits it carries.
        await postWhole(queue, b, sha(2), 'passed', null);
        expect(await landings(queue)).toEqual([]);
        await postWhole(queue, a, sha(1), 'passed', null);
        expect(
            (await landings(queue)).map(function (order) {
                return order.change;
            }),
        ).toEqual([a]);
        await report(queue, a, { main: sha(50), from: main, landed: sha(1) });
        expect(
            (await landings(queue)).map(function (order) {
                return order.change;
            }),
        ).toEqual([b]);
        // A second stack: its base goes red, and both dependents, however deep, are parked with the reason.
        const d = await idOf(submit(queue, change(5)));
        const e = await idOf(submit(queue, change(6, { parent: d })));
        const f = await idOf(submit(queue, change(7, { parent: e })));
        await postWhole(queue, d, sha(5), 'failed', 'change');
        const feed = (await (await queue.fetch('https://queue/events?owners=1')).text())
            .trim()
            .split('\n')
            .map(function (line) {
                return JSON.parse(line) as QueueEvent;
            })
            .filter(function (event) {
                return event.type !== 'change.landed';
            });
        expect(
            feed.map(function (event) {
                return [event.type, event.subject.change, event.data.reason ?? null];
            }),
        ).toEqual([
            ['change.red', d, null],
            ['change.parked', e, `its base ${d} is red`],
            ['change.parked', f, `its base ${d} is red`],
        ]);
        expect(await (await queue.fetch(`https://queue/changes/${c}`)).json()).toMatchObject({ state: 'queued' });
        expect(await (await queue.fetch(`https://queue/changes/${other}`)).json()).toMatchObject({ state: 'queued' });
    });
});

describe("today's gate and main's own red", function () {
    it("lands a change whose only failure is main's red, as the judge's Green excuses it, and nothing else that failed", async function () {
        const queue = await freshQueue();
        const excused = ((await (await submit(queue, change(11))).json()) as { change: string }).change;
        const flaky = ((await (await submit(queue, change(12))).json()) as { change: string }).change;
        await postWhole(queue, excused, sha(11), 'failed', 'mainRed');
        await postWhole(queue, flaky, sha(12), 'failed', 'flake');
        expect(
            (await landings(queue)).map(function (order) {
                return order.change;
            }),
        ).toEqual([excused]);
        expect(((await (await queue.fetch('https://queue/events?owners=1')).text()).trim())).toBe('');
    });
});

describe('blocks, behind their switch', function () {
    it('are off until a rule.changed turns them on; then a cleared change waits and one block takes what waits, one in flight', async function () {
        const queue = await freshQueue();
        const idOf = async function (fields: Record<string, unknown>, seed: number): Promise<string> {
            return ((await (await submit(queue, change(seed, fields))).json()) as { change: string }).change;
        };
        // Off: a change is its own future, as tonight.
        const before = await idOf({}, 51);
        expect(await (await queue.fetch(`https://queue/changes/${before}`)).json()).toMatchObject({ future: sha(51) });
        const rules = function (body: unknown): Promise<Response> {
            return queue.fetch('https://queue/rules', { method: 'POST', body: JSON.stringify(body) });
        };
        expect((await rules({ rule: 'blocks', value: { on: true, budget: 2 } })).status).toBe(400);
        // Not until a block can resolve.
        expect((await rules({ rule: 'blocks', value: { on: true, budget: 2 }, commit: sha(99) })).status).toBe(422);
        await runInDurableObject(queue, function (instance: Queue) {
            instance.blocksReady = true;
        });
        expect((await rules({ rule: 'blocks', value: { on: true, budget: 2 }, commit: sha(99) })).status).toBe(200);
        const first = await idOf({}, 52);
        expect(await (await queue.fetch(`https://queue/changes/${first}`)).json()).toMatchObject({ future: null });
        const second = await idOf({}, 53);
        const third = await idOf({}, 54);
        // A parity run never joins a block.
        const parity = await idOf({ parity: true }, 55);
        expect(await (await queue.fetch(`https://queue/changes/${parity}`)).json()).toMatchObject({ future: sha(55) });
        // One block in flight: the first change opened it the instant it cleared; the rest wait for its slot.
        const unbuilt = (await (await queue.fetch('https://queue/blocks?state=unbuilt')).json()) as { blocks: { block: number; changes: { change: string }[] }[] };
        expect(unbuilt.blocks.map((block) => [block.block, block.changes.map((item) => item.change)])).toEqual([[1, [first]]]);
        const log = await logOf(queue);
        expect(log.filter((event) => event.type === 'block.opened')).toHaveLength(1);
        expect(log.filter((event) => event.type === 'change.checked').map((event) => event.subject.change)).toEqual([first, second, third]);
        expect(log.find((event) => event.type === 'rule.changed')).toMatchObject({ data: { rule: 'blocks', value: { on: true, budget: 2 }, commit: sha(99) } });
        const replayed = await replay(log);
        expect(replayed.rules.blocks).toEqual({ on: true, budget: 2 });
        expect(replayed.changes.get(second)?.block).toBe(null);
        expect(replayed.changes.get(first)?.block).toBe(1);
        expect(await landings(queue)).toEqual([]);
        // The builder's chain: a prefix that adds any other change is refused; the right one becomes the change's future.
        const built = function (body: unknown): Promise<Response> {
            return queue.fetch('https://queue/blocks/1/built', { method: 'POST', body: JSON.stringify(body) });
        };
        expect((await built({ base: main, prefixes: [{ tree: sha(70), change: second }], conflicts: [] })).status).toBe(422);
        expect((await built({ base: main, prefixes: [], conflicts: [] })).status).toBe(422);
        expect((await built({ base: main, prefixes: [{ tree: sha(70), change: first }], conflicts: [] })).status).toBe(200);
        expect((await built({ base: main, prefixes: [{ tree: sha(70), change: first }], conflicts: [] })).status).toBe(409);
        expect(await (await queue.fetch(`https://queue/changes/${first}`)).json()).toMatchObject({ future: sha(70) });
        expect(((await (await queue.fetch('https://queue/blocks?state=unbuilt')).json()) as { blocks: unknown[] }).blocks).toEqual([]);
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: { future: string; changes: string[] }[] }).futures).toContainEqual(
            expect.objectContaining({ future: sha(70), base: main, changes: [first] }),
        );
    });

    it("parks a change that conflicts with the ones ahead of it in its block, with the paths", async function () {
        const queue = await freshQueue();
        await runInDurableObject(queue, function (instance: Queue) {
            instance.blocksReady = true;
        });
        await queue.fetch('https://queue/rules', { method: 'POST', body: JSON.stringify({ rule: 'blocks', value: { on: true, budget: 4 }, commit: sha(99) }) });
        const id = ((await (await submit(queue, change(61))).json()) as { change: string }).change;
        const response = await queue.fetch('https://queue/blocks/1/built', { method: 'POST', body: JSON.stringify({ base: main, prefixes: [], conflicts: [{ change: id, paths: ['x.go'] }] }) });
        expect(response.status, await response.clone().text()).toBe(200);
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ state: 'parked', future: null });
        const feed = (await (await queue.fetch('https://queue/events?owners=1')).text()).trim();
        expect(feed).toContain('conflicts with the changes ahead of it in block 1: x.go');
    });
});

describe('a decided block', function () {
    it('lands its longest green prefix as one order, sends the changes behind a red back, and opens the next block at once', async function () {
        const queue = await freshQueue();
        await runInDurableObject(queue, function (instance: Queue) {
            instance.blocksReady = true;
        });
        await queue.fetch('https://queue/rules', { method: 'POST', body: JSON.stringify({ rule: 'blocks', value: { on: true, budget: 8 }, commit: sha(99) }) });
        const idOf = async function (seed: number): Promise<string> {
            return ((await (await submit(queue, change(seed))).json()) as { change: string }).change;
        };
        const built = function (block: number, prefixes: unknown[]): Promise<Response> {
            return queue.fetch(`https://queue/blocks/${block}/built`, { method: 'POST', body: JSON.stringify({ base: main, prefixes: prefixes, conflicts: [] }) });
        };
        const decide = function (id: string, tree: string, status: string, cause: string | null): Promise<Response> {
            return queue.fetch('https://queue/verdicts', {
                method: 'POST',
                body: JSON.stringify({ change: id, verdict: { future: tree, run: 'r-' + tree.slice(-4), status: status, cause: cause, rule: 'todays-gate-v0' } }),
            });
        };
        const blockList = async function (): Promise<[number, string[]][]> {
            const listed = (await (await queue.fetch('https://queue/blocks?state=unbuilt')).json()) as { blocks: { block: number; changes: { change: string }[] }[] };
            return listed.blocks.map((block) => [block.block, block.changes.map((item) => item.change)]);
        };
        // Block 1 is A alone (it opened the instant A cleared); B, C and D wait behind it.
        const a = await idOf(81);
        const b = await idOf(82);
        const c = await idOf(83);
        const d = await idOf(84);
        expect(await blockList()).toEqual([[1, [a]]]);
        await built(1, [{ tree: sha(181), change: a }]);
        await decide(a, sha(181), 'passed', null);
        // Decided: A lands alone, and block 1 holds the slot until it has, so block 2 builds on the main A makes.
        expect((await landings(queue)).map((order) => [order.change, order.future])).toEqual([[a, sha(181)]]);
        expect(await blockList()).toEqual([]);
        await report(queue, a, { main: sha(181), from: main, landed: sha(181) });
        expect(await blockList()).toEqual([[2, [b, c, d]]]);
        // Block 2's chain: main+B, +C, +D. C's prefix is red (C's own), so B's prefix lands, carrying B; D waits again.
        await built(2, [{ tree: sha(182), change: b }, { tree: sha(183), change: c }, { tree: sha(184), change: d }]);
        await decide(b, sha(182), 'passed', null);
        expect(await landings(queue)).toEqual([]);
        await decide(c, sha(183), 'failed', 'change');
        expect((await landings(queue)).map((order) => [order.change, order.future])).toEqual([[b, sha(182)]]);
        expect(await (await queue.fetch(`https://queue/changes/${c}`)).json()).toMatchObject({ state: 'red' });
        expect(await (await queue.fetch(`https://queue/changes/${d}`)).json()).toMatchObject({ state: 'queued', future: null });
        expect(await blockList()).toEqual([]);
        await report(queue, b, { main: sha(182), from: sha(181), landed: sha(182) });
        expect(await blockList()).toEqual([[3, [d]]]);
        await built(3, [{ tree: sha(185), change: d }]);
        await decide(d, sha(185), 'passed', null);
        expect((await landings(queue)).map((order) => order.change)).toEqual([d]);
        const replayed = await replay(await logOf(queue));
        expect([a, b, c, d].map((id) => replayed.changes.get(id)?.state)).toEqual(['landed', 'landed', 'red', 'testing']);
        expect(replayed.blocks.get(2)).toMatchObject({ resolved: true, landing: b });
    });

    it('lands a longer green prefix that carries the changes ahead of it, and waits on a void', async function () {
        const queue = await freshQueue();
        await runInDurableObject(queue, function (instance: Queue) {
            instance.blocksReady = true;
        });
        await queue.fetch('https://queue/rules', { method: 'POST', body: JSON.stringify({ rule: 'blocks', value: { on: true, budget: 8 }, commit: sha(99) }) });
        const ids: string[] = [];
        for (const seed of [91, 92, 93]) {
            ids.push(((await (await submit(queue, change(seed))).json()) as { change: string }).change);
        }
        const [x, y, z] = ids as [string, string, string];
        const post = (path: string, body: unknown): Promise<Response> => queue.fetch(`https://queue${path}`, { method: 'POST', body: JSON.stringify(body) });
        await post('/blocks/1/built', { base: main, prefixes: [{ tree: sha(191), change: x }], conflicts: [] });
        await post('/verdicts', { change: x, verdict: { future: sha(191), run: 'r1', status: 'passed', cause: null, rule: 'g' } });
        await report(queue, x, { main: sha(191), from: main, landed: sha(191) });
        await post('/blocks/2/built', { base: sha(191), prefixes: [{ tree: sha(192), change: y }, { tree: sha(193), change: z }], conflicts: [] });
        await post('/verdicts', { change: z, verdict: { future: sha(193), run: 'r3', status: 'passed', cause: null, rule: 'g' } });
        await post('/verdicts', { change: y, verdict: { future: sha(192), run: 'r2', status: 'void', cause: 'infra', rule: 'g' } });
        // Y's prefix voided: the block waits for its rerun.
        expect(await landings(queue)).toEqual([]);
        await post('/verdicts', { change: y, verdict: { future: sha(192), run: 'r2b', status: 'passed', cause: null, rule: 'g' } });
        expect((await landings(queue)).map((order) => [order.change, order.future])).toEqual([[z, sha(193)]]);
        await report(queue, z, { main: sha(193), from: sha(191), landed: sha(193) });
        expect(await (await queue.fetch(`https://queue/changes/${y}`)).json()).toMatchObject({ state: 'landed', landed: sha(193) });
    });
});

describe('outside verdicts, behind their switch', function () {
    it("are refused only after Judge decided a real change that landed, logged, and refusing one writes nothing", async function () {
        const queue = await freshQueue();
        const rules = function (body: unknown): Promise<Response> {
            return queue.fetch('https://queue/rules', { method: 'POST', body: JSON.stringify(body) });
        };
        const refuse = { rule: 'outsideVerdicts', value: { refused: true }, commit: sha(99) };
        expect((await rules({ rule: 'outsideVerdicts', value: { refused: true } })).status).toBe(400);
        // A landing today's gate decided with a whole verdict isn't Judge's: still not.
        const whole = ((await (await submit(queue, change(201))).json()) as { change: string }).change;
        expect((await postWhole(queue, whole, sha(201), 'passed', null)).status).toBe(200);
        expect((await report(queue, whole, { main: sha(201), from: main, landed: sha(201) })).status).toBe(200);
        expect((await rules(refuse)).status).toBe(422);
        // Judge decides one by its units, and it lands: now the switch may flip.
        const judged = ((await (await submit(queue, change(202))).json()) as { change: string }).change;
        const units = await planOf(['outside']);
        expect((await postPlan(queue, sha(202), units)).status).toBe(200);
        const key = units[0]?.unitKey ?? '';
        expect((await postBatch(queue, sha(202), batch(judged, sha(202), 'run-1', [record(judged, key, 'run-1', 'passed', null)], 'green'))).status).toBe(200);
        expect((await report(queue, judged, { main: sha(202), from: sha(201), landed: sha(202) })).status).toBe(200);
        expect((await rules(refuse)).status).toBe(200);
        // A whole verdict from outside is refused and logs nothing; the change waits for Judge.
        const waiting = ((await (await submit(queue, change(203))).json()) as { change: string }).change;
        const before = (await logOf(queue)).length;
        const refused = await postWhole(queue, waiting, sha(203), 'passed', null);
        expect(refused.status).toBe(409);
        expect(await refused.text()).toContain('outsideVerdicts');
        expect((await logOf(queue)).length).toBe(before);
        expect(await landings(queue)).toEqual([]);
        const log = await logOf(queue);
        expect(log.filter((event) => event.type === 'rule.changed').map((event) => event.data)).toEqual([{ rule: 'outsideVerdicts', value: { refused: true }, commit: sha(99) }]);
        expect((await replay(log)).rules.outsideVerdicts).toEqual({ refused: true });
        // The rollback: off again, today's gate decides as before.
        expect((await rules({ ...refuse, value: { refused: false } })).status).toBe(200);
        expect((await postWhole(queue, waiting, sha(203), 'passed', null)).status).toBe(200);
        expect(await landings(queue)).toEqual([expect.objectContaining({ change: waiting, future: sha(203) })]);
    });
});

describe('an empty plan', function () {
    it("is taken only for a Markdown-only future, listed for Judge, and decided only by Judge's docs rule", async function () {
        const queue = await freshQueue({
            [sha(302)]: facts({}, ['docs/a.md', 'internal/lower/a.go']),
            [sha(303)]: facts({}, ['docs/a.md']),
            [sha(304)]: facts({}, ['README.md', 'docs/a.md']),
            [sha(305)]: facts({}, ['docs/a.md', 'internal/lower/a.go']),
        });
        // Declaring only the Markdown of a diff that holds code is refused at the door, so it never reaches a plan.
        const hiding = await submit(queue, change(305, { paths: ['docs/a.md'] }));
        expect(hiding.status).toBe(422);
        expect(((await hiding.json()) as { reason: string }).reason).toContain('the paths leave out 1 of the diff base..sha: internal/lower/a.go');
        const idOf = async function (seed: number, paths: string[], fields: Record<string, unknown> = {}): Promise<string> {
            const answer = await submit(queue, change(seed, { paths: paths, ...fields }));
            return ((await answer.json()) as { change: string }).change;
        };
        const empty = { empty: true, reason: 'no unit key moved' };
        const code = await idOf(301, ['internal/lower/a.go']);
        expect((await postPlan(queue, sha(301), empty as unknown as unknown[])).status).toBe(422);
        const mixed = await idOf(302, ['docs/a.md', 'internal/lower/a.go']);
        expect((await postPlan(queue, sha(302), empty as unknown as unknown[])).status).toBe(422);
        const parity = await idOf(303, ['docs/a.md'], { parity: true });
        const parityFuture = ((await (await queue.fetch(`https://queue/changes/${parity}`)).json()) as { future: string }).future;
        expect((await postPlan(queue, parityFuture, empty as unknown as unknown[])).status).toBe(422);
        expect((await postPlan(queue, sha(304), { empty: true } as unknown as unknown[])).status).toBe(422);
        const docs = await idOf(304, ['docs/a.md', 'README.md']);
        expect((await postPlan(queue, sha(304), empty as unknown as unknown[])).status).toBe(200);
        expect((await postPlan(queue, sha(304), empty as unknown as unknown[])).status).toBe(200);
        const planned = (await (await queue.fetch('https://queue/futures?state=planned')).json()) as { futures: Record<string, unknown>[] };
        expect(planned.futures.find((future) => future.future === sha(304))).toMatchObject({ empty: true, reason: 'no unit key moved', rule: 'ruled-gate-docs-v0', units: [] });
        expect(await landings(queue)).toEqual([]);
        // Only Judge's docs rule decides it; any other rule is refused and logs nothing.
        const before = (await logOf(queue)).length;
        expect((await postBatch(queue, sha(304), { ...batch(docs, sha(304), 'run-1', [], 'green'), rule: 'judge-v1' })).status).toBe(422);
        expect((await logOf(queue)).length).toBe(before);
        expect(await landings(queue)).toEqual([]);
        const decided = await postBatch(queue, sha(304), { ...batch(docs, sha(304), 'run-1', [], 'green'), rule: 'ruled-gate-docs-v0' });
        expect(decided.status, await decided.clone().text()).toBe(200);
        expect(await landings(queue)).toEqual([expect.objectContaining({ change: docs, future: sha(304) })]);
        const replayed = await replay(await logOf(queue));
        expect(replayed.futures.get(sha(304))?.empty).toEqual({ reason: 'no unit key moved' });
        expect(replayed.futures.get(sha(304))?.decided).toEqual({ run: 'run-1', status: 'green' });
        expect([code, mixed, parity].every((id) => replayed.changes.get(id)?.state === 'queued')).toBe(true);
    });
});

describe('a withdrawn branch', function () {
    // Mutants: the owner unchecked, a landing order unchecked, a landed branch withdrawn, the branch left in the line,
    // its dependents left live.
    it('leaves the line for good, by its owner only, logged with why, and never under a landing order or once landed', async function () {
        const queue = await freshQueue();
        const idOf = async function (seed: number, fields: Record<string, unknown> = {}): Promise<string> {
            return ((await (await submit(queue, change(seed, fields))).json()) as { change: string }).change;
        };
        const withdraw = function (id: string, body: Record<string, unknown> = { owner: 'system_adamic_compiler', reason: 'superseded by another branch' }): Promise<Response> {
            return queue.fetch(`https://queue/changes/${id}/withdraw`, { method: 'POST', body: JSON.stringify(body) });
        };
        // Queued, with a branch stacked on it: only its owner withdraws it, and only with a reason.
        const superseded = await idOf(61);
        const stacked = await idOf(62, { parent: superseded });
        expect((await withdraw(superseded, { owner: 'system_adamic_other', reason: 'mine now' })).status).toBe(403);
        expect((await withdraw(superseded, { owner: 'system_adamic_compiler', reason: ' ' })).status).toBe(400);
        expect((await withdraw(superseded, { owner: 'system_adamic_compiler' })).status).toBe(400);
        const answer = await withdraw(superseded);
        expect(answer.status, await answer.clone().text()).toBe(200);
        expect(await answer.json()).toEqual({ change: superseded, state: 'withdrawn' });
        expect(await (await queue.fetch(`https://queue/changes/${superseded}`)).json()).toMatchObject({
            state: 'withdrawn',
            verdict: { withdrawn: 'superseded by another branch', by: 'system_adamic_compiler' },
        });
        // Its future leaves every listing, and the branch stacked on it parks.
        for (const listing of ['unplanned', 'planned']) {
            const futures = ((await (await queue.fetch(`https://queue/futures?state=${listing}`)).json()) as { futures: { future: string }[] }).futures;
            expect(futures.map((future) => future.future)).not.toContain(sha(61));
        }
        expect(await (await queue.fetch(`https://queue/changes/${stacked}`)).json()).toMatchObject({ state: 'parked' });
        // Once over, it's over: withdrawn again, refused.
        expect((await withdraw(superseded)).status).toBe(409);
        // A green branch with a landing order isn't withdrawn under the lander; once landed, never.
        const green = await idOf(63);
        expect((await postWhole(queue, green, sha(63), 'passed', null)).status).toBe(200);
        expect((await landings(queue)).map((order) => order.change)).toContain(green);
        const ordered = await withdraw(green);
        expect(ordered.status).toBe(409);
        expect(((await ordered.json()) as { reason: string }).reason).toContain('has a landing order');
        expect((await report(queue, green, { main: sha(70), from: main, landed: sha(63) })).status).toBe(200);
        expect((await withdraw(green)).status).toBe(409);
        // A red branch is ended rather than left to resubmit.
        const red = await idOf(64);
        await postWhole(queue, red, sha(64), 'failed', 'change');
        expect((await withdraw(red)).status).toBe(200);
        // Logged with who and why; replay reaches the same state.
        const logged = (await logOf(queue)).filter((event) => event.type === 'change.withdrawn');
        expect(logged).toMatchObject([
            { subject: { change: superseded }, data: { by: 'system_adamic_compiler', reason: 'superseded by another branch' } },
            { subject: { change: red } },
        ]);
        const replayed = await replay(await logOf(queue));
        expect([replayed.changes.get(superseded)?.state, replayed.changes.get(red)?.state, replayed.changes.get(stacked)?.state]).toEqual(['withdrawn', 'withdrawn', 'parked']);
        expect(replayed.line).not.toContain(superseded);
        expect(replayed.line).not.toContain(red);
    });
});

describe('a resubmit', function () {
    it('moves a red or parked change to a new sha under the same id, rechecked by git, and never back to a tested sha', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(41))).json()) as { change: string }).change;
        const resubmit = function (fields: Record<string, unknown>): Promise<Response> {
            return queue.fetch(`https://queue/changes/${id}/sha`, { method: 'POST', body: JSON.stringify({ ...change(42), ...fields }) });
        };
        await postWhole(queue, id, sha(41), 'failed', 'change');
        expect((await resubmit({ owner: 'system_adamic_other' })).status).toBe(403);
        expect((await resubmit({ sha: sha(41) })).status).toBe(422);
        expect((await resubmit({ parity: true })).status).toBe(422);
        const moved = await resubmit({});
        expect(moved.status, await moved.clone().text()).toBe(200);
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ state: 'queued', record: { change: id, sha: sha(42) }, future: null, position: 0 });
        // Unchecked until git clears the new sha, then its own future.
        expect(((await (await queue.fetch('https://queue/submissions?state=unchecked')).json()) as { changes: { sha: string }[] }).changes.map((item) => item.sha)).toEqual([sha(42)]);
        await queue.fetch(`https://queue/submissions/${id}/facts`, { method: 'POST', body: JSON.stringify(facts()) });
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ future: sha(42) });
        expect((await postWhole(queue, id, sha(42), 'passed', null)).status).toBe(200);
        expect((await landings(queue)).map((order) => order.future)).toEqual([sha(42)]);
        const restacked = (await logOf(queue)).find((event) => event.type === 'change.restacked');
        expect(restacked).toMatchObject({ subject: { change: id }, data: { from: sha(41), to: sha(42) } });
        expect((await replay(await logOf(queue))).changes.get(id)?.record.sha).toBe(sha(42));
    });

    it("moves a queued change with no plan and no verdict, so it's judged once on main's tip, and never a planned or judged one", async function () {
        const queue = await freshQueue();
        const idOf = async function (seed: number): Promise<string> {
            return ((await (await submit(queue, change(seed))).json()) as { change: string }).change;
        };
        const resubmit = function (id: string, seed: number): Promise<Response> {
            return queue.fetch(`https://queue/changes/${id}/sha`, { method: 'POST', body: JSON.stringify(change(seed)) });
        };
        const unplan = function (tree: string): Promise<Response> {
            return queue.fetch(`https://queue/futures/${tree}/unplan`, { method: 'POST', body: JSON.stringify({ by: 'system_adamic_loom', reason: 'replan on main' }) });
        };
        // Queued with no plan: moves, keeping its id and place.
        const fresh = await idOf(51);
        expect((await resubmit(fresh, 52)).status).toBe(200);
        expect(await (await queue.fetch(`https://queue/changes/${fresh}`)).json()).toMatchObject({ state: 'queued', record: { sha: sha(52) }, position: 0 });
        // Planned: refused while the plan stands; withdrawn, it moves.
        const planned = await idOf(53);
        const units = await planOf(['q']);
        expect((await postPlan(queue, sha(53), units)).status).toBe(200);
        expect((await resubmit(planned, 54)).status).toBe(409);
        expect((await unplan(sha(53))).status).toBe(200);
        expect((await resubmit(planned, 54)).status).toBe(200);
        // Judged (a void run): the change can't move. Its plan may be withdrawn, since a void is no verdict on the
        // change (#0zndrgw), and it still can't move after.
        const judged = await idOf(55);
        expect((await postPlan(queue, sha(55), units)).status).toBe(200);
        const key = units[0]?.unitKey ?? '';
        expect((await postBatch(queue, sha(55), batch(judged, sha(55), 'run-1', [record(judged, key, 'run-1', 'void', 'infra')], 'void'))).status).toBe(200);
        expect((await resubmit(judged, 56)).status).toBe(409);
        expect((await unplan(sha(55))).status).toBe(200);
        expect((await resubmit(judged, 56)).status).toBe(409);
        expect((await replay(await logOf(queue))).changes.get(planned)?.record.sha).toBe(sha(54));
    });
});

describe("a plan's resources", function () {
    it('ride from the plan to unit.planned and the judge listing, placement only, and a plan without them replays the same', async function () {
        const queue = await freshQueue();
        await submit(queue, change(71));
        const units = await planOf(['a', 'b']);
        expect((await postPlan(queue, sha(71), [{ ...units[0], resources: { memoryMegabytes: 0, cpus: 2 } }, units[1]])).status).toBe(422);
        expect((await postPlan(queue, sha(71), [{ ...units[0], resources: { memoryMegabytes: 4096, cpus: 2, disk: 1 } }, units[1]])).status).toBe(422);
        expect((await postPlan(queue, sha(71), [{ ...units[0], resources: { memoryMegabytes: 4096, cpus: 2 } }, units[1]])).status).toBe(200);
        const listed = (await (await queue.fetch('https://queue/futures?state=planned')).json()) as { futures: { units: Record<string, unknown>[] }[] };
        expect(listed.futures[0]?.units[0]).toMatchObject({ unitKey: units[0]?.unitKey, resources: { memoryMegabytes: 4096, cpus: 2 } });
        expect(listed.futures[0]?.units[1]).not.toHaveProperty('resources');
        const log = await logOf(queue);
        const replayed = await replay(log);
        expect(replayed.head).toBe((await (await queue.fetch('https://queue/head')).json() as { head: string }).head);
        expect(replayed.futures.get(sha(71))?.units?.get(units[0]?.unitKey ?? '')?.resources).toEqual({ memoryMegabytes: 4096, cpus: 2 });
        expect(replayed.futures.get(sha(71))?.units?.get(units[1]?.unitKey ?? '')?.resources).toBe(null);
    });
});

describe("a plan's tree key", function () {
    it('rides from a test unit of the plan to unit.planned and the planned listing, placement only, and a plan without one replays the same', async function () {
        const queue = await freshQueue();
        await submit(queue, change(72));
        const units = await planOf(['a', 'b']);
        const tree = 'f'.repeat(64);
        expect((await postPlan(queue, sha(72), [{ ...units[0], tree: 'F'.repeat(64) }, units[1]])).status).toBe(422);
        expect((await postPlan(queue, sha(72), [{ ...units[0], tree: tree.slice(1) }, units[1]])).status).toBe(422);
        const phase = { kind: 'phase', package: 'github.com/system-inc/adamic', select: { run: 'vet', skip: '' }, gateTools: 'a'.repeat(40) };
        const phaseUnit = { name: 'phase:vet', unitKey: await unitKeyOf(phase), keyParts: phase, decision: 'run', reason: 'phase', tree: tree };
        // A phase unit names its tree as a test unit does: its job takes the tree's npm packages from it (#v03v751). A build
        // unit does too, since its runner builds on the tree's source (#8j1qygw: the Queue refused every plan with one).
        const build = { ...units[1]?.keyParts, kind: 'build', package: 'github.com/system-inc/adamic/cmd/adamic-gate' };
        const buildUnit = { name: build.package, unitKey: await unitKeyOf(build), keyParts: build, decision: 'run', reason: 'build', tree: tree };
        expect((await postPlan(queue, sha(72), [{ ...units[0], tree: tree }, units[1], phaseUnit, buildUnit])).status).toBe(200);
        const listed = (await (await queue.fetch('https://queue/futures?state=planned')).json()) as { futures: { units: Record<string, unknown>[] }[] };
        expect(listed.futures[0]?.units[0]).toMatchObject({ unitKey: units[0]?.unitKey, tree: tree });
        expect(listed.futures[0]?.units[1]).not.toHaveProperty('tree');
        expect(listed.futures[0]?.units[2]).toMatchObject({ unitKey: phaseUnit.unitKey, tree: tree });
        expect(listed.futures[0]?.units[3]).toMatchObject({ unitKey: buildUnit.unitKey, tree: tree });
        const log = await logOf(queue);
        const replayed = await replay(log);
        expect(replayed.head).toBe((await (await queue.fetch('https://queue/head')).json() as { head: string }).head);
        expect(replayed.futures.get(sha(72))?.units?.get(units[0]?.unitKey ?? '')?.tree).toBe(tree);
        expect(replayed.futures.get(sha(72))?.units?.get(units[1]?.unitKey ?? '')?.tree).toBe(null);
    });
});

describe('a withdrawn plan', function () {
    it('goes back to the planner while nothing decided it green or red, logged with who and why, and never after one', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(21, { parity: true }))).json()) as { change: string }).change;
        const unplan = function (body: unknown): Promise<Response> {
            return queue.fetch(`https://queue/futures/${sha(21)}/unplan`, { method: 'POST', body: JSON.stringify(body) });
        };
        const ruling = { by: 'system_adamic_loom', reason: 'parity units carry the box record inputs (Loom, 00:3xZ)' };
        expect((await unplan(ruling)).status).toBe(409);
        const units = await planOf(['a']);
        await postPlan(queue, sha(21), units);
        expect((await unplan({ by: 'x' })).status).toBe(400);
        expect((await unplan(ruling)).status).toBe(200);
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ state: 'queued', units: { planned: 0 } });
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: { future: string }[] }).futures.map((future) => future.future)).toEqual([sha(21)]);
        // The replan with other keys is taken now.
        const replanned = await planOf(['b']);
        expect((await postPlan(queue, sha(21), replanned)).status).toBe(200);
        const run = function (attempt: number): string {
            return `future-${sha(21)}-${attempt}`;
        };
        const voided = batch(id, sha(21), run(1), [record(id, replanned[0]?.unitKey ?? '', run(1), 'void', 'infra')], 'void');
        expect((await postBatch(queue, sha(21), voided)).status).toBe(200);
        // A void is Loom's failure, not a verdict on the change (#0zndrgw): the plan can still be withdrawn, and the
        // future goes back to the planner, past the withdrawn plan's attempts, the one the judge started after the void
        // (2) among them.
        const pinMoved = { by: 'system_adamic_loom', reason: "release 5 moved the pin: the plan's keys name a runner no pool serves" };
        expect((await unplan(pinMoved)).status).toBe(200);
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: { future: string }[] }).futures.map((future) => future.future)).toEqual([sha(21)]);
        const third = await planOf(['c']);
        expect((await postPlan(queue, sha(21), third)).status).toBe(200);
        const planned = ((await (await queue.fetch('https://queue/futures?state=planned')).json()) as { futures: { future: string; attempt: number; firstAttempt?: number }[] }).futures;
        expect(planned.find((future) => future.future === sha(21))).toMatchObject({ attempt: 3, firstAttempt: 3 });
        // Nothing run on the withdrawn plan decides the new one: the attempt in flight when it was withdrawn is refused.
        const stale = batch(id, sha(21), run(2), [record(id, third[0]?.unitKey ?? '', run(2), 'failed', 'change')], 'red', [], [third[0]?.unitKey ?? '']);
        expect((await postBatch(queue, sha(21), stale)).status).toBe(422);
        // Once a red decides it, its plan stands.
        const red = batch(id, sha(21), run(3), [record(id, third[0]?.unitKey ?? '', run(3), 'failed', 'change')], 'red', [], [third[0]?.unitKey ?? '']);
        expect((await postBatch(queue, sha(21), red)).status).toBe(200);
        expect((await unplan(ruling)).status).toBe(409);
        const logged = (await logOf(queue)).filter((event) => event.type === 'future.unplanned');
        expect(logged).toMatchObject([{ subject: { change: id, future: sha(21) }, data: ruling }, { subject: { change: id, future: sha(21) }, data: pinMoved }]);
        const replayed = (await replay(await logOf(queue))).futures.get(sha(21));
        expect([replayed?.units?.size, replayed?.attemptsBefore, replayed?.decided?.status]).toEqual([1, 2, 'red']);
    });

    it('stands once a green decides it, even after a void before it', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(26, { parity: true }))).json()) as { change: string }).change;
        const run = function (attempt: number): string {
            return `future-${sha(26)}-${attempt}`;
        };
        const units = await planOf(['a']);
        await postPlan(queue, sha(26), units);
        expect((await postBatch(queue, sha(26), batch(id, sha(26), run(1), [record(id, units[0]?.unitKey ?? '', run(1), 'void', 'infra')], 'void'))).status).toBe(200);
        expect((await postBatch(queue, sha(26), batch(id, sha(26), run(2), [record(id, units[0]?.unitKey ?? '', run(2), 'passed', null)], 'green'))).status).toBe(200);
        const refused = await queue.fetch(`https://queue/futures/${sha(26)}/unplan`, { method: 'POST', body: JSON.stringify({ by: 'system_adamic_loom', reason: 'replan' }) });
        expect(refused.status).toBe(409);
        expect(await refused.json()).toEqual({ error: `future ${sha(26)} was decided green or red, so its plan stands` });
    });
});

describe('a witness of main', function () {
    it('is a parity run of a main commit with no paths, planned every unit uncached, and never lands', async function () {
        const queue = await freshQueue({ [sha(31)]: facts({}, []) });
        const witness = { sha: sha(31), base: sha(31), owner: 'system_adamic_loom_release', paths: [], parity: true, witness: true };
        expect((await submit(queue, { ...witness, parity: undefined })).status).toBe(422);
        expect((await submit(queue, { ...witness, base: main })).status).toBe(422);
        expect((await submit(queue, { ...witness, paths: ['a.go'] })).status).toBe(422);
        expect((await submit(queue, { ...witness, witness: 'yes' })).status).toBe(422);
        // A real change still needs paths.
        expect((await submit(queue, change(32, { paths: [] }))).status).toBe(422);
        const id = ((await (await submit(queue, witness)).json()) as { change: string }).change;
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: unknown[] }).futures).toMatchObject([
            { future: sha(31), parity: true, witness: true, uncached: true },
        ]);
        const units = await planOf(['a', 'b']);
        // The index holds a passed verdict for b, so a normal future could reuse it; a witness may not.
        const other = ((await (await submit(queue, change(35))).json()) as { change: string }).change;
        await postPlan(queue, sha(35), [units[1]]);
        expect((await postBatch(queue, sha(35), batch(other, sha(35), 'run-b', [record(other, units[1]?.unitKey ?? '', 'run-b', 'passed', null)], 'green'))).status).toBe(200);
        expect((await postPlan(queue, sha(31), [units[0], { ...units[1], decision: 'reuse', reused: 'run-b' }])).status).toBe(422);
        expect((await postPlan(queue, sha(31), units)).status).toBe(200);
        expect((await landings(queue)).map((order) => order.change)).toEqual([other]);
        expect(id).toMatch(/^chg_/);
    });
});

describe("main's red pause", function () {
    it('holds every green change while the newest decided witness of main is red, but a fix-forward or a revert, and lifts on a later green witness', async function () {
        const queue = await freshQueue({ [sha(40)]: headOf(40), [sha(41)]: headOf(41), [sha(42)]: headOf(42), [sha(45)]: facts({ revertOf: sha(9) }) });
        const idOf = async function (request: Record<string, unknown>): Promise<string> {
            const answer = await submit(queue, request);
            expect(answer.status, await answer.clone().text()).toBe(201);
            return ((await answer.json()) as { change: string }).change;
        };
        const witnessOf = function (seed: number): Record<string, unknown> {
            return { sha: sha(seed), base: sha(seed), owner: 'system_adamic_loom_judge', paths: [], parity: true, witness: true };
        };
        // Judge decides a future of one unit: green, or red on that unit.
        const decide = async function (id: string, tree: string, name: string, status: 'green' | 'red' | 'void'): Promise<void> {
            const units = await planOf([name]);
            const key = units[0]?.unitKey ?? '';
            expect((await postPlan(queue, tree, units)).status).toBe(200);
            const verdict = status === 'green' ? 'passed' : status === 'red' ? 'failed' : 'void';
            const records = [record(id, key, 'run-' + name, verdict, status === 'red' ? 'change' : status === 'void' ? 'infra' : null)];
            const answer = await postBatch(queue, tree, batch(id, tree, 'run-' + name, records, status, [], status === 'red' ? [key] : []));
            expect(answer.status, await answer.clone().text()).toBe(200);
        };
        const listed = async function (): Promise<string[]> {
            return (await landings(queue)).map((order) => order.change);
        };
        const older = await idOf(witnessOf(40));
        const red = await idOf(witnessOf(41));
        const green = await idOf(change(43));
        await decide(green, sha(43), 'g', 'green');
        expect(await listed()).toEqual([green]);
        // A void witness decides nothing about main.
        await decide(red, sha(41), 'w-void', 'void');
        expect(await listed()).toEqual([green]);
        expect((await logOf(queue)).filter((event) => event.type.startsWith('main.'))).toEqual([]);
        // The newest decided witness is red: main is held, and the green change waits, still green, never parked.
        const units = await planOf(['w-void']);
        const answer = await postBatch(queue, sha(41), batch(red, sha(41), 'run-2', [record(red, units[0]?.unitKey ?? '', 'run-2', 'failed', 'change')], 'red', [], [units[0]?.unitKey ?? '']));
        expect(answer.status, await answer.clone().text()).toBe(200);
        expect(await listed()).toEqual([]);
        expect(await (await queue.fetch(`https://queue/changes/${green}`)).json()).toMatchObject({ state: 'testing' });
        expect(await (await queue.fetch('https://queue/head')).json()).toMatchObject({ mainRed: { witness: red, main: sha(41), units: ['github.com/system-inc/adamic/internal/w-void'] } });
        // A fix-forward naming that red main lands, and so does a revert by git's facts; nothing else does.
        const fix = await idOf(change(44, { fixesRed: sha(41) }));
        await decide(fix, sha(44), 'f', 'green');
        const revert = await idOf(change(45));
        await decide(revert, sha(45), 'r', 'green');
        const wrongFix = await idOf(change(46, { fixesRed: sha(9) }));
        await decide(wrongFix, sha(46), 'x', 'green');
        expect((await listed()).sort()).toEqual([fix, revert].sort());
        // An older witness decided late says nothing about main now.
        await decide(older, sha(40), 'o', 'green');
        expect(await listed()).not.toContain(green);
        // A later witness, green, lifts the hold, and everything green lands again.
        const later = await idOf(witnessOf(42));
        await decide(later, sha(42), 'l', 'green');
        expect((await listed()).sort()).toEqual([green, fix, revert, wrongFix].sort());
        const log = await logOf(queue);
        expect(log.filter((event) => event.type.startsWith('main.')).map((event) => [event.type, event.data.witness])).toEqual([
            ['main.red', red],
            ['main.green', later],
        ]);
        expect((await replay(log)).mainRed).toBe(null);
    });
});

describe('a green witness of main', function () {
    const witnessOf = function (seed: number): Record<string, unknown> {
        return { sha: sha(seed), base: sha(seed), owner: 'system_adamic_loom', paths: [], parity: true, witness: true };
    };
    const idOf = async function (queue: DurableObjectStub<Queue>, request: Record<string, unknown>): Promise<string> {
        const answer = await submit(queue, request);
        expect(answer.status, await answer.clone().text()).toBe(201);
        return ((await answer.json()) as { change: string }).change;
    };
    // The judge's run of one attempt, as the placer names it.
    const runOf = function (tree: string, attempt: number): string {
        return `future-${tree}-${attempt}`;
    };
    // Judge decides one run of the future's one unit: green, red on that unit, or void.
    const judge = async function (queue: DurableObjectStub<Queue>, id: string, tree: string, run: string, status: 'green' | 'red' | 'void'): Promise<void> {
        const units = await planOf(['w']);
        const key = units[0]?.unitKey ?? '';
        const planned = await postPlan(queue, tree, units);
        expect(planned.status, await planned.clone().text()).toBe(200);
        const verdict = status === 'green' ? 'passed' : status === 'red' ? 'failed' : 'void';
        const records = [record(id, key, run, verdict, status === 'red' ? 'change' : status === 'void' ? 'infra' : null)];
        const answer = await postBatch(queue, tree, batch(id, tree, run, records, status, [], status === 'red' ? [key] : []));
        expect(answer.status, await answer.clone().text()).toBe(200);
    };
    const stateOf = async function (queue: DurableObjectStub<Queue>, id: string): Promise<string> {
        return ((await (await queue.fetch(`https://queue/changes/${id}`)).json()) as { state: string }).state;
    };
    const mainEvents = async function (queue: DurableObjectStub<Queue>): Promise<[string, unknown, unknown][]> {
        return (await logOf(queue)).filter((event) => event.type.startsWith('main.')).map((event) => [event.type, event.data.witness, event.data.main]);
    };
    const planned = async function (queue: DurableObjectStub<Queue>): Promise<Record<string, unknown>[]> {
        return ((await (await queue.fetch('https://queue/futures?state=planned')).json()) as { futures: Record<string, unknown>[] }).futures;
    };

    it('finishes witnessed, a state of its own the board shows, and records main green on a fresh queue', async function () {
        const queue = await freshQueue({ [sha(51)]: headOf(51) });
        const id = await idOf(queue, witnessOf(51));
        await judge(queue, id, sha(51), runOf(sha(51), 1), 'green');
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ state: 'witnessed', verdict: { status: 'green' } });
        // Finished: nothing waits on the judge, and no landing order is ever written for it.
        expect(await planned(queue)).toEqual([]);
        expect(await landings(queue)).toEqual([]);
        const log = await logOf(queue);
        expect(log.slice(-2).map((event) => [event.type, event.subject.change, event.data])).toEqual([
            ['change.witnessed', id, { decision: { status: 'green', red: [], excused: [], problems: [] } }],
            // Main was never red here, and its green is recorded all the same.
            ['main.green', id, { witness: id, main: sha(51), cleared: null }],
        ]);
        expect(await (await queue.fetch('https://queue/head')).json()).toMatchObject({ mainRed: null });
        const replayed = await replay(log);
        expect(replayed.changes.get(id)?.state).toBe('witnessed');
        expect(replayed.mainTip).toBe(sha(51));
        // The board shows it finished.
        await runDurableObjectAlarm(queue);
        const board = (env as unknown as { ChangeBoard: DurableObjectNamespace }).ChangeBoard;
        const lines = ((await (await board.get(board.idFromName('board')).fetch('https://board/changes')).json()) as { changes: Record<string, unknown>[] }).changes;
        const line = lines.find((held) => held.change === id);
        expect(line).toMatchObject({ state: 'witnessed', sha: sha(51), units: { planned: 1, passed: 1 } });
        expect(line?.finishedAt).toMatch(/^\d{4}-/);
    });

    it('may witness the same sha again once the first is finished, never while it is live, in fresh attempts nothing carries into', async function () {
        const queue = await freshQueue({ [sha(52)]: headOf(52) });
        const first = await idOf(queue, witnessOf(52));
        // Live, the sha is in the line once.
        const twice = await submit(queue, witnessOf(52));
        expect(twice.status).toBe(422);
        expect(await twice.json()).toEqual({ reason: `sha ${sha(52)} is already in the line as ${first}` });
        await judge(queue, first, sha(52), runOf(sha(52), 1), 'void');
        expect(await planned(queue)).toMatchObject([{ future: sha(52), attempt: 2 }]);
        expect((await planned(queue))[0]).not.toHaveProperty('firstAttempt');
        expect((await submit(queue, witnessOf(52))).status).toBe(422);
        await judge(queue, first, sha(52), runOf(sha(52), 2), 'green');
        // Finished, the same sha is witnessed again: a new change, a new future of the same tree.
        const second = await idOf(queue, witnessOf(52));
        expect(second).not.toBe(first);
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: unknown[] }).futures).toMatchObject([
            { future: sha(52), changes: [second], witness: true, uncached: true },
        ]);
        expect((await postPlan(queue, sha(52), await planOf(['w']))).status).toBe(200);
        // Attempts 1 and 2 were the first witness's runs: the second starts at 3, and the judge carries from 3 on only.
        expect(await planned(queue)).toMatchObject([{ future: sha(52), attempt: 3, firstAttempt: 3, change: { change: second } }]);
        // The first witness's batch, posted late, decides nothing about the second.
        const units = await planOf(['w']);
        const late = batch(first, sha(52), runOf(sha(52), 2), [record(first, units[0]?.unitKey ?? '', runOf(sha(52), 2), 'passed', null)], 'green');
        expect((await postBatch(queue, sha(52), late)).status).toBe(422);
        expect(await stateOf(queue, second)).toBe('testing');
        await judge(queue, second, sha(52), runOf(sha(52), 3), 'green');
        expect(await stateOf(queue, second)).toBe('witnessed');
        expect(await stateOf(queue, first)).toBe('witnessed');
        expect(await mainEvents(queue)).toEqual([
            ['main.green', first, sha(52)],
            ['main.green', second, sha(52)],
        ]);
        const replayed = await replay(await logOf(queue));
        expect(replayed.futures.get(sha(52))).toMatchObject({ attemptsBefore: 2, changes: [second], decided: { run: runOf(sha(52), 3), status: 'green' } });
    });

    it("records main green only for main's tip as the log knows it, never for a sha main has moved past", async function () {
        const queue = await freshQueue({ [sha(61)]: headOf(61), [sha(62)]: headOf(62), [sha(63)]: headOf(63), [sha(80)]: headOf(80) });
        const stale = await idOf(queue, witnessOf(61));
        const tip = await idOf(queue, witnessOf(62));
        // A witness of a newer main was cleared after it, so main is 62 now: the stale green finishes, and says nothing.
        await judge(queue, stale, sha(61), runOf(sha(61), 1), 'green');
        expect(await stateOf(queue, stale)).toBe('witnessed');
        expect(await mainEvents(queue)).toEqual([]);
        await judge(queue, tip, sha(62), runOf(sha(62), 1), 'green');
        expect(await mainEvents(queue)).toEqual([['main.green', tip, sha(62)]]);
        // A landing moves main past a witness on its way: its green says nothing either.
        const passing = await idOf(queue, witnessOf(63));
        const landing = await idOf(queue, change(64));
        expect((await postWhole(queue, landing, sha(64), 'passed', null)).status).toBe(200);
        expect((await report(queue, landing, { main: sha(80), from: main, landed: sha(64) })).status).toBe(200);
        await judge(queue, passing, sha(63), runOf(sha(63), 1), 'green');
        expect(await stateOf(queue, passing)).toBe('witnessed');
        expect(await mainEvents(queue)).toEqual([['main.green', tip, sha(62)]]);
        // A witness of the main that landing made is the tip, and its green is recorded.
        const landed = await idOf(queue, witnessOf(80));
        await judge(queue, landed, sha(80), runOf(sha(80), 1), 'green');
        expect(await mainEvents(queue)).toEqual([
            ['main.green', tip, sha(62)],
            ['main.green', landed, sha(80)],
        ]);
        expect((await replay(await logOf(queue))).mainTip).toBe(sha(80));
    });

    it('leaves a log written before witnesses finished to replay exactly as it did', async function () {
        const queue = await freshQueue({ [sha(71)]: headOf(71) });
        const witness = await idOf(queue, witnessOf(71));
        await judge(queue, witness, sha(71), runOf(sha(71), 1), 'green');
        // A parity run red, then the same sha as a parity run again (a red one is finished): a future of the same tree.
        const red = await idOf(queue, change(72, { parity: true }));
        await judge(queue, red, sha(72), runOf(sha(72), 1), 'red');
        const again = await idOf(queue, change(72, { parity: true }));
        const log = await logOf(queue);
        // The log as the queue wrote it before this: no change.witnessed, no main.green without a red to clear, and no
        // attemptsBefore on a future.built, chained again from genesis.
        const older: QueueEvent[] = [];
        let prev = GenesisHash;
        for (const event of log) {
            if (event.type === 'change.witnessed' || event.type === 'main.green') {
                continue;
            }
            const data = { ...event.data };
            delete data.attemptsBefore;
            const kept = { ...event, seq: older.length + 1, prev: prev, data: data };
            older.push(kept);
            prev = await sha256Text(canonical(kept));
        }
        expect(older.length).toBe(log.length - 2);
        const before = await replay(older);
        const now = await replay(log);
        // The witness stays testing, live, so its sha is still refused; the parity run's second future starts at
        // attempt 1, as it did; every other change is where the new log puts it.
        expect(before.changes.get(witness)?.state).toBe('testing');
        expect(before.mainRed).toBe(null);
        expect(before.futures.get(sha(72))?.attemptsBefore).toBe(0);
        const request = { sha: sha(71), base: sha(71), owner: 'system_adamic_loom', paths: [], parent: null, fixesRed: null, parity: true as const, witness: true as const };
        expect(lineRefusalOf(request, before)).toBe(`sha ${sha(71)} is already in the line as ${witness}`);
        expect(now.changes.get(witness)?.state).toBe('witnessed');
        expect(now.futures.get(sha(72))?.attemptsBefore).toBe(1);
        expect(lineRefusalOf(request, now)).toBe(null);
        for (const [id, entry] of now.changes) {
            if (id !== witness) {
                expect(before.changes.get(id)?.state, id).toBe(entry.state);
            }
        }
        expect(now.changes.get(red)?.state).toBe('red');
        expect(now.changes.get(again)?.state).toBe('queued');
    });

    // The review of 844ad60 (#6gj7n9p): each of its findings, as the queue must answer it.

    // A queue whose git facts come from the bridge, as they do today: posted with the queue's seq when git was read.
    const bridged = async function (): Promise<{
        queue: DurableObjectStub<Queue>;
        post: (id: string, body: unknown) => Promise<Response>;
        seq: () => Promise<number>;
    }> {
        const queue = await freshQueue();
        await runInDurableObject(queue, function (instance: Queue) {
            instance.history = null;
        });
        return {
            queue: queue,
            post: function (id: string, body: unknown): Promise<Response> {
                return queue.fetch(`https://queue/submissions/${id}/facts`, { method: 'POST', body: JSON.stringify(body) });
            },
            seq: async function (): Promise<number> {
                return ((await (await queue.fetch('https://queue/head')).json()) as { seq: number }).seq;
            },
        };
    };

    it("never lets the green of an older main commit clear a red on main's head (F1)", async function () {
        const queue = await freshQueue({ [sha(161)]: headOf(161, 162), [sha(162)]: headOf(162) });
        const tip = await idOf(queue, witnessOf(162));
        await judge(queue, tip, sha(162), runOf(sha(162), 1), 'red');
        // 161 is on main, behind its head: git read main's head as 162.
        const older = await idOf(queue, witnessOf(161));
        await judge(queue, older, sha(161), runOf(sha(161), 1), 'green');
        expect(await stateOf(queue, older)).toBe('witnessed');
        expect(await mainEvents(queue)).toEqual([['main.red', tip, sha(162)]]);
        expect(await (await queue.fetch('https://queue/head')).json()).toMatchObject({ mainRed: { witness: tip, main: sha(162) } });
    });

    it("says nothing about main for a witness a bridge from before mainHead checked, even of main's tip", async function () {
        const { queue, post } = await bridged();
        // A landing makes 165 main's tip as the log knows it; the witnesses' facts, from a bridge before mainHead, don't say.
        const landing = await idOf(queue, change(164));
        expect((await post(landing, facts())).status).toBe(200);
        expect((await postWhole(queue, landing, sha(164), 'passed', null)).status).toBe(200);
        expect((await report(queue, landing, { main: sha(165), from: main, landed: sha(164) })).status).toBe(200);
        const red = await idOf(queue, witnessOf(165));
        expect((await post(red, facts({}, []))).status).toBe(200);
        await judge(queue, red, sha(165), runOf(sha(165), 1), 'red');
        const green = await idOf(queue, witnessOf(166));
        expect((await post(green, facts({}, []))).status).toBe(200);
        await judge(queue, green, sha(166), runOf(sha(166), 1), 'green');
        expect([await stateOf(queue, red), await stateOf(queue, green)]).toEqual(['red', 'witnessed']);
        expect(await mainEvents(queue)).toEqual([]);
    });

    it("takes nothing on the GitHub path when main's head can't be read, so no change is checked without it (G1)", async function () {
        const queue = await freshQueue({ [sha(167)]: { ...facts({}, []), mainHead: undefined }, [sha(168)]: { ...facts(), mainHead: undefined } });
        for (const request of [witnessOf(167), change(168)]) {
            const answer = await submit(queue, request);
            expect(answer.status).toBe(503);
            expect(await answer.json()).toEqual({ reason: "main's head can't be read from GitHub, try again" });
        }
        expect(await logOf(queue)).toEqual([]);
        // Read again with its head, the witness is taken, of main's head.
        await runInDurableObject(queue, function (instance: Queue) {
            const history = instance.history;
            instance.history = {
                async facts(asked: string, base: string): Promise<GitFacts> {
                    return asked === sha(167) ? headOf(167) : (history?.facts(asked, base) ?? facts());
                },
            };
        });
        const id = await idOf(queue, witnessOf(167));
        await judge(queue, id, sha(167), runOf(sha(167), 1), 'green');
        expect(await mainEvents(queue)).toEqual([['main.green', id, sha(167)]]);
    });

    it("orders readings of main's head by when they began, so facts that arrive late never move the tip back (F2)", async function () {
        const { queue, post, seq } = await bridged();
        const stale = await idOf(queue, witnessOf(171));
        // The bridge began reading 171's facts here, while main's head was 171, and posts them last.
        const readStale = await seq();
        const tip = await idOf(queue, witnessOf(172));
        expect((await post(tip, { ...headOf(172), asOf: await seq() })).status).toBe(200);
        expect((await post(stale, { ...headOf(171), asOf: readStale })).status).toBe(200);
        expect((await replay(await logOf(queue))).mainTip).toBe(sha(172));
        await judge(queue, stale, sha(171), runOf(sha(171), 1), 'green');
        expect(await mainEvents(queue)).toEqual([]);
        await judge(queue, tip, sha(172), runOf(sha(172), 1), 'green');
        expect(await mainEvents(queue)).toEqual([['main.green', tip, sha(172)]]);
        // mainHead and asOf come together, and asOf is never past the log.
        const loose = await idOf(queue, witnessOf(173));
        expect((await post(loose, headOf(173))).status).toBe(400);
        expect((await post(loose, { ...facts({}, []), asOf: 1 })).status).toBe(400);
        expect((await post(loose, { ...headOf(173), asOf: -1 })).status).toBe(400);
        expect((await post(loose, { ...headOf(173), asOf: (await seq()) + 1 })).status).toBe(422);
        expect((await post(loose, { ...headOf(173), asOf: await seq() })).status).toBe(200);
    });

    it('leaves the tip unknown when two readings begun at the same seq disagree, until a newer one', async function () {
        const { queue, post, seq } = await bridged();
        const first = await idOf(queue, witnessOf(176));
        const second = await idOf(queue, witnessOf(177));
        const began = await seq();
        expect((await post(first, { ...headOf(176), asOf: began })).status).toBe(200);
        expect((await post(second, { ...headOf(177), asOf: began })).status).toBe(200);
        expect((await replay(await logOf(queue))).mainTip).toBe(null);
        await judge(queue, first, sha(176), runOf(sha(176), 1), 'green');
        await judge(queue, second, sha(177), runOf(sha(177), 1), 'red');
        expect(await mainEvents(queue)).toEqual([]);
        const third = await idOf(queue, witnessOf(178));
        expect((await post(third, { ...headOf(178), asOf: await seq() })).status).toBe(200);
        await judge(queue, third, sha(178), runOf(sha(178), 1), 'green');
        expect(await mainEvents(queue)).toEqual([['main.green', third, sha(178)]]);
    });

    it('never records main green for a head main moved past, whether it landed before git was read or while git answered (F3)', async function () {
        // Read after the landing, git says main's head is 190: 181 is behind it.
        const queue = await freshQueue({ [sha(181)]: headOf(181, 190) });
        const landing = await idOf(queue, change(184));
        expect((await postWhole(queue, landing, sha(184), 'passed', null)).status).toBe(200);
        expect((await report(queue, landing, { main: sha(190), from: main, landed: sha(184) })).status).toBe(200);
        const old = await idOf(queue, witnessOf(181));
        await judge(queue, old, sha(181), runOf(sha(181), 1), 'green');
        expect(await mainEvents(queue)).toEqual([]);
        // Read while a landing is logged: git said 185 was main's head, but the landing after the reading began outranks it.
        await runInDurableObject(queue, function (instance: Queue) {
            instance.history = null;
        });
        const post = function (id: string, body: unknown): Promise<Response> {
            return queue.fetch(`https://queue/submissions/${id}/facts`, { method: 'POST', body: JSON.stringify(body) });
        };
        const racing = await idOf(queue, witnessOf(185));
        const began = ((await (await queue.fetch('https://queue/head')).json()) as { seq: number }).seq;
        const second = await idOf(queue, change(186));
        expect((await post(second, facts())).status).toBe(200);
        expect((await postWhole(queue, second, sha(186), 'passed', null)).status).toBe(200);
        expect((await report(queue, second, { main: sha(191), from: sha(190), landed: sha(186) })).status).toBe(200);
        expect((await post(racing, { ...headOf(185), asOf: began })).status).toBe(200);
        await judge(queue, racing, sha(185), runOf(sha(185), 1), 'green');
        expect(await stateOf(queue, racing)).toBe('witnessed');
        expect(await mainEvents(queue)).toEqual([]);
        expect((await replay(await logOf(queue))).mainTip).toBe(sha(191));
    });

    it("decides main by its tip, never by line positions, which shrink, or the future two witnesses of a sha share (F4)", async function () {
        const { queue, post, seq } = await bridged();
        const ahead = [await idOf(queue, change(191)), await idOf(queue, change(192))];
        const first = await idOf(queue, witnessOf(201));
        expect((await post(first, { ...headOf(201), asOf: await seq() })).status).toBe(200);
        await judge(queue, first, sha(201), runOf(sha(201), 1), 'green');
        // The changes ahead are refused, so the line shrinks and the second witness's position is below the first's.
        for (const id of ahead) {
            expect(await (await post(id, facts({ shaExists: false }))).json()).toMatchObject({ state: 'refused' });
        }
        const second = await idOf(queue, witnessOf(201));
        const positions = await replay(await logOf(queue));
        expect([positions.changes.get(first)?.position, positions.changes.get(second)?.position]).toEqual([2, 1]);
        expect((await post(second, { ...headOf(201), asOf: await seq() })).status).toBe(200);
        await judge(queue, second, sha(201), runOf(sha(201), 2), 'red');
        expect(await mainEvents(queue)).toEqual([
            ['main.green', first, sha(201)],
            ['main.red', second, sha(201)],
        ]);
        expect(await (await queue.fetch('https://queue/head')).json()).toMatchObject({ mainRed: { witness: second, main: sha(201) } });
    });

    it("refuses a re-witness's batch that cites an earlier future's run, as a judge that doesn't read firstAttempt would (F5)", async function () {
        const queue = await freshQueue({ [sha(221)]: headOf(221) });
        const first = await idOf(queue, witnessOf(221));
        await judge(queue, first, sha(221), runOf(sha(221), 1), 'green');
        const second = await idOf(queue, witnessOf(221));
        const units = await planOf(['w']);
        const key = units[0]?.unitKey ?? '';
        expect((await postPlan(queue, sha(221), units)).status).toBe(200);
        const batchOf = function (run: string, rule = 'judge-v1'): Record<string, unknown> {
            return batch(second, sha(221), run, [{ ...record(second, key, run, 'passed', null), rule: rule }], 'green');
        };
        const refused: [string, string?][] = [
            // The old run itself, the old judge's run name for this attempt.
            [runOf(sha(221), 1)],
            // This attempt's run, carrying the old run's pass.
            [runOf(sha(221), 2), `judge-v1 carried ${runOf(sha(221), 1)}`],
            // A phase unit's carried pass, as the judge names it (G2).
            [runOf(sha(221), 2), `judge-v1 phase carried ${runOf(sha(221), 1)}`],
            // A run that names no attempt of this tree.
            ['run-x'],
            [runOf(sha(222), 2)],
        ];
        for (const [run, rule] of refused) {
            const answer = await postBatch(queue, sha(221), batchOf(run, rule));
            expect(answer.status, `${run} ${rule ?? ''}`).toBe(422);
        }
        expect(await stateOf(queue, second)).toBe('testing');
        expect((await postBatch(queue, sha(221), batchOf(runOf(sha(221), 2)))).status).toBe(200);
        expect(await stateOf(queue, second)).toBe('witnessed');
    });

    it('replays every new event to exactly the live state, and an old log exactly as the queue before them did', async function () {
        const plain = function (value: unknown): unknown {
            return JSON.parse(JSON.stringify(value, (_key, inner: unknown) => (inner instanceof Map ? [...inner.entries()] : inner)));
        };
        const queue = await freshQueue({ [sha(231)]: headOf(231), [sha(232)]: headOf(232) });
        const red = await idOf(queue, witnessOf(231));
        await judge(queue, red, sha(231), runOf(sha(231), 1), 'red');
        const again = await idOf(queue, witnessOf(231));
        await judge(queue, again, sha(231), runOf(sha(231), 2), 'void');
        await judge(queue, again, sha(231), runOf(sha(231), 3), 'green');
        await idOf(queue, witnessOf(231));
        const landing = await idOf(queue, change(233));
        expect((await postWhole(queue, landing, sha(233), 'passed', null)).status).toBe(200);
        expect((await report(queue, landing, { main: sha(240), from: main, landed: sha(233) })).status).toBe(200);
        const log = await logOf(queue);
        const live = await runInDurableObject(queue, (instance: Queue) => plain((instance as unknown as { state: unknown }).state));
        expect(plain(await replay(log))).toEqual(live);
        expect(log.map((event) => event.type)).toContain('change.witnessed');
        // A log with no new event or field (this scenario's, stripped), and its replay by 09bb1dd's apply, pinned: the
        // new apply reaches the same state, with each new field at its empty value.
        const older = plain(await replay(oldQueueLog.log as QueueEvent[])) as {
            mainTip?: unknown;
            mainTipRank?: unknown;
            futures: [string, Record<string, unknown>][];
            changes: [string, Record<string, unknown>][];
        };
        expect([older.mainTip, older.mainTipRank]).toEqual([sha(240), 2 * oldQueueLog.log.findIndex((event) => event.type === 'change.landed') + 2]);
        delete older.mainTip;
        delete older.mainTipRank;
        for (const [, future] of older.futures) {
            expect(future.attemptsBefore).toBe(0);
            delete future.attemptsBefore;
            // decisive is derived, true exactly where a green or red decided the future.
            const decided = future.decided as { status: string } | null;
            expect(future.decisive).toBe(decided !== null && decided.status !== 'void');
            delete future.decisive;
        }
        for (const [, entry] of older.changes) {
            expect(entry.mainHead).toBe(null);
            delete entry.mainHead;
        }
        expect(older).toEqual(oldQueueLog.state);
    });
});

describe('the replay proof', function () {
    it('reads the whole log and the head, and replaying the log reaches exactly that head and main', async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(33))).json()) as { change: string }).change;
        await postWhole(queue, id, sha(33), 'passed', null);
        await report(queue, id, { main: sha(80), from: main, landed: sha(33) });
        const head = (await (await queue.fetch('https://queue/head')).json()) as { seq: number; head: string; landedMain: string };
        const lines = (await (await queue.fetch('https://queue/log?after=0')).text())
            .trim()
            .split('\n')
            .map(function (line) {
                return JSON.parse(line) as QueueEvent;
            });
        const replayed = await replay(lines);
        expect(head).toEqual({ seq: replayed.seq, head: replayed.head, landedMain: sha(80), mainRed: replayed.mainRed });
        expect(replayed.landedMain).toBe(sha(80));
        expect(lines).toHaveLength(head.seq);
    });
});

describe('a parity run', function () {
    it("plans exactly the box record's selection when it carries one, uncached", async function () {
        const queue = await freshQueue();
        expect((await submit(queue, change(8, { select: { packages: ['x'] } }))).status).toBe(422);
        expect((await submit(queue, change(8, { parity: true, select: { packages: [] } }))).status).toBe(422);
        const select = { packages: ['github.com/system-inc/adamic/internal/a', 'github.com/system-inc/adamic/internal/b'], tests: { 'github.com/system-inc/adamic/internal/b': ['TestB1'] } };
        const id = ((await (await submit(queue, change(8, { parity: true, select: select }))).json()) as { change: string }).change;
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: unknown[] }).futures).toMatchObject([{ future: sha(8), parity: true, select: select }]);
        const units = await planOf(['a', 'b', 'c']);
        // A plan of other packages, or one that reuses a verdict, is refused.
        expect((await postPlan(queue, sha(8), units)).status).toBe(422);
        expect((await postPlan(queue, sha(8), units.slice(0, 1))).status).toBe(422);
        expect((await postPlan(queue, sha(8), [units[0], { ...units[1], decision: 'reuse' }])).status).toBe(422);
        // Phase units ride along, outside the package comparison; a wrong test package still refuses with them.
        const phaseParts = { kind: 'phase', phase: 'vet', tools: { runner: 'e'.repeat(64) }, gateInputs: 'f'.repeat(64) };
        const phase = { name: 'phase/vet', unitKey: await unitKeyOf(phaseParts), keyParts: phaseParts, decision: 'run', reason: 'new key' };
        expect((await postPlan(queue, sha(8), [units[0], units[2], phase])).status).toBe(422);
        const planned = await postPlan(queue, sha(8), [...units.slice(0, 2), phase]);
        expect(planned.status, await planned.clone().text()).toBe(200);
        expect(id).toMatch(/^chg_/);
    });

    it("is tested on exactly merge(base, sha), its records logged whole, and never gets a landing order", async function () {
        const queue = await freshQueue();
        const id = ((await (await submit(queue, change(1, { parity: true }))).json()) as { change: string }).change;
        expect((await submit(queue, change(2, { parity: true, parent: id }))).status).toBe(422);
        expect((await submit(queue, change(3, { parity: 'yes' }))).status).toBe(422);
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ record: { parity: true }, future: sha(1) });
        // Nothing moves it to a newer main: a gate merge is refused for it.
        const moved = await queue.fetch('https://queue/verdicts', {
            method: 'POST',
            body: JSON.stringify({ change: id, verdict: { future: sha(60), run: 'm', status: 'passed', cause: null, rule: 'todays-gate-v0' }, gateMerge: { base: sha(61) } }),
        });
        expect(moved.status).toBe(409);
        expect(((await (await queue.fetch('https://queue/futures?state=unplanned')).json()) as { futures: { parity: boolean }[] }).futures).toMatchObject([{ future: sha(1), parity: true }]);
        const units = await planOf(['a']);
        await postPlan(queue, sha(1), units);
        expect(((await (await queue.fetch('https://queue/futures?state=planned')).json()) as { futures: { parity: boolean }[] }).futures).toMatchObject([{ future: sha(1), parity: true }]);
        const records = [{ ...record(id, units[0]?.unitKey ?? '', 'run-p', 'passed', null), tests: [{ package: 'p', test: 'TestA', outcome: 'pass' }] }];
        expect((await postBatch(queue, sha(1), batch(id, sha(1), 'run-p', records, 'green'))).status).toBe(200);
        // Green, and still no landing order, and the pusher's report is refused.
        expect(await landings(queue)).toEqual([]);
        expect((await report(queue, id, { main: sha(50), from: main, landed: sha(1) })).status).toBe(409);
        const decided = (await logOf(queue)).find(function (event) {
            return event.type === 'verdict.decided' && event.subject.unitKey !== undefined;
        });
        expect(decided?.data.verdict).toMatchObject({ tests: [{ package: 'p', test: 'TestA', outcome: 'pass' }], future: sha(1) });
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
                    return [key, { unitKey: key, name: `u${index}`, keyParts: {}, decision: 'run' as const, reused: null, resources: null, tree: null, verdict: verdict === null ? null : { ...verdict, unitKey: key } }];
                }),
            );
            return { tree: sha(1), base: main, changes: [], units: units, empty: null, whole: null, decided: { run: 'r', status: 'green' }, voids: 0, judged: true, decisive: true, attemptsBefore: 0 };
        };
        expect(futureLandable(futureWith([verdictOf('', 'passed', null), verdictOf('', 'failed', 'mainRed')]))).toBe(true);
        expect(futureLandable(futureWith([verdictOf('', 'passed', null), verdictOf('', 'void', 'infra')]))).toBe(false);
        expect(futureLandable(futureWith([verdictOf('', 'passed', null), null]))).toBe(false);
        expect(futureLandable(futureWith([verdictOf('', 'passed', null), verdictOf('', 'passed', null, sha(2))]))).toBe(false);
        expect(futureLandable(futureWith([]))).toBe(false);
        // An empty plan lands on Judge's green alone, since nothing is left to recompute.
        expect(futureLandable({ ...futureWith([]), empty: { reason: 'no key moved' } })).toBe(true);
        expect(futureLandable({ ...futureWith([]), empty: { reason: 'no key moved' }, decided: null })).toBe(false);
        expect(futureLandable({ ...futureWith([verdictOf('', 'passed', null)]), decided: { run: 'r', status: 'void' } })).toBe(false);
    });
});

describe("loom's queue seams", function () {
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
            ['GET', '/log?after=0'],
            ['GET', '/head'],
            ['POST', '/rules'],
            ['GET', '/blocks?state=unbuilt'],
            ['POST', `/submissions/chg_${'q'.repeat(26)}/facts`],
        ];
        const board = await token('system_adamic_loom_release', 'board');
        for (const [method, path] of routes) {
            const body = method === 'POST' ? '{}' : undefined;
            expect((await call(path, { method: method, body: body })).status, path).toBe(401);
            // A board token reads the log and its head, and nothing else.
            const readsLog = method === 'GET' && (path.startsWith('/log') || path === '/head');
            expect((await call(path, { method: method, body: body, bearer: board })).status === 403, `board ${path}`).toBe(!readsLog);
            for (const scope of ['submit', 'runner', 'pool'] as const) {
                expect((await call(path, { method: method, body: body, bearer: await token('system_adamic_loom', scope) })).status, `${scope} ${path}`).toBe(403);
            }
        }
        expect(await (await call('/landings', { bearer: coordinator })).json()).toEqual({ landings: [] });
        expect((await call('/head', { bearer: board })).status).toBe(200);
        expect((await call('/log?after=0', { bearer: board })).status).toBe(200);
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
