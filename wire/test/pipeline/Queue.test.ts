import { env } from 'cloudflare:workers';
import { runInDurableObject } from 'cloudflare:test';
import { describe, expect, it } from 'vitest';
import { canonical, GenesisHash, MaximumStackDepth, replay, sha256Text, type GitFacts, type Queue, type QueueEvent } from '../../source/Queue';

const main = 'a'.repeat(40);

function sha(seed: number): string {
    return seed.toString(16).padStart(40, '0');
}

// A stand-in for GitHub: every sha exists, descends from main, and touches the paths it's given, unless told otherwise.
function facts(overrides: Partial<GitFacts> = {}, diffPaths = ['internal/lower/a.go', 'internal/lower/a_test.go']): GitFacts {
    return { shaExists: true, baseIsAncestor: true, baseOnMain: true, diffPaths: diffPaths, ...overrides };
}

// A fresh queue object per test, with git answered from a table keyed by sha and a pinned clock.
// main as a test holds it: a sha, moved only forward from the sha the lander read, or refused with a reason.
class TestMain {
    sha = main;
    refuse: string | null = null;
    async read(): Promise<string> {
        return this.sha;
    }
    async fastForward(from: string, to: string): Promise<string | null> {
        if (this.refuse !== null) {
            return this.refuse;
        }
        if (from !== this.sha) {
            return `main is ${this.sha}, not ${from}`;
        }
        this.sha = to;
        return null;
    }
}

async function freshQueue(answers: Record<string, GitFacts> = {}, mainRef: TestMain = new TestMain()): Promise<DurableObjectStub<Queue>> {
    // The pipeline's binding (pipeline.jsonc); the generated Env type knows only loom-wire's.
    const namespace = (env as unknown as { Queue: DurableObjectNamespace<Queue> }).Queue;
    const stub = namespace.get(namespace.idFromName('queue-' + crypto.randomUUID()));
    await runInDurableObject(stub, function (instance: Queue) {
        instance.main = mainRef;
        instance.history = {
            async facts(asked: string): Promise<GitFacts> {
                return answers[asked] ?? facts();
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
        ).toEqual([1, 2, 3]);
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
        expect(lines).toHaveLength(1);
        expect(JSON.parse(lines[0] ?? '')).toMatchObject({ seq: 1, type: 'change.submitted', subject: { change: id } });
        expect((await (await queue.fetch(`https://queue/changes/${id}/events?after=1`)).text()).trim()).toBe('');
        expect((await queue.fetch(`https://queue/changes/chg_${'q'.repeat(26)}`)).status).toBe(404);
        // The owners' feed carries only landed, red and parked; a submit is none of them.
        expect((await (await queue.fetch('https://queue/changes/events')).text()).trim()).toBe('');
    });

    it('lands a change only on a passed verdict, by fast-forward to exactly its future, and says so once', async function () {
        const held = new TestMain();
        const queue = await freshQueue({}, held);
        const id = ((await (await submit(queue, change(1))).json()) as { change: string }).change;
        const land = function (): Promise<Response> {
            return queue.fetch('https://queue/land', { method: 'POST', body: JSON.stringify({ change: id }) });
        };
        expect((await land()).status).toBe(409);
        const verdict = { future: sha(1), run: 'gate-logs/x/fast', status: 'passed', cause: null, rule: 'todays-gate-v0' };
        expect((await queue.fetch('https://queue/verdicts', { method: 'POST', body: JSON.stringify({ change: id, verdict: verdict }) })).status).toBe(200);
        const landed = await land();
        expect(landed.status, await landed.clone().text()).toBe(200);
        expect(await landed.json()).toEqual({ change: id, state: 'landed', landed: sha(1) });
        expect(held.sha).toBe(sha(1));
        expect(await (await queue.fetch(`https://queue/changes/${id}`)).json()).toMatchObject({ state: 'landed', landed: sha(1), future: sha(1) });
        // A second land is an answer, not a second move.
        expect((await land()).status).toBe(200);
        const types = (await logOf(queue)).map(function (event) {
            return event.type;
        });
        expect(types).toEqual(['change.submitted', 'verdict.decided', 'change.landed']);
        const feed = (await (await queue.fetch('https://queue/changes/events')).text()).trim().split('\n');
        expect(feed).toHaveLength(1);
        expect(JSON.parse(feed[0] ?? '')).toMatchObject({ type: 'change.landed', subject: { change: id }, data: { main: sha(1), from: main } });
        expect((await replay(await logOf(queue))).changes.get(id)?.state).toBe('landed');
    });

    it('refuses to land a future main has moved past, or one GitHub will not fast-forward, and moves nothing', async function () {
        const held = new TestMain();
        const queue = await freshQueue({ [sha(2)]: facts({ baseIsAncestor: false }) }, held);
        const ids: string[] = [];
        for (const seed of [2, 3]) {
            const id = ((await (await submit(queue, change(seed))).json().catch(function () {
                return {};
            })) as { change?: string }).change;
            ids.push(id ?? '');
        }
        // sha(2) was refused at submit (its base isn't its ancestor); land sha(3) after GitHub refuses.
        expect(ids[0]).toBe('');
        const id = ids[1] ?? '';
        await queue.fetch('https://queue/verdicts', {
            method: 'POST',
            body: JSON.stringify({ change: id, verdict: { future: sha(3), run: 'r', status: 'passed', cause: null, rule: 'todays-gate-v0' } }),
        });
        held.refuse = 'Update is not a fast forward';
        const refused = await queue.fetch('https://queue/land', { method: 'POST', body: JSON.stringify({ change: id }) });
        expect(refused.status).toBe(409);
        expect(((await refused.json()) as { error: string }).error).toContain('not a fast forward');
        expect(held.sha).toBe(main);
        expect(
            (await logOf(queue)).map(function (event) {
                return event.type;
            }),
        ).not.toContain('change.landed');
    });

    it('sends a red caused by the change to its owner, and keeps every other red and void off the feed', async function () {
        const queue = await freshQueue();
        const ids: string[] = [];
        for (const seed of [1, 2, 3]) {
            ids.push(((await (await submit(queue, change(seed))).json()) as { change: string }).change);
        }
        const post = function (id: string, status: string, cause: string | null): Promise<Response> {
            return queue.fetch('https://queue/verdicts', {
                method: 'POST',
                body: JSON.stringify({ change: id, verdict: { future: sha(9), run: 'r', status: status, cause: cause, rule: 'todays-gate-v0' } }),
            });
        };
        expect((await post(ids[0] ?? '', 'failed', 'change')).status).toBe(200);
        expect((await post(ids[1] ?? '', 'failed', 'mainRed')).status).toBe(200);
        expect((await post(ids[2] ?? '', 'void', 'infra')).status).toBe(200);
        const feed = (await (await queue.fetch('https://queue/changes/events')).text()).trim().split('\n');
        expect(feed).toHaveLength(1);
        expect(JSON.parse(feed[0] ?? '')).toMatchObject({ type: 'change.red', subject: { change: ids[0] } });
        expect(await (await queue.fetch(`https://queue/changes/${ids[0]}`)).json()).toMatchObject({ state: 'red' });
        expect((await post(ids[0] ?? '', 'passed', null)).status).toBe(409);
    });
});
