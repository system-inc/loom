// The front door to main (docs/contracts.md, section 5): an owner submits a change, anyone with a reason reads one,
// and the Mac's bridge reads what owners act on. The Worker checks the token and the body's shape here; the Queue
// object (binding Queue, idFromName('main')) owns everything after that, answering at https://queue/... by the seam
// Loom, Queue and Web agreed on Oct 9. Until Queue is on the wire, a change endpoint answers 503 once its checks
// pass, and the tests run these routes against MemoryQueue, a stand-in answering the same calls.

import { canonicalJson } from './Events';
import { jsonResponse, readBodyText } from './Http';
import type { TokenClaims } from './Token';

export const MaximumChangeBodyBytes = 1024 * 1024;
export const MaximumChangePaths = 20_000;
export const MaximumPathLength = 4096;
export const QueueName = 'main';
// Queue's change ids: chg_ and 26 characters of lowercase base32 without i, l, o or u (Queue, Oct 9).
export const ChangeIdPattern = /^chg_[0-9a-hjkmnp-tv-z]{26}$/;
export const ShaPattern = /^[0-9a-f]{40}$/;
const sequencePattern = /^\d{1,15}$/;

// The section 1 request, after Web's checks.
export interface ChangeRequest {
    sha: string;
    base: string;
    owner: string;
    paths: string[];
    parent: string | null;
    fixesRed: string | null;
}

// The three calls Web makes, as internal requests; Queue's object and the stand-in both answer them.
export interface ChangeQueue {
    fetch(request: Request): Promise<Response>;
}

// A path as git names it: relative, slash separated, no empty, `.` or `..` part, no NUL.
function isRepositoryPath(path: unknown): path is string {
    if (typeof path !== 'string' || path === '' || path.length > MaximumPathLength || path.includes('\0')) {
        return false;
    }
    return path.split('/').every(function (part) {
        return part !== '' && part !== '.' && part !== '..';
    });
}

// The request body is exactly sha, base, owner and paths, plus parent and fixesRed when given, and its owner is the
// token's. What only git can say (the base an ancestor of sha and on main, the paths in base..sha) is Queue's check.
export function checkChangeRequest(body: string, owner: string): ChangeRequest | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the change is not JSON';
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        return 'the change is a JSON object';
    }
    const fields = parsed as Record<string, unknown>;
    const allowed = ['sha', 'base', 'owner', 'paths', 'parent', 'fixesRed'];
    const unknown = Object.keys(fields).filter(function (key) {
        return !allowed.includes(key);
    });
    if (unknown.length > 0) {
        return `the change has no field ${unknown[0]}`;
    }
    if (typeof fields.sha !== 'string' || !ShaPattern.test(fields.sha)) {
        return 'sha is a commit, 40 lowercase hex digits';
    }
    if (typeof fields.base !== 'string' || !ShaPattern.test(fields.base)) {
        return 'base is a commit, 40 lowercase hex digits';
    }
    if (fields.base === fields.sha) {
        return 'base is sha itself, so the change has nothing in it';
    }
    if (fields.owner !== owner) {
        return `owner is ${owner}, the submit token's owner`;
    }
    if (!Array.isArray(fields.paths) || fields.paths.length === 0 || fields.paths.length > MaximumChangePaths) {
        return `paths lists 1 to ${MaximumChangePaths} paths the change touches`;
    }
    const seen = new Set<string>();
    for (const path of fields.paths as unknown[]) {
        if (!isRepositoryPath(path)) {
            return `${JSON.stringify(path)} is not a repository path`;
        }
        if (seen.has(path)) {
            return `paths names ${path} twice`;
        }
        seen.add(path);
    }
    const parent = fields.parent ?? null;
    if (parent !== null && (typeof parent !== 'string' || !ChangeIdPattern.test(parent))) {
        return 'parent is a change id, chg_ and 26 lowercase base32 characters, or null';
    }
    const fixesRed = fields.fixesRed ?? null;
    if (fixesRed !== null && (typeof fixesRed !== 'string' || !ShaPattern.test(fixesRed))) {
        return 'fixesRed is the red main commit it fixes, 40 lowercase hex digits, or null';
    }
    return { sha: fields.sha, base: fields.base, owner: owner, paths: fields.paths as string[], parent: parent, fixesRed: fixesRed };
}

// The Queue object when it is on the wire, else null.
export function queueOf(environment: Env): ChangeQueue | null {
    const namespace = (environment as unknown as { Queue?: DurableObjectNamespace }).Queue;
    return namespace === undefined ? null : namespace.get(namespace.idFromName(QueueName));
}

// The change endpoints, after the Worker has checked the token. `operation` is what follows /changes: '' for a
// submit, 'events' for the owners' feed, or '<change>' or '<change>/events'. Every check runs before the queue is
// asked, so a refusal never depends on whether Queue is on the wire.
export async function handleChanges(
    request: Request,
    claims: TokenClaims,
    operation: string,
    queue: ChangeQueue | null,
): Promise<Response> {
    const inner = await queueRequest(request, claims, operation);
    if (inner instanceof Response) {
        return inner;
    }
    if (queue === null) {
        return jsonResponse(503, { error: "the queue isn't on the wire yet" });
    }
    return queue.fetch(inner);
}

// The request to the Queue object that a change endpoint makes, or the refusal.
async function queueRequest(request: Request, claims: TokenClaims, operation: string): Promise<Request | Response> {
    const after = new URL(request.url).searchParams.get('after');
    if (after !== null && !sequencePattern.test(after)) {
        await request.body?.cancel();
        return jsonResponse(400, { error: 'after is a sequence number, a whole number from 0' });
    }
    const afterQuery = `?after=${after ?? '0'}`;
    if (operation === '') {
        if (request.method !== 'POST') {
            return jsonResponse(405, { error: 'use POST' }, { Allow: 'POST' });
        }
        if (claims.scope !== 'submit') {
            await request.body?.cancel();
            return jsonResponse(403, { error: `a ${claims.scope} token can't submit a change` });
        }
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `a change is at most ${MaximumChangeBodyBytes} bytes` });
        }
        const change = checkChangeRequest(body, claims.run);
        if (typeof change === 'string') {
            return jsonResponse(400, { error: change });
        }
        return new Request('https://queue/changes', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: canonicalJson(change),
        });
    }
    if (request.method !== 'GET') {
        await request.body?.cancel();
        return jsonResponse(405, { error: 'use GET' }, { Allow: 'GET' });
    }
    if (operation === 'events') {
        // Every owner's landed, red and parked, for the bridge that posts them to inboxes.
        if (claims.scope !== 'coordinator') {
            return jsonResponse(403, { error: "only a coordinator token reads every owner's events" });
        }
        return new Request(`https://queue/events${afterQuery}&owners=1`);
    }
    const match = /^(chg_[0-9a-hjkmnp-tv-z]{26})(\/events)?$/.exec(operation);
    if (match === null) {
        return jsonResponse(404, { error: 'no such change endpoint' });
    }
    return new Request(`https://queue/changes/${match[1] ?? ''}${match[2] === undefined ? '' : `/events${afterQuery}`}`);
}

// ---------- The stand-in ----------

interface MemoryChange {
    record: ChangeRequest & { change: string; submittedAt: string };
}

interface MemoryEvent {
    seq: number;
    at: string;
    prev: string;
    type: string;
    subject: { change: string };
    data: Record<string, unknown>;
}

const base32 = '0123456789abcdefghjkmnpqrstvwxyz';

async function sha256Hex(text: string): Promise<string> {
    const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text));
    return Array.from(new Uint8Array(digest), function (byte) {
        return byte.toString(16).padStart(2, '0');
    }).join('');
}

// Queue as an in-memory list, for Web's tests only (the stub contracts.md names): every change it is given is
// queued, in order, with a change.submitted event on a hash chain starting from 64 zeros. It refuses nothing, plans
// nothing and lands nothing; `emit` lets a test append what Queue would.
export class MemoryQueue implements ChangeQueue {
    readonly changes = new Map<string, MemoryChange>();
    readonly log: MemoryEvent[] = [];
    readonly received: Request[] = [];

    async emit(type: string, change: string, data: Record<string, unknown> = {}): Promise<MemoryEvent> {
        const last = this.log.at(-1);
        const event: MemoryEvent = {
            seq: this.log.length + 1,
            at: new Date().toISOString(),
            prev: last === undefined ? '0'.repeat(64) : await sha256Hex(canonicalJson(last)),
            type: type,
            subject: { change: change },
            data: data,
        };
        this.log.push(event);
        return event;
    }

    async fetch(request: Request): Promise<Response> {
        this.received.push(request.clone());
        const url = new URL(request.url);
        const after = Number(url.searchParams.get('after') ?? '0');
        if (url.pathname === '/changes' && request.method === 'POST') {
            const request_ = JSON.parse(await request.text()) as ChangeRequest;
            const random = crypto.getRandomValues(new Uint8Array(26));
            const change = 'chg_' + Array.from(random, function (byte) {
                return base32[byte % 32];
            }).join('');
            this.changes.set(change, { record: { change: change, ...request_, submittedAt: new Date().toISOString() } });
            await this.emit('change.submitted', change);
            return jsonResponse(201, { change: change, state: 'queued' });
        }
        if (url.pathname === '/events') {
            const owners = ['change.landed', 'change.red', 'change.parked'];
            return lines(this.log.filter(function (event) {
                return event.seq > after && owners.includes(event.type);
            }));
        }
        const match = /^\/changes\/([^/]+)(\/events)?$/.exec(url.pathname);
        const held = match === null ? undefined : this.changes.get(match[1] ?? '');
        if (match === null || held === undefined) {
            return jsonResponse(404, { error: 'no such change' });
        }
        if (match[2] !== undefined) {
            return lines(this.log.filter(function (event) {
                return event.seq > after && event.subject.change === held.record.change;
            }));
        }
        return jsonResponse(200, {
            record: held.record,
            state: 'queued',
            future: null,
            units: { planned: 0, passed: 0, failed: 0, void: 0 },
            verdict: null,
            landed: null,
        });
    }
}

function lines(events: MemoryEvent[]): Response {
    return new Response(
        events
            .map(function (event) {
                return canonicalJson(event) + '\n';
            })
            .join(''),
        { headers: { 'Content-Type': 'application/x-ndjson; charset=utf-8', 'Cache-Control': 'no-store' } },
    );
}
