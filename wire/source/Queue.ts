// The queue: one Durable Object (idFromName('main')), the single writer of main's future (Loom's contracts, sections 1, 4
// and 5). Every decision is an event appended to one hash-chained log, and the state is a pure function of that log, so
// replaying it gives the same queue. The Worker has already checked the submit token and the body's shape; this object
// checks the change against git and decides whether it joins the line.

import { DurableObject } from 'cloudflare:workers';
import { jsonResponse, readBodyText } from './Http';

export const GenesisHash = '0'.repeat(64);
export const MaximumStackDepth = 5;
export const MaximumChangeBodyBytes = 1024 * 1024;
export const MaximumEventsPage = 1000;
// The events an owner acts on; the owners' feed carries only these (contracts v1.1, section 5).
export const OwnerEventTypes: readonly EventType[] = ['change.landed', 'change.red', 'change.parked'];

export type EventType =
    | 'change.submitted'
    | 'change.refused'
    | 'block.opened'
    | 'future.built'
    | 'unit.planned'
    | 'unit.placed'
    | 'unit.finished'
    | 'verdict.decided'
    | 'change.landed'
    | 'change.red'
    | 'change.kicked'
    | 'change.restacked'
    | 'change.parked'
    | 'rule.changed';

export type ChangeState = 'queued' | 'building' | 'testing' | 'landed' | 'red' | 'parked' | 'refused';

export interface ChangeRecord {
    change: string;
    sha: string;
    base: string;
    owner: string;
    paths: string[];
    parent: string | null;
    fixesRed: string | null;
    submittedAt: string;
}

export interface QueueEvent {
    seq: number;
    at: string;
    prev: string;
    type: EventType;
    subject: { change?: string; future?: string; unitKey?: string; run?: string };
    data: Record<string, unknown>;
}

// What git says about a submitted change, asked once at submit and kept in the event, so replay never asks again.
export interface GitFacts {
    shaExists: boolean;
    // base is an ancestor of sha.
    baseIsAncestor: boolean;
    // base is main or an ancestor of main.
    baseOnMain: boolean;
    // The paths of the diff base..sha, as git names them.
    diffPaths: string[];
}

export interface History {
    facts(sha: string, base: string): Promise<GitFacts>;
}

export interface ChangeEntry {
    record: ChangeRecord;
    state: ChangeState;
    position: number;
    // The tested tree and its verdict, from the judge (today's gate record, while the judge is a stub).
    future: string | null;
    verdict: Verdict | null;
    landed: string | null;
}

// The verdict record's fields the queue decides on (contracts v1, section 3); the rest rides along in the event.
export interface Verdict {
    future: string;
    run: string;
    status: 'passed' | 'failed' | 'void';
    cause: 'change' | 'mainRed' | 'flake' | 'infra' | null;
    rule: string;
}

// main itself: read it, and move it only by fast-forward. Production is GitHub's ref; a test holds its own.
export interface MainRef {
    read(): Promise<string>;
    fastForward(from: string, to: string): Promise<string | null>;
}

export interface QueueState {
    changes: Map<string, ChangeEntry>;
    // The changes in the line, in submit order: the positions the queue hands out.
    line: string[];
    refused: number;
    head: string;
    seq: number;
}

const shaPattern = /^[0-9a-f]{40}$/;
const changePattern = /^chg_[0-9a-z]{26}$/;
const base32 = '0123456789abcdefghjkmnpqrstvwxyz';

// Canonical JSON: sorted keys, no insignificant whitespace (contracts v1, the preamble).
export function canonical(value: unknown): string {
    if (value === null || typeof value !== 'object') {
        return JSON.stringify(value);
    }
    if (Array.isArray(value)) {
        return '[' + value.map(canonical).join(',') + ']';
    }
    const record = value as Record<string, unknown>;
    return (
        '{' +
        Object.keys(record)
            .filter(function (key) {
                return record[key] !== undefined;
            })
            .sort()
            .map(function (key) {
                return JSON.stringify(key) + ':' + canonical(record[key]);
            })
            .join(',') +
        '}'
    );
}

export async function sha256Text(text: string): Promise<string> {
    const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text));
    return Array.from(new Uint8Array(digest), function (byte) {
        return byte.toString(16).padStart(2, '0');
    }).join('');
}

export function newChangeId(): string {
    const bytes = crypto.getRandomValues(new Uint8Array(26));
    return (
        'chg_' +
        Array.from(bytes, function (byte) {
            return base32[byte % 32];
        }).join('')
    );
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
    return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// The request's shape: section 1's sha, base, owner and paths, and the optional parent and fixesRed. Web checks it
// first; this object checks again, since nothing joins the line on another part's word.
export function checkChangeRequest(body: string): Omit<ChangeRecord, 'change' | 'submittedAt'> | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the change is not JSON';
    }
    if (!isPlainObject(parsed)) {
        return 'the change is a JSON object';
    }
    for (const key of Object.keys(parsed)) {
        if (!['sha', 'base', 'owner', 'paths', 'parent', 'fixesRed'].includes(key)) {
            return `unknown field ${JSON.stringify(key)}; a change is sha, base, owner, paths, and optionally parent and fixesRed`;
        }
    }
    if (typeof parsed.sha !== 'string' || !shaPattern.test(parsed.sha)) {
        return 'sha is 40 lowercase hex digits';
    }
    if (typeof parsed.base !== 'string' || !shaPattern.test(parsed.base)) {
        return 'base is 40 lowercase hex digits';
    }
    if (typeof parsed.owner !== 'string' || !/^[a-z][a-z0-9_]{0,63}$/.test(parsed.owner)) {
        return 'owner is a username: lowercase letters, digits and underscores';
    }
    if (
        !Array.isArray(parsed.paths) ||
        parsed.paths.length === 0 ||
        !parsed.paths.every(function (path) {
            return typeof path === 'string' && path !== '' && !path.startsWith('/') && !path.split('/').includes('..');
        })
    ) {
        return 'paths is a non-empty list of repo-relative paths';
    }
    const parent = parsed.parent ?? null;
    if (parent !== null && (typeof parent !== 'string' || !changePattern.test(parent))) {
        return 'parent is a change id (chg_ and 26 base32 characters) or null';
    }
    const fixesRed = parsed.fixesRed ?? null;
    if (fixesRed !== null && (typeof fixesRed !== 'string' || !shaPattern.test(fixesRed))) {
        return 'fixesRed is a main sha (40 lowercase hex digits) or null';
    }
    return {
        sha: parsed.sha,
        base: parsed.base,
        owner: parsed.owner,
        paths: [...(parsed.paths as string[])].sort(),
        parent: parent,
        fixesRed: fixesRed,
    };
}

function stackDepth(state: QueueState, parent: string | null): number {
    let depth = 0;
    let current = parent;
    while (current !== null) {
        depth++;
        current = state.changes.get(current)?.record.parent ?? null;
    }
    return depth;
}

// Why a change can't join the line, or null when it can. A pure function of the request, git's facts and the
// state before it, so replay reaches the same answer from the same event.
export function refusalOf(
    request: Omit<ChangeRecord, 'change' | 'submittedAt'>,
    facts: GitFacts,
    state: QueueState,
): string | null {
    if (!facts.shaExists) {
        return `sha ${request.sha} is not on GitHub`;
    }
    if (!facts.baseIsAncestor) {
        return `base ${request.base} is not an ancestor of sha ${request.sha}`;
    }
    if (!facts.baseOnMain) {
        return `base ${request.base} is not on main`;
    }
    const diff = new Set(facts.diffPaths);
    const outside = request.paths.filter(function (path) {
        return !diff.has(path);
    });
    if (outside.length > 0) {
        return `paths outside the diff base..sha: ${outside.join(', ')}`;
    }
    if (request.parent !== null && !state.changes.has(request.parent)) {
        return `parent ${request.parent} is not a change this queue holds`;
    }
    if (stackDepth(state, request.parent) + 1 > MaximumStackDepth) {
        return `a stack is at most ${MaximumStackDepth} deep`;
    }
    return null;
}

export function emptyState(): QueueState {
    return { changes: new Map(), line: [], refused: 0, head: GenesisHash, seq: 0 };
}

// Applies one event to the state. Replay is this over the log in order; nothing here reads a clock or asks git.
export function apply(state: QueueState, event: QueueEvent, hash: string): void {
    if (event.type === 'change.submitted') {
        const record = event.data.record as ChangeRecord;
        state.changes.set(record.change, {
            record: record,
            state: 'queued',
            position: state.line.length,
            future: null,
            verdict: null,
            landed: null,
        });
        state.line.push(record.change);
    }
    else if (event.type === 'change.refused') {
        state.refused++;
    }
    else if (event.type === 'verdict.decided') {
        const entry = state.changes.get(event.subject.change ?? '');
        if (entry !== undefined) {
            entry.verdict = event.data.verdict as Verdict;
            entry.future = entry.verdict.future;
            entry.state = entry.verdict.status === 'passed' ? 'testing' : entry.state;
        }
    }
    else if (event.type === 'change.red') {
        const entry = state.changes.get(event.subject.change ?? '');
        if (entry !== undefined) {
            entry.state = 'red';
        }
    }
    else if (event.type === 'change.landed') {
        const entry = state.changes.get(event.subject.change ?? '');
        if (entry !== undefined) {
            entry.state = 'landed';
            entry.landed = event.data.main as string;
            state.line = state.line.filter(function (id) {
                return id !== entry.record.change;
            });
        }
    }
    state.head = hash;
    state.seq = event.seq;
}

// Replays a log from genesis, checking every link of the chain; a broken link is an error, never a state.
export async function replay(events: QueueEvent[]): Promise<QueueState> {
    const state = emptyState();
    for (const event of events) {
        if (event.seq !== state.seq + 1) {
            throw new Error(`event ${event.seq} follows ${state.seq}; the log has a gap`);
        }
        if (event.prev !== state.head) {
            throw new Error(`event ${event.seq}'s prev is ${event.prev}, not the hash of the event before it`);
        }
        apply(state, event, await sha256Text(canonical(event)));
    }
    return state;
}

// Asks GitHub's compare API for the facts a submit needs: base...sha for ancestry and the diff, main...base for
// whether base is on main. A 404 on the commit means it isn't there.
export class GitHubHistory implements History {
    constructor(
        private readonly token: string,
        private readonly repository = 'system-inc/adamic',
    ) {}

    private async compare(from: string, to: string): Promise<{ status: string; files: string[] } | null> {
        const response = await fetch(`https://api.github.com/repos/${this.repository}/compare/${from}...${to}?per_page=300`, {
            headers: {
                Authorization: `Bearer ${this.token}`,
                Accept: 'application/vnd.github+json',
                'User-Agent': 'loom-queue',
            },
        });
        if (response.status === 404) {
            return null;
        }
        if (!response.ok) {
            throw new Error(`GitHub compare ${from}...${to}: ${response.status}`);
        }
        const body = (await response.json()) as { status: string; files?: { filename: string }[] };
        return {
            status: body.status,
            files: (body.files ?? []).map(function (file) {
                return file.filename;
            }),
        };
    }

    async facts(sha: string, base: string): Promise<GitFacts> {
        if (this.token === '') {
            throw new Error('the queue has no GitHub token to read git with');
        }
        const forward = await this.compare(base, sha);
        if (forward === null) {
            return { shaExists: false, baseIsAncestor: false, baseOnMain: false, diffPaths: [] };
        }
        const onMain = await this.compare(base, 'main');
        return {
            shaExists: true,
            baseIsAncestor: forward.status === 'ahead' || forward.status === 'identical',
            baseOnMain: onMain !== null && (onMain.status === 'ahead' || onMain.status === 'identical'),
            diffPaths: forward.files.sort(),
        };
    }
}

// main on GitHub: read refs/heads/main, and move it with update-ref and force false, so GitHub itself refuses
// anything but a fast-forward (Loom 23:4xZ). Returns null on success, or GitHub's reason.
export class GitHubMain implements MainRef {
    constructor(
        private readonly token: string,
        private readonly repository = 'system-inc/adamic',
    ) {}

    private headers(): Record<string, string> {
        return { Authorization: `Bearer ${this.token}`, Accept: 'application/vnd.github+json', 'User-Agent': 'loom-lander' };
    }

    async read(): Promise<string> {
        const response = await fetch(`https://api.github.com/repos/${this.repository}/git/ref/heads/main`, { headers: this.headers() });
        if (!response.ok) {
            throw new Error(`GitHub main ref: ${response.status}`);
        }
        return ((await response.json()) as { object: { sha: string } }).object.sha;
    }

    async fastForward(_from: string, to: string): Promise<string | null> {
        const response = await fetch(`https://api.github.com/repos/${this.repository}/git/refs/heads/main`, {
            method: 'PATCH',
            headers: { ...this.headers(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ sha: to, force: false }),
        });
        if (response.ok) {
            return null;
        }
        return `GitHub refused the fast-forward to ${to}: ${response.status} ${await response.text()}`;
    }
}

// A verdict's shape as the judge (or, until it lands, today's gate) posts it for one change.
export function checkVerdict(body: string): { change: string; verdict: Verdict } | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the verdict is not JSON';
    }
    if (!isPlainObject(parsed) || !isPlainObject(parsed.verdict)) {
        return 'the body is {change, verdict}';
    }
    const verdict = parsed.verdict;
    if (typeof parsed.change !== 'string' || !changePattern.test(parsed.change)) {
        return 'change is a change id';
    }
    if (typeof verdict.future !== 'string' || !shaPattern.test(verdict.future)) {
        return 'verdict.future is the tested tree, 40 lowercase hex digits';
    }
    if (!['passed', 'failed', 'void'].includes(verdict.status as string)) {
        return 'verdict.status is passed, failed or void';
    }
    const cause = verdict.cause ?? null;
    if (cause !== null && !['change', 'mainRed', 'flake', 'infra'].includes(cause as string)) {
        return 'verdict.cause is change, mainRed, flake, infra or null';
    }
    if (typeof verdict.run !== 'string' || verdict.run === '' || typeof verdict.rule !== 'string' || verdict.rule === '') {
        return 'verdict.run and verdict.rule are named';
    }
    return {
        change: parsed.change,
        verdict: { future: verdict.future, run: verdict.run, status: verdict.status as Verdict['status'], cause: cause as Verdict['cause'], rule: verdict.rule },
    };
}

export class Queue extends DurableObject<Env> {
    private readonly sql: SqlStorage;
    private state: QueueState | null = null;
    // The git facts' source. Production asks GitHub; a test sets its own on this object.
    history: History;
    // main's ref. Production is GitHub with the lander's token (set at cutover); a test holds its own.
    main: MainRef;
    // The clock for an event's at, which nothing decides on. A test pins it.
    now: () => string = function () {
        return new Date().toISOString();
    };

    constructor(context: DurableObjectState, environment: Env) {
        super(context, environment);
        this.sql = context.storage.sql;
        this.sql.exec(`
            CREATE TABLE IF NOT EXISTS events (
                seq INTEGER PRIMARY KEY,
                json TEXT NOT NULL,
                hash TEXT NOT NULL,
                type TEXT NOT NULL,
                change TEXT
            );
            CREATE INDEX IF NOT EXISTS eventsByChange ON events (change, seq);
        `);
        const token = (environment as unknown as { GITHUB_TOKEN?: string }).GITHUB_TOKEN ?? '';
        this.history = new GitHubHistory(token);
        const landerToken = (environment as unknown as { GITHUB_LANDER_TOKEN?: string }).GITHUB_LANDER_TOKEN ?? '';
        this.main = new GitHubMain(landerToken);
    }

    override async fetch(request: Request): Promise<Response> {
        const path = new URL(request.url).pathname;
        if (path === '/changes' && request.method === 'POST') {
            return this.submit(request);
        }
        if (path === '/verdicts' && request.method === 'POST') {
            return this.decide(request);
        }
        if (path === '/land' && request.method === 'POST') {
            return this.land(request);
        }
        if (path === '/changes/events' && request.method === 'GET') {
            return this.ownerEvents(request);
        }
        const eventsMatch = /^\/changes\/(chg_[0-9a-z]{26})\/events$/.exec(path);
        if (eventsMatch !== null && request.method === 'GET') {
            return this.changeEvents(request, eventsMatch[1] ?? '');
        }
        const changeMatch = /^\/changes\/(chg_[0-9a-z]{26})$/.exec(path);
        if (changeMatch !== null && request.method === 'GET') {
            return this.readChange(changeMatch[1] ?? '');
        }
        return jsonResponse(404, { error: 'no such queue operation' });
    }

    // The state, replayed from the log once per object lifetime and kept current by apply after that.
    private async current(): Promise<QueueState> {
        if (this.state === null) {
            this.state = await replay(this.log());
        }
        return this.state;
    }

    log(after = 0, limit = Number.MAX_SAFE_INTEGER): QueueEvent[] {
        return this.sql
            .exec<{ json: string }>('SELECT json FROM events WHERE seq > ? ORDER BY seq LIMIT ?', after, limit)
            .toArray()
            .map(function (row) {
                return JSON.parse(row.json) as QueueEvent;
            });
    }

    // The only write: one event, chained to the head, stored and applied in one step.
    private async append(type: EventType, subject: QueueEvent['subject'], data: Record<string, unknown>): Promise<QueueEvent> {
        const state = await this.current();
        const event: QueueEvent = { seq: state.seq + 1, at: this.now(), prev: state.head, type: type, subject: subject, data: data };
        const json = canonical(event);
        const hash = await sha256Text(json);
        this.sql.exec(
            'INSERT INTO events (seq, json, hash, type, change) VALUES (?, ?, ?, ?, ?)',
            event.seq,
            json,
            hash,
            type,
            subject.change ?? null,
        );
        apply(state, event, hash);
        return event;
    }

    private async submit(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        if (body === null) {
            return jsonResponse(413, { reason: `a change is at most ${MaximumChangeBodyBytes} bytes` });
        }
        const checked = checkChangeRequest(body);
        if (typeof checked === 'string') {
            return jsonResponse(422, { reason: checked });
        }
        // Git is asked before the object's turn is taken, since asking waits on the network. When git can't be asked,
        // nothing is decided, so nothing is logged: the submitter tries again.
        let facts: GitFacts;
        try {
            facts = await this.history.facts(checked.sha, checked.base);
        }
        catch (error) {
            return jsonResponse(503, { reason: `git facts are unavailable, try again: ${(error as Error).message}` });
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const reason = refusalOf(checked, facts, state);
            if (reason !== null) {
                await this.append('change.refused', {}, { request: checked, facts: facts, reason: reason });
                return jsonResponse(422, { reason: reason });
            }
            const record: ChangeRecord = { change: newChangeId(), submittedAt: this.now(), ...checked };
            await this.append('change.submitted', { change: record.change }, { record: record, facts: facts });
            const entry = state.changes.get(record.change);
            return jsonResponse(201, { change: record.change, state: entry?.state ?? 'queued', position: entry?.position ?? 0 });
        });
    }

    // The judge's verdict for one change's future. A red caused by the change is the owner's (change.red); any other
    // red and every void is recorded and goes nowhere, since only the change's own red reaches its owner.
    private async decide(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: 'a verdict is too large' });
        }
        const checked = checkVerdict(body);
        if (typeof checked === 'string') {
            return jsonResponse(400, { error: checked });
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const entry = (await this.current()).changes.get(checked.change);
            if (entry === undefined) {
                return jsonResponse(404, { error: `no change ${checked.change}` });
            }
            if (entry.state === 'landed' || entry.state === 'red') {
                return jsonResponse(409, { error: `change ${checked.change} is already ${entry.state}` });
            }
            await this.append('verdict.decided', { change: checked.change, future: checked.verdict.future, run: checked.verdict.run }, { verdict: checked.verdict });
            if (checked.verdict.status === 'failed' && checked.verdict.cause === 'change') {
                await this.append('change.red', { change: checked.change, future: checked.verdict.future }, { verdict: checked.verdict });
            }
            return jsonResponse(200, { change: checked.change, state: entry.state });
        });
    }

    // The lander: main moves only to a future whose verdict passed, and only by a fast-forward from main as it is.
    private async land(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        let change = '';
        try {
            change = (JSON.parse(body ?? '') as { change?: string }).change ?? '';
        }
        catch {
            return jsonResponse(400, { error: 'the body is {change}' });
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const entry = (await this.current()).changes.get(change);
            if (entry === undefined) {
                return jsonResponse(404, { error: `no change ${change}` });
            }
            if (entry.state === 'landed') {
                return jsonResponse(200, { change: change, state: 'landed', landed: entry.landed });
            }
            if (entry.verdict === null || entry.verdict.status !== 'passed' || entry.future === null) {
                return jsonResponse(409, { error: `change ${change} has no passed verdict for a tested future` });
            }
            const from = await this.main.read();
            const facts = await this.history.facts(entry.future, from);
            if (!facts.baseIsAncestor) {
                return jsonResponse(409, { error: `future ${entry.future} does not descend from main ${from}; it needs a new future` });
            }
            const refused = await this.main.fastForward(from, entry.future);
            if (refused !== null) {
                return jsonResponse(409, { error: refused });
            }
            await this.append('change.landed', { change: change, future: entry.future }, { main: entry.future, from: from });
            return jsonResponse(200, { change: change, state: 'landed', landed: entry.future });
        });
    }

    private async readChange(change: string): Promise<Response> {
        const state = await this.current();
        const entry = state.changes.get(change);
        if (entry === undefined) {
            return jsonResponse(404, { error: `no change ${change}` });
        }
        return jsonResponse(200, {
            record: entry.record,
            state: entry.state,
            position: entry.position,
            future: entry.future,
            units: { planned: 0, passed: 0, failed: 0, void: 0 },
            verdict: entry.verdict,
            landed: entry.landed,
        });
    }

    private afterOf(request: Request): number {
        const after = Number(new URL(request.url).searchParams.get('after') ?? '0');
        return Number.isSafeInteger(after) && after >= 0 ? after : 0;
    }

    private eventLines(events: QueueEvent[]): Response {
        return new Response(
            events
                .map(function (event) {
                    return canonical(event) + '\n';
                })
                .join(''),
            { headers: { 'Content-Type': 'application/x-ndjson; charset=utf-8', 'Cache-Control': 'no-store' } },
        );
    }

    private async changeEvents(request: Request, change: string): Promise<Response> {
        const state = await this.current();
        if (!state.changes.has(change)) {
            return jsonResponse(404, { error: `no change ${change}` });
        }
        const rows = this.sql
            .exec<{ json: string }>(
                'SELECT json FROM events WHERE change = ? AND seq > ? ORDER BY seq LIMIT ?',
                change,
                this.afterOf(request),
                MaximumEventsPage,
            )
            .toArray();
        return this.eventLines(
            rows.map(function (row) {
                return JSON.parse(row.json) as QueueEvent;
            }),
        );
    }

    private ownerEvents(request: Request): Response {
        const placeholders = OwnerEventTypes.map(function () {
            return '?';
        }).join(', ');
        const rows = this.sql
            .exec<{ json: string }>(
                `SELECT json FROM events WHERE type IN (${placeholders}) AND seq > ? ORDER BY seq LIMIT ?`,
                ...OwnerEventTypes,
                this.afterOf(request),
                MaximumEventsPage,
            )
            .toArray();
        return this.eventLines(
            rows.map(function (row) {
                return JSON.parse(row.json) as QueueEvent;
            }),
        );
    }
}
