// The queue: one Durable Object (idFromName('main')), the single writer of main's future (Loom's contracts, sections 1, 4
// and 5, and v1.1's seams). Every decision is an event appended to one hash-chained log, and the state is a pure
// function of that log, so replaying it gives the same queue. The Worker has already checked the token and the body's
// shape; this object checks the change against git and decides whether it joins the line, takes the planner's plan
// and the judge's verdicts for each future, and says which changes may land. It never moves main itself: a process on
// workshop pulls the landing orders, pushes with Kirk's fast-forward-only script, and reports back (Loom's four-hour
// cut, Oct 9), so no credential that can move main lives here.

import { DurableObject } from 'cloudflare:workers';
import { jsonResponse, readBodyText } from './Http';

export const GenesisHash = '0'.repeat(64);
export const MaximumStackDepth = 5;
export const MaximumChangeBodyBytes = 1024 * 1024;
// A plan or a judge's batch carries every unit of a future, each verdict with its tests.
export const MaximumFutureBodyBytes = 32 * 1024 * 1024;
export const MaximumEventsPage = 1000;
export const UnitKeyVersion = 'loom-unit-v1';
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

export type VerdictStatus = 'passed' | 'failed' | 'void';
export type VerdictCause = 'change' | 'mainRed' | 'flake' | 'infra' | null;

// A whole verdict on an unplanned future: today's gate's record through the adapter (#jtdwm5n), standing in for the
// planner and judge until they land. The rest of what the gate posts rides along in the event.
export interface Verdict {
    future: string;
    run: string;
    status: VerdictStatus;
    cause: VerdictCause;
    rule: string;
}

// One unit's verdict record (contracts v1, section 3), as the judge posts it. The fields the queue decides on are
// typed; the rest (attempts, tests, outputs, decidedAt) rides along and is served back whole by the verdict index.
export interface UnitVerdict {
    unitKey: string;
    change: string;
    future: string;
    run: string;
    status: VerdictStatus;
    cause: VerdictCause;
    [field: string]: unknown;
}

// A run's decision, the judge's Green (judge/judge.go): red names the units that failed as the change's, excused the
// ones that failed as main's red, and problems why a run that is neither green nor red is void.
export interface Decision {
    status: 'green' | 'red' | 'void';
    red: string[];
    excused: string[];
    problems: string[];
}

// One unit the planner planned for a future, with its latest verdict at that future's tree (for a reuse, the
// passed verdict the index held when the plan came in).
export interface UnitEntry {
    unitKey: string;
    name: string;
    decision: 'reuse' | 'run';
    verdict: UnitVerdict | null;
}

// A tree to test and the changes in it, in order, keyed by the tree's sha. Slice 1's futures are one change each,
// tree = the change's sha.
export interface FutureEntry {
    tree: string;
    base: string;
    changes: string[];
    // null until the planner posts its units.
    units: Map<string, UnitEntry> | null;
    // Today's gate's whole verdict, on an unplanned future only.
    whole: Verdict | null;
    // The run that decided the future and how: a judge's batch, today's gate's whole verdict, or a plan that reused
    // every unit. A void lets the next run decide it.
    decided: { run: string; status: Decision['status'] } | null;
}

export interface ChangeEntry {
    record: ChangeRecord;
    state: ChangeState;
    position: number;
    // The tree the change is tested in, its future's key: what main moves to when it lands.
    future: string | null;
    // The whole verdict, or the red's decision: what GET /changes/<change> shows.
    verdict: Record<string, unknown> | null;
    landed: string | null;
}

export interface QueueState {
    changes: Map<string, ChangeEntry>;
    futures: Map<string, FutureEntry>;
    // The verdict index (contracts v1.1): each unit key's latest decided verdict record, a projection of the log.
    verdicts: Map<string, { seq: number; record: UnitVerdict }>;
    // The changes in the line, in submit order: the positions the queue hands out.
    line: string[];
    refused: number;
    head: string;
    seq: number;
}

const shaPattern = /^[0-9a-f]{40}$/;
const hashPattern = /^[0-9a-f]{64}$/;
const changePattern = /^chg_[0-9a-z]{26}$/;
const base32 = '0123456789abcdefghjkmnpqrstvwxyz';
const statuses: readonly string[] = ['passed', 'failed', 'void'];
const causes: readonly string[] = ['change', 'mainRed', 'flake', 'infra'];
// A change in one of these is finished: nothing more is planned, decided or landed for it.
const finishedStates: readonly ChangeState[] = ['landed', 'red', 'parked', 'refused'];

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

// The unit key (contracts v1, section 2), computed the way the planner computes it (planner/unitkey.go).
export function unitKeyOf(keyParts: unknown): Promise<string> {
    return sha256Text(UnitKeyVersion + '\n' + canonical(keyParts));
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

function parseJson(body: string): unknown {
    try {
        return JSON.parse(body) as unknown;
    }
    catch {
        return undefined;
    }
}

function sortedEqual(left: readonly string[], right: readonly string[]): boolean {
    return canonical([...left].sort()) === canonical([...right].sort());
}

// The request's shape: section 1's sha, base, owner and paths, and the optional parent and fixesRed. Web checks it
// first; this object checks again, since nothing joins the line on another part's word.
export function checkChangeRequest(body: string): Omit<ChangeRecord, 'change' | 'submittedAt'> | string {
    const parsed = parseJson(body);
    if (parsed === undefined) {
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

function isLive(entry: ChangeEntry | undefined): boolean {
    return entry !== undefined && !finishedStates.includes(entry.state);
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
    // A future is keyed by its tree, so one sha is in the line once.
    for (const change of state.futures.get(request.sha)?.changes ?? []) {
        if (isLive(state.changes.get(change))) {
            return `sha ${request.sha} is already in the line as ${change}`;
        }
    }
    return null;
}

// The judge's Green (judge/judge.go), so the queue never lands on another part's word: green only when the verdicts
// cover exactly the planned keys once each, every one passed or failed as main's red. A failure the change caused is
// red ahead of anything missing; any other gap or void is void.
export function judgeGreen(plan: readonly string[], verdicts: readonly UnitVerdict[]): Decision {
    const decision: Decision = { status: 'void', red: [], excused: [], problems: [] };
    const planned = new Set(plan);
    const seen = new Set<string>();
    for (const verdict of verdicts) {
        if (!planned.has(verdict.unitKey)) {
            decision.problems.push(`unit ${verdict.unitKey} isn't in the plan`);
            continue;
        }
        if (seen.has(verdict.unitKey)) {
            decision.problems.push(`unit ${verdict.unitKey} has more than one verdict`);
            continue;
        }
        seen.add(verdict.unitKey);
        if (verdict.status === 'passed') {
            continue;
        }
        if (verdict.status === 'failed' && verdict.cause === 'change') {
            decision.red.push(verdict.unitKey);
        }
        else if (verdict.status === 'failed' && verdict.cause === 'mainRed') {
            decision.excused.push(verdict.unitKey);
        }
        else {
            decision.problems.push(`unit ${verdict.unitKey} is ${verdict.status} with cause ${String(verdict.cause)}`);
        }
    }
    for (const key of [...planned].sort()) {
        if (!seen.has(key)) {
            decision.problems.push(`unit ${key} has no verdict`);
        }
    }
    decision.status = decision.red.length > 0 ? 'red' : decision.problems.length > 0 ? 'void' : 'green';
    return decision;
}

// Whether a future may land: decided green, and its verdicts at this exact tree still say so by the judge's rule,
// every planned unit's (reused ones included, as the judge posts them). An unplanned future needs today's gate's pass.
export function futureLandable(future: FutureEntry | undefined): boolean {
    if (future === undefined || future.decided === null || future.decided.status !== 'green') {
        return false;
    }
    if (future.units === null) {
        return future.whole !== null && future.whole.status === 'passed' && future.whole.future === future.tree;
    }
    if (future.units.size === 0) {
        return false;
    }
    const verdicts: UnitVerdict[] = [];
    for (const unit of future.units.values()) {
        if (unit.verdict !== null && unit.verdict.future === future.tree) {
            verdicts.push(unit.verdict);
        }
    }
    return judgeGreen([...future.units.keys()], verdicts).status === 'green';
}

// A change's GET units: the plan's size and its latest verdicts by status.
export function unitCounts(future: FutureEntry | undefined): { planned: number; passed: number; failed: number; void: number } {
    const counts = { planned: 0, passed: 0, failed: 0, void: 0 };
    for (const unit of future?.units?.values() ?? []) {
        counts.planned++;
        if (unit.verdict !== null) {
            counts[unit.verdict.status]++;
        }
    }
    return counts;
}

export function emptyState(): QueueState {
    return { changes: new Map(), futures: new Map(), verdicts: new Map(), line: [], refused: 0, head: GenesisHash, seq: 0 };
}

function decisionOf(status: VerdictStatus): Decision['status'] {
    return status === 'passed' ? 'green' : status === 'failed' ? 'red' : 'void';
}

// Applies one event to the state. Replay is this over the log in order; nothing here reads a clock or asks git.
export function apply(state: QueueState, event: QueueEvent, hash: string): void {
    const entry = state.changes.get(event.subject.change ?? '');
    if (event.type === 'change.submitted') {
        const record = event.data.record as ChangeRecord;
        state.changes.set(record.change, { record: record, state: 'queued', position: state.line.length, future: null, verdict: null, landed: null });
        state.line.push(record.change);
    }
    else if (event.type === 'change.refused') {
        state.refused++;
    }
    else if (event.type === 'future.built') {
        const tree = event.subject.future ?? '';
        const changes = event.data.changes as string[];
        state.futures.set(tree, { tree: tree, base: event.data.base as string, changes: changes, units: null, whole: null, decided: null });
        for (const change of changes) {
            const member = state.changes.get(change);
            if (member !== undefined) {
                member.future = tree;
            }
        }
    }
    else if (event.type === 'unit.planned') {
        const future = state.futures.get(event.subject.future ?? '');
        if (future !== undefined) {
            future.units ??= new Map();
            const reused = (event.data.verdict ?? null) as UnitVerdict | null;
            future.units.set(event.subject.unitKey ?? '', {
                unitKey: event.subject.unitKey ?? '',
                name: event.data.name as string,
                decision: event.data.decision as UnitEntry['decision'],
                // The same key is the same verdict, so a reused one decides this tree too.
                verdict: reused === null ? null : { ...reused, future: future.tree },
            });
            for (const change of future.changes) {
                const member = state.changes.get(change);
                if (member !== undefined && member.state === 'queued') {
                    member.state = 'testing';
                }
            }
        }
    }
    else if (event.type === 'verdict.decided') {
        const future = state.futures.get(event.subject.future ?? '');
        if (event.subject.unitKey !== undefined) {
            // One unit's record: the index's latest for its key, and the unit's verdict at this tree.
            const record = event.data.verdict as UnitVerdict;
            state.verdicts.set(record.unitKey, { seq: event.seq, record: record });
            const unit = future?.units?.get(record.unitKey);
            if (unit !== undefined && record.future === future?.tree) {
                unit.verdict = record;
            }
        }
        else if (event.data.decision !== undefined && future !== undefined) {
            future.decided = { run: event.subject.run ?? '', status: (event.data.decision as Decision).status };
        }
        else if (event.data.verdict !== undefined && future !== undefined) {
            const verdict = event.data.verdict as Verdict;
            future.whole = verdict;
            future.decided = { run: verdict.run, status: decisionOf(verdict.status) };
            if (entry !== undefined) {
                entry.verdict = verdict as unknown as Record<string, unknown>;
                entry.state = entry.state === 'queued' ? 'testing' : entry.state;
            }
        }
    }
    else if (event.type === 'change.red' && entry !== undefined) {
        entry.state = 'red';
        entry.verdict = (event.data.decision ?? event.data.verdict ?? null) as Record<string, unknown> | null;
    }
    else if (event.type === 'change.parked' && entry !== undefined) {
        entry.state = 'parked';
    }
    else if (event.type === 'change.landed' && entry !== undefined) {
        entry.state = 'landed';
        entry.landed = event.data.main as string;
        state.line = state.line.filter(function (id) {
            return id !== entry.record.change;
        });
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
// whether base is on main. A 404 on the commit means it isn't there. The token only reads.
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

// A whole verdict's shape as today's gate's adapter posts it for one change: {change, verdict}.
export function checkVerdict(body: string): { change: string; verdict: Verdict } | string {
    const parsed = parseJson(body);
    if (!isPlainObject(parsed) || !isPlainObject(parsed.verdict)) {
        return 'the body is {change, verdict}';
    }
    const verdict = parsed.verdict;
    if (typeof parsed.change !== 'string' || !changePattern.test(parsed.change)) {
        return 'change is a change id';
    }
    if (verdict.unitKey !== undefined) {
        return "a unit's verdict comes in the judge's batch, POST /futures/<tree>/verdicts";
    }
    if (typeof verdict.future !== 'string' || !shaPattern.test(verdict.future)) {
        return 'verdict.future is the tested tree, 40 lowercase hex digits';
    }
    if (!statuses.includes(verdict.status as string)) {
        return 'verdict.status is passed, failed or void';
    }
    const cause = verdict.cause ?? null;
    if (cause !== null && !causes.includes(cause as string)) {
        return 'verdict.cause is change, mainRed, flake, infra or null';
    }
    if (typeof verdict.run !== 'string' || verdict.run === '' || typeof verdict.rule !== 'string' || verdict.rule === '') {
        return 'verdict.run and verdict.rule are named';
    }
    return {
        change: parsed.change,
        verdict: { future: verdict.future, run: verdict.run, status: verdict.status as VerdictStatus, cause: cause as VerdictCause, rule: verdict.rule },
    };
}

export interface PlannedUnit {
    name: string;
    unitKey: string;
    keyParts: Record<string, unknown>;
    decision: 'reuse' | 'run';
    reason: string;
    reused: string | null;
}

// The planner's body for POST /futures/<tree>/plan: the unit list, each unit once, its key computed from its parts.
export async function checkPlan(body: string): Promise<PlannedUnit[] | string> {
    const parsed = parseJson(body);
    if (!Array.isArray(parsed) || parsed.length === 0) {
        return 'the plan is a non-empty JSON list of units';
    }
    const units: PlannedUnit[] = [];
    const names = new Set<string>();
    const keys = new Set<string>();
    for (const [index, item] of parsed.entries()) {
        if (!isPlainObject(item)) {
            return `unit ${index} is a JSON object`;
        }
        if (typeof item.name !== 'string' || item.name === '') {
            return `unit ${index} has a name`;
        }
        if (typeof item.unitKey !== 'string' || !hashPattern.test(item.unitKey)) {
            return `unit ${item.name}'s unitKey is 64 lowercase hex digits`;
        }
        if (!isPlainObject(item.keyParts)) {
            return `unit ${item.name}'s keyParts is a JSON object`;
        }
        if (item.decision !== 'reuse' && item.decision !== 'run') {
            return `unit ${item.name}'s decision is reuse or run`;
        }
        if (item.reused !== undefined && (typeof item.reused !== 'string' || item.reused === '')) {
            return `unit ${item.name}'s reused names the run whose verdict it reuses`;
        }
        if (names.has(item.name) || keys.has(item.unitKey)) {
            return `unit ${item.name} is planned twice`;
        }
        const computed = await unitKeyOf(item.keyParts);
        if (computed !== item.unitKey) {
            return `unit ${item.name}'s unitKey ${item.unitKey} is not its keyParts' key ${computed}`;
        }
        names.add(item.name);
        keys.add(item.unitKey);
        units.push({
            name: item.name,
            unitKey: item.unitKey,
            keyParts: item.keyParts,
            decision: item.decision,
            reason: typeof item.reason === 'string' ? item.reason : '',
            reused: typeof item.reused === 'string' ? item.reused : null,
        });
    }
    return units;
}

export interface JudgeBatch {
    change: string;
    run: string;
    rule: string;
    plan: string[];
    verdicts: UnitVerdict[];
    decision: Decision;
    kicks: Record<string, unknown>;
    // The flaky tests the judge quarantined in this run, logged with its decision.
    quarantine: unknown[];
}

function isKeyList(value: unknown): value is string[] {
    return (
        Array.isArray(value) &&
        value.every(function (key) {
            return typeof key === 'string' && hashPattern.test(key);
        })
    );
}

// The judge's body for POST /futures/<tree>/verdicts: one run's records for every unit it ran, and its decision.
export function checkJudgeBatch(body: string, tree: string): JudgeBatch | string {
    const parsed = parseJson(body);
    if (!isPlainObject(parsed)) {
        return 'the body is {change, run, rule, plan, verdicts, decision}';
    }
    if (typeof parsed.change !== 'string' || !changePattern.test(parsed.change)) {
        return 'change is a change id';
    }
    if (typeof parsed.run !== 'string' || parsed.run === '' || typeof parsed.rule !== 'string' || parsed.rule === '') {
        return 'run and rule are named';
    }
    if (!isKeyList(parsed.plan) || new Set(parsed.plan).size !== parsed.plan.length) {
        return 'plan is a list of unit keys, each once';
    }
    const decision = parsed.decision;
    if (
        !isPlainObject(decision) ||
        !['green', 'red', 'void'].includes(decision.status as string) ||
        !isKeyList(decision.red) ||
        !isKeyList(decision.excused) ||
        !Array.isArray(decision.problems)
    ) {
        return 'decision is {status: green, red or void, red, excused, problems}';
    }
    const quarantine = parsed.quarantine ?? [];
    if (!Array.isArray(quarantine)) {
        return 'quarantine is a list of test outcomes';
    }
    const kicks = decision.kicks ?? {};
    if (!isPlainObject(kicks)) {
        return 'decision.kicks maps a red unit key to its kick';
    }
    if (!Array.isArray(parsed.verdicts)) {
        return 'verdicts is a list of verdict records';
    }
    const verdicts: UnitVerdict[] = [];
    for (const [index, record] of parsed.verdicts.entries()) {
        if (!isPlainObject(record) || typeof record.unitKey !== 'string' || !hashPattern.test(record.unitKey)) {
            return `verdict ${index} is a record with a unitKey`;
        }
        if (record.future !== tree || record.change !== parsed.change || record.run !== parsed.run) {
            return `verdict ${record.unitKey} is for future ${String(record.future)}, change ${String(record.change)} and run ${String(record.run)}, not this batch's`;
        }
        if (!statuses.includes(record.status as string) || !(record.cause === null || causes.includes(record.cause as string))) {
            return `verdict ${record.unitKey}'s status is passed, failed or void, and its cause one of the four or null`;
        }
        verdicts.push(record as UnitVerdict);
    }
    return {
        change: parsed.change,
        run: parsed.run,
        rule: parsed.rule,
        plan: parsed.plan,
        verdicts: verdicts,
        decision: { status: decision.status as Decision['status'], red: decision.red, excused: decision.excused, problems: decision.problems as string[] },
        kicks: kicks,
        quarantine: quarantine,
    };
}

export class Queue extends DurableObject<Env> {
    private readonly sql: SqlStorage;
    private state: QueueState | null = null;
    // The git facts' source. Production asks GitHub; a test sets its own on this object.
    history: History;
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
    }

    override async fetch(request: Request): Promise<Response> {
        const url = new URL(request.url);
        const path = url.pathname;
        const method = request.method;
        if (path === '/changes' && method === 'POST') {
            return this.submit(request);
        }
        if (path === '/verdicts' && method === 'POST') {
            return this.decideWhole(request);
        }
        if (path === '/events' && method === 'GET') {
            return url.searchParams.get('owners') === '1' ? this.ownerEvents(request) : this.events(request);
        }
        if (path === '/futures' && method === 'GET') {
            return this.unplannedFutures(url);
        }
        if (path === '/landings' && method === 'GET') {
            return this.landings();
        }
        const futureMatch = /^\/futures\/([0-9a-f]{40})\/(plan|verdicts)$/.exec(path);
        if (futureMatch !== null && method === 'POST') {
            return futureMatch[2] === 'plan' ? this.plan(request, futureMatch[1] ?? '') : this.decideBatch(request, futureMatch[1] ?? '');
        }
        const verdictMatch = /^\/verdicts\/([0-9a-f]{64})$/.exec(path);
        if (verdictMatch !== null && method === 'GET') {
            return this.readVerdict(verdictMatch[1] ?? '');
        }
        const landingMatch = /^\/landings\/(chg_[0-9a-z]{26})$/.exec(path);
        if (landingMatch !== null && method === 'POST') {
            return this.reportLanding(request, landingMatch[1] ?? '');
        }
        const eventsMatch = /^\/changes\/(chg_[0-9a-z]{26})\/events$/.exec(path);
        if (eventsMatch !== null && method === 'GET') {
            return this.changeEvents(request, eventsMatch[1] ?? '');
        }
        const changeMatch = /^\/changes\/(chg_[0-9a-z]{26})$/.exec(path);
        if (changeMatch !== null && method === 'GET') {
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
            // Slice 1: the change is its own future, its tree its sha. Speculation (#j3t4qbg) builds main+A+B instead.
            await this.append('future.built', { change: record.change, future: record.sha }, { base: record.base, changes: [record.change] });
            const entry = state.changes.get(record.change);
            return jsonResponse(201, { change: record.change, state: entry?.state ?? 'queued', position: entry?.position ?? 0 });
        });
    }

    // The future a change is tested in, while every change in it is still on its way; or why not.
    private liveFuture(state: QueueState, tree: string): FutureEntry | Response {
        const future = state.futures.get(tree);
        if (future === undefined) {
            return jsonResponse(404, { error: `no future ${tree}` });
        }
        const finished = future.changes.find(function (change) {
            return !isLive(state.changes.get(change));
        });
        if (finished !== undefined) {
            return jsonResponse(409, { error: `change ${finished} in future ${tree} is already ${state.changes.get(finished)?.state ?? 'gone'}` });
        }
        return future;
    }

    // Today's gate's whole verdict for an unplanned future (the adapter, #jtdwm5n). A red the change caused is its
    // owner's (change.red); any other red and every void is recorded and goes nowhere.
    private async decideWhole(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: 'a verdict is too large' });
        }
        const checked = checkVerdict(body);
        if (typeof checked === 'string') {
            return jsonResponse(400, { error: checked });
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const entry = state.changes.get(checked.change);
            if (entry === undefined) {
                return jsonResponse(404, { error: `no change ${checked.change}` });
            }
            if (entry.future !== checked.verdict.future) {
                return jsonResponse(409, { error: `change ${checked.change} is tested in ${String(entry.future)}, not ${checked.verdict.future}` });
            }
            const future = this.liveFuture(state, checked.verdict.future);
            if (future instanceof Response) {
                return future;
            }
            if (future.units !== null) {
                return jsonResponse(409, { error: `future ${future.tree} is planned, so the judge decides it` });
            }
            if (future.decided !== null && future.decided.status !== 'void') {
                return jsonResponse(future.decided.run === checked.verdict.run ? 200 : 409, {
                    error: `future ${future.tree} was decided ${future.decided.status} by run ${future.decided.run}`,
                });
            }
            const subject = { change: checked.change, future: future.tree, run: checked.verdict.run };
            await this.append('verdict.decided', subject, { verdict: checked.verdict });
            if (checked.verdict.status === 'failed' && checked.verdict.cause === 'change') {
                await this.append('change.red', { change: checked.change, future: future.tree }, { verdict: checked.verdict });
            }
            return jsonResponse(200, { change: checked.change, state: entry.state, landable: futureLandable(future) });
        });
    }

    // The planner's plan for one future (contract v1.1's planning seam): one unit.planned per unit. A reuse needs the
    // index to hold a passed verdict for its exact key, since nothing is reused on the planner's word alone.
    private async plan(request: Request, tree: string): Promise<Response> {
        const body = await readBodyText(request, MaximumFutureBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `a plan is at most ${MaximumFutureBodyBytes} bytes` });
        }
        const units = await checkPlan(body);
        if (typeof units === 'string') {
            return jsonResponse(422, { error: units });
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const future = this.liveFuture(state, tree);
            if (future instanceof Response) {
                return future;
            }
            if (future.units !== null) {
                const same = sortedEqual([...future.units.keys()], units.map(function (unit) {
                    return unit.unitKey;
                }));
                return jsonResponse(same ? 200 : 409, same ? { future: tree, planned: future.units.size } : { error: `future ${tree} is already planned` });
            }
            if (future.decided !== null) {
                return jsonResponse(409, { error: `future ${tree} was decided ${future.decided.status} by today's gate, run ${future.decided.run}` });
            }
            for (const unit of units) {
                const indexed = state.verdicts.get(unit.unitKey)?.record;
                if (unit.decision === 'reuse' && (indexed === undefined || indexed.status !== 'passed' || (unit.reused !== null && indexed.run !== unit.reused))) {
                    return jsonResponse(422, { error: `unit ${unit.name} reuses a verdict the index doesn't hold passed for key ${unit.unitKey}` });
                }
            }
            // The judge decides every future, a reused unit included (it posts the reuse as that unit's record).
            for (const unit of units) {
                const reused = unit.decision === 'reuse' ? (state.verdicts.get(unit.unitKey)?.record ?? null) : null;
                await this.append(
                    'unit.planned',
                    { change: future.changes[0], future: tree, unitKey: unit.unitKey },
                    { name: unit.name, keyParts: unit.keyParts, decision: unit.decision, reason: unit.reason, reused: unit.reused, verdict: reused },
                );
            }
            return jsonResponse(200, { future: tree, planned: units.length });
        });
    }

    // The judge's batch for one run of a future: its records for every unit the plan runs, and its decision, which
    // the queue recomputes by the same rule before it logs anything.
    private async decideBatch(request: Request, tree: string): Promise<Response> {
        const body = await readBodyText(request, MaximumFutureBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `a batch is at most ${MaximumFutureBodyBytes} bytes` });
        }
        const batch = checkJudgeBatch(body, tree);
        if (typeof batch === 'string') {
            return jsonResponse(422, { error: batch });
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const existing = state.futures.get(tree);
            if (existing?.decided !== null && existing?.decided !== undefined && existing.decided.status !== 'void') {
                return jsonResponse(existing.decided.run === batch.run ? 200 : 409, {
                    future: tree,
                    decided: existing.decided,
                    error: existing.decided.run === batch.run ? undefined : `future ${tree} was decided ${existing.decided.status} by run ${existing.decided.run}`,
                });
            }
            const future = this.liveFuture(state, tree);
            if (future instanceof Response) {
                return future;
            }
            if (future.units === null) {
                return jsonResponse(409, { error: `future ${tree} has no plan yet` });
            }
            if (!future.changes.includes(batch.change)) {
                return jsonResponse(422, { error: `change ${batch.change} is not in future ${tree}` });
            }
            const planned = [...future.units.keys()];
            if (!sortedEqual(batch.plan, planned)) {
                return jsonResponse(422, { error: `the batch's plan is not the ${planned.length} units the planner planned for future ${tree}` });
            }
            const decision = judgeGreen(batch.plan, batch.verdicts);
            if (
                decision.status !== batch.decision.status ||
                !sortedEqual(decision.red, batch.decision.red) ||
                !sortedEqual(decision.excused, batch.decision.excused)
            ) {
                return jsonResponse(422, { error: `the records decide ${decision.status} (red ${decision.red.length}, excused ${decision.excused.length}), not ${batch.decision.status}`, problems: decision.problems });
            }
            for (const record of batch.verdicts) {
                await this.append('verdict.decided', { change: batch.change, future: tree, unitKey: record.unitKey, run: batch.run }, { verdict: record });
            }
            const logged = { ...batch.decision, problems: decision.problems };
            await this.append('verdict.decided', { change: batch.change, future: tree, run: batch.run }, { decision: logged, rule: batch.rule, quarantine: batch.quarantine });
            if (decision.status === 'red') {
                await this.append('change.red', { change: batch.change, future: tree, run: batch.run }, { decision: logged, kicks: batch.kicks });
            }
            return jsonResponse(200, { future: tree, decided: decision.status, landable: futureLandable(future) });
        });
    }

    // The landing orders the workshop pusher pulls: every change whose future may land, in line order.
    private async landings(): Promise<Response> {
        const state = await this.current();
        const orders = state.line.flatMap(function (change) {
            const entry = state.changes.get(change);
            if (entry === undefined || !isLive(entry) || entry.future === null || !futureLandable(state.futures.get(entry.future))) {
                return [];
            }
            return [{ change: change, future: entry.future, base: entry.record.base, owner: entry.record.owner }];
        });
        return jsonResponse(200, { landings: orders });
    }

    // What the workshop pusher did with a landing order: {main, from} when main is now exactly the change's future,
    // or {refused, main} when the fast-forward was refused (main moved past the change's base). Until restacking
    // (#05b5c2f), a refused change is parked and its owner resubmits on main.
    private async reportLanding(request: Request, change: string): Promise<Response> {
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        const parsed = parseJson(body ?? '');
        if (!isPlainObject(parsed) || typeof parsed.main !== 'string' || !shaPattern.test(parsed.main)) {
            return jsonResponse(400, { error: 'the body is {main, from} after a push, or {refused, main} after a refusal' });
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const entry = state.changes.get(change);
            if (entry === undefined) {
                return jsonResponse(404, { error: `no change ${change}` });
            }
            if (entry.state === 'landed') {
                return jsonResponse(entry.landed === parsed.main ? 200 : 409, { change: change, state: 'landed', landed: entry.landed });
            }
            if (!isLive(entry) || entry.future === null || !futureLandable(state.futures.get(entry.future))) {
                return jsonResponse(409, { error: `change ${change} has no landing order` });
            }
            if (typeof parsed.refused === 'string' && parsed.refused !== '') {
                await this.append(
                    'change.parked',
                    { change: change, future: entry.future },
                    { reason: `main is ${parsed.main}, which ${entry.future} doesn't fast-forward from: ${parsed.refused}. Resubmit on main.`, main: parsed.main },
                );
                return jsonResponse(200, { change: change, state: 'parked' });
            }
            if (parsed.main !== entry.future) {
                return jsonResponse(409, { error: `main ${parsed.main} is not ${entry.future}, the tree that was tested` });
            }
            if (typeof parsed.from !== 'string' || !shaPattern.test(parsed.from)) {
                return jsonResponse(400, { error: 'from is the main the push moved from, 40 lowercase hex digits' });
            }
            await this.append('change.landed', { change: change, future: entry.future }, { main: parsed.main, from: parsed.from });
            return jsonResponse(200, { change: change, state: 'landed', landed: parsed.main });
        });
    }

    private async readVerdict(unitKey: string): Promise<Response> {
        const indexed = (await this.current()).verdicts.get(unitKey);
        if (indexed === undefined) {
            return jsonResponse(404, { error: `no verdict for unit ${unitKey}` });
        }
        return jsonResponse(200, indexed.record);
    }

    // The futures waiting for the planner: no plan and no verdict yet, every change in them on its way.
    private async unplannedFutures(url: URL): Promise<Response> {
        if (url.searchParams.get('state') !== 'unplanned') {
            return jsonResponse(400, { error: 'state=unplanned is the one listing' });
        }
        const state = await this.current();
        const futures = [...state.futures.values()]
            .filter(function (future) {
                return (
                    future.units === null &&
                    future.decided === null &&
                    future.changes.every(function (change) {
                        return isLive(state.changes.get(change));
                    })
                );
            })
            .map(function (future) {
                return { future: future.tree, tree: future.tree, base: future.base, changes: future.changes };
            });
        return jsonResponse(200, { futures: futures });
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
            units: unitCounts(state.futures.get(entry.future ?? '')),
            verdict: entry.verdict,
            landed: entry.landed,
        });
    }

    private afterOf(request: Request): number {
        const after = Number(new URL(request.url).searchParams.get('after') ?? '0');
        return Number.isSafeInteger(after) && after >= 0 ? after : 0;
    }

    private eventLines(rows: { json: string }[]): Response {
        return new Response(
            rows
                .map(function (row) {
                    return row.json + '\n';
                })
                .join(''),
            { headers: { 'Content-Type': 'application/x-ndjson; charset=utf-8', 'Cache-Control': 'no-store' } },
        );
    }

    // The whole log after a sequence number, a page at a time, exactly as stored: anyone can replay it.
    private events(request: Request): Response {
        return this.eventLines(
            this.sql.exec<{ json: string }>('SELECT json FROM events WHERE seq > ? ORDER BY seq LIMIT ?', this.afterOf(request), MaximumEventsPage).toArray(),
        );
    }

    private async changeEvents(request: Request, change: string): Promise<Response> {
        const state = await this.current();
        if (!state.changes.has(change)) {
            return jsonResponse(404, { error: `no change ${change}` });
        }
        return this.eventLines(
            this.sql
                .exec<{ json: string }>('SELECT json FROM events WHERE change = ? AND seq > ? ORDER BY seq LIMIT ?', change, this.afterOf(request), MaximumEventsPage)
                .toArray(),
        );
    }

    private ownerEvents(request: Request): Response {
        const placeholders = OwnerEventTypes.map(function () {
            return '?';
        }).join(', ');
        return this.eventLines(
            this.sql
                .exec<{ json: string }>(
                    `SELECT json FROM events WHERE type IN (${placeholders}) AND seq > ? ORDER BY seq LIMIT ?`,
                    ...OwnerEventTypes,
                    this.afterOf(request),
                    MaximumEventsPage,
                )
                .toArray(),
        );
    }
}
