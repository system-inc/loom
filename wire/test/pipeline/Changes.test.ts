// The front door (docs/contracts.md, section 5), against loom-pipeline's Worker and, past its checks, MemoryQueue.

import { describe, expect, it } from 'vitest';
import { checkChangeRequest, handleChanges, MemoryQueue } from '../../source/Changes';
import type { TokenClaims, TokenScope } from '../../source/Token';
import { call, token } from '../Helpers';

const sha = 'a'.repeat(40);
const base = 'b'.repeat(40);
const owner = 'system_adamic_loom_web';

function body(fields: Record<string, unknown> = {}): string {
    return JSON.stringify({ sha: sha, base: base, owner: owner, paths: ['wire/source/Changes.ts'], ...fields });
}

function claimsOf(scope: TokenScope, run = owner): TokenClaims {
    return { run: run, scope: scope, expires: 4102444800 };
}

function submit(queue: MemoryQueue, text: string, scope: TokenScope = 'submit'): Promise<Response> {
    return handleChanges(new Request('https://pipeline.test/changes', { method: 'POST', body: text }), claimsOf(scope), '', queue);
}

describe('submitting a change', function () {
    it('queues a well-formed change from its owner, and hands Queue the checked request in canonical form', async function () {
        const queue = new MemoryQueue();
        const response = await submit(queue, body({ parent: null }));
        expect(response.status).toBe(201);
        const answer = (await response.json()) as { change: string; state: string };
        expect(answer.state).toBe('queued');
        expect(answer.change).toMatch(/^chg_[0-9a-hjkmnp-tv-z]{26}$/);
        const sent = queue.received[0];
        expect(sent?.method).toBe('POST');
        expect(new URL(sent?.url ?? '').pathname).toBe('/changes');
        expect(await sent?.text()).toBe(
            `{"base":"${base}","fixesRed":null,"owner":"${owner}","parent":null,"paths":["wire/source/Changes.ts"],"sha":"${sha}"}`,
        );
    });

    it('refuses a change whose owner isn\'t the token\'s, or whose shape is wrong, before Queue hears of it', async function () {
        const queue = new MemoryQueue();
        const refused: [string, string][] = [
            ['not json', 'the change is not JSON'],
            ['[]', 'the change is a JSON object'],
            [body({ owner: 'someone_else' }), `owner is ${owner}, the submit token's owner`],
            [body({ admin: true }), 'the change has no field admin'],
            [body({ sha: 'A'.repeat(40) }), 'sha is a commit, 40 lowercase hex digits'],
            [body({ base: 'b'.repeat(39) }), 'base is a commit, 40 lowercase hex digits'],
            [body({ base: sha }), 'base is sha itself, so the change has nothing in it'],
            [body({ paths: [] }), 'paths lists 1 to 20000 paths the change touches'],
            [body({ paths: ['/etc/passwd'] }), '"/etc/passwd" is not a repository path'],
            [body({ paths: ['wire/../secrets'] }), '"wire/../secrets" is not a repository path'],
            [body({ paths: ['a', 'a'] }), 'paths names a twice'],
            [body({ parent: 'chg_short' }), 'parent is a change id, chg_ and 26 lowercase base32 characters, or null'],
            [body({ parent: 'chg_' + 'A'.repeat(26) }), 'parent is a change id, chg_ and 26 lowercase base32 characters, or null'],
            [body({ parent: 'chg_' + 'i'.repeat(26) }), 'parent is a change id, chg_ and 26 lowercase base32 characters, or null'],
            [body({ fixesRed: 'main' }), 'fixesRed is the red main commit it fixes, 40 lowercase hex digits, or null'],
        ];
        for (const [text, reason] of refused) {
            const response = await submit(queue, text);
            expect(response.status, text).toBe(400);
            expect(await response.json(), text).toEqual({ error: reason });
        }
        expect(queue.received).toHaveLength(0);
    });

    it("passes a parity run on to the queue, and nothing but true as parity", async function () {
        const queue = new MemoryQueue();
        expect((await submit(queue, body({ parity: true }))).status).toBe(201);
        expect(await queue.received[0]?.text()).toContain('"parity":true');
        expect((await submit(queue, body({ parity: 'yes' }))).status).toBe(400);
        expect((await submit(queue, body({ parity: false }))).status).toBe(201);
        expect(await queue.received[1]?.text()).not.toContain('parity');
    });

    it('takes a change only from a submit token', async function () {
        const queue = new MemoryQueue();
        for (const scope of ['coordinator', 'board'] as const) {
            expect((await submit(queue, body(), scope)).status, scope).toBe(403);
        }
        expect(queue.received).toHaveLength(0);
        expect(checkChangeRequest(body(), owner)).toMatchObject({ owner: owner, parent: null, fixesRed: null });
    });
});

describe('reading changes', function () {
    it('reads a change, its events after a sequence, and the owners\' feed for a coordinator only', async function () {
        const queue = new MemoryQueue();
        const { change } = (await (await submit(queue, body())).json()) as { change: string };
        await queue.emit('change.landed', change, { main: 'c'.repeat(40) });
        const read = await handleChanges(new Request('https://pipeline.test/changes/' + change), claimsOf('board'), change, queue);
        expect(await read.json()).toMatchObject({ record: { change: change, owner: owner, sha: sha }, state: 'queued' });
        const events = await handleChanges(new Request(`https://pipeline.test/changes/${change}/events?after=1`), claimsOf('submit'), `${change}/events`, queue);
        const lines = (await events.text()).trim().split('\n').map(function (line) {
            return JSON.parse(line) as { seq: number; type: string; prev: string };
        });
        expect(lines.map(function (line) {
            return [line.seq, line.type];
        })).toEqual([[2, 'change.landed']]);
        const feed = new Request('https://pipeline.test/changes/events?after=0');
        expect((await handleChanges(feed, claimsOf('submit'), 'events', queue)).status).toBe(403);
        const owners = await handleChanges(feed, claimsOf('coordinator', 'loom'), 'events', queue);
        expect((await owners.text()).trim().split('\n')).toHaveLength(1);
        expect(new URL(queue.received.at(-1)?.url ?? '').search).toBe('?after=0&owners=1');
        for (const [path, operation, status] of [
            ['/changes/chg_nope', 'chg_nope', 404],
            ['/changes/' + change.toUpperCase(), change.toUpperCase().replace('CHG_', 'chg_'), 404],
            [`/changes/${change}/events?after=-1`, `${change}/events`, 400],
        ] as const) {
            expect((await handleChanges(new Request('https://pipeline.test' + path), claimsOf('board'), operation, queue)).status, path).toBe(status);
        }
        const put = new Request(`https://pipeline.test/changes/${change}`, { method: 'PUT', body: '{}' });
        expect((await handleChanges(put, claimsOf('submit'), change, queue)).status).toBe(405);
    });
});

describe("loom-pipeline's Worker", function () {
    it('checks the token with loom-wire\'s verifier, then the shape, then hands the change to the queue', async function () {
        const submitToken = await token(owner, 'submit');
        expect((await call('/changes', { method: 'POST', body: body() })).status).toBe(401);
        for (const scope of ['runner', 'viewer', 'pool', 'publish', 'publish-candidate'] as const) {
            expect((await call('/changes', { method: 'POST', bearer: await token(owner, scope), body: body() })).status, scope).toBe(403);
        }
        expect((await call('/changes', { method: 'POST', bearer: submitToken, body: body({ owner: 'x' }) })).status).toBe(400);
        const accepted = await call('/changes', { method: 'POST', bearer: submitToken, body: body() });
        // Past the front door the Queue object answers. No GitHub credential lives in the Worker, so the change joins
        // the line unchecked, and the bridge posts git's facts for it.
        expect(accepted.status, await accepted.clone().text()).toBe(201);
        expect(await accepted.json()).toMatchObject({ state: 'queued' });
        expect((await call('/runs/r1')).status).toBe(404);
        expect(await (await call('/')).text()).toContain('Loom pipeline');
    });
});
