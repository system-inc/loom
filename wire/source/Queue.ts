// The queue: one Durable Object (idFromName('main')), the single writer of main's future (Loom's contracts, sections 1, 4
// and 5, and v1.1's seams). Every decision is an event appended to one hash-chained log, and the state is a pure
// function of that log, so replaying it gives the same queue. The Worker has already checked the token and the body's
// shape; this object checks the change against git and decides whether it joins the line, takes the planner's plan
// and the judge's verdicts for each future, and says which changes may land. It never moves main itself: a process on
// workshop pulls the landing orders, pushes with Kirk's fast-forward-only script, and reports back (Loom's four-hour
// cut, Oct 9), so no credential that can move main lives here.

import { DurableObject } from 'cloudflare:workers';
import { changeBoardOf } from './ChangeBoard';
import { jsonResponse, readBodyText } from './Http';

export const GenesisHash = '0'.repeat(64);
export const MaximumStackDepth = 5;
export const MaximumChangeBodyBytes = 1024 * 1024;
// A plan or a judge's batch carries every unit of a future, each verdict with its tests.
export const MaximumFutureBodyBytes = 32 * 1024 * 1024;
export const MaximumEventsPage = 1000;
export const UnitKeyVersion = 'loom-unit-v1';
// The one rule that decides an empty-planned future (#p74thc8): Judge runs every check push-main --ruled-gate ran.
export const DocsRule = 'ruled-gate-docs-v0';
// The board hears each moved change at most once a second (contracts v1.1), and a failed push is tried again.
export const BoardPushMilliseconds = 1000;
export const BoardRetryMilliseconds = 5000;
// The events an owner acts on; the owners' feed carries only these (contracts v1.1, section 5).
export const OwnerEventTypes: readonly EventType[] = ['change.landed', 'change.red', 'change.parked'];

export type EventType =
    | 'change.submitted'
    // git's facts cleared a change that waits for a block (the blocks switch on), rather than becoming its own future.
    | 'change.checked'
    | 'change.refused'
    | 'block.opened'
    // The workshop builder wrote a block's merge chain: its prefixes are futures and its conflicts are parked.
    | 'block.built'
    // A block's prefixes decided up to its first red: the longest green prefix lands, the changes behind the red wait again.
    | 'block.decided'
    | 'future.built'
    | 'unit.planned'
    // The planner's empty plan: no unit's key moved for a Markdown-only future, which Judge decides by its docs rule.
    | 'future.planned'
    // A plan withdrawn by a ruling before anything judged it, so the planner can plan the future again.
    | 'future.unplanned'
    | 'unit.placed'
    | 'unit.finished'
    | 'verdict.decided'
    | 'change.landed'
    | 'change.red'
    | 'change.kicked'
    | 'change.restacked'
    | 'change.parked'
    | 'rule.changed'
    // Main's red pause (#3ypyka5): the newest decided witness of main's tip was red, then a later one cleared it.
    | 'main.red'
    | 'main.green';

export type ChangeState = 'queued' | 'building' | 'testing' | 'landed' | 'red' | 'parked' | 'refused';

export interface ChangeRecord {
    change: string;
    sha: string;
    base: string;
    owner: string;
    paths: string[];
    parent: string | null;
    fixesRed: string | null;
    // A parity run (Release's proofs 1 and 2, #6c3xkws): its future is exactly merge(base, sha), the tree the box
    // record tested, it never joins a block with real changes, and no landing order is ever written for it.
    parity?: true;
    // A parity run's selection, the box record's own (Release's proof 1): exactly these packages run, uncached, and
    // within a package run.py split, exactly these tests.
    select?: ParitySelect;
    // A witness of main (Release's proof 3, #82d430f): a parity run of a main commit itself, base = sha, no paths, every
    // package planned uncached, never landed.
    witness?: true;
    submittedAt: string;
}

// Whether a request is a well-formed witness, or why not: a parity run of one sha with base the same sha, no paths, no
// select and no parent. Whether the sha is on main is git's fact, checked like any base.
export function witnessRefusal(fields: Record<string, unknown>): string | null {
    if (fields.witness === undefined) {
        return null;
    }
    if (fields.witness !== true) {
        return 'witness is true for a witness of main, or absent';
    }
    if (fields.parity !== true || fields.base !== fields.sha || !Array.isArray(fields.paths) || fields.paths.length > 0) {
        return 'a witness is a parity run of a main commit: parity true, base the sha itself, paths empty';
    }
    if (fields.select !== undefined || (fields.parent ?? null) !== null) {
        return 'a witness plans every package, alone: no select and no parent';
    }
    return null;
}

export interface ParitySelect {
    packages: string[];
    tests: Record<string, string[]>;
}

// {packages, tests?}: the packages a parity run plans, each once, and for any package run.py split, its test names.
export function checkParitySelect(value: unknown): ParitySelect | string {
    if (typeof value !== 'object' || value === null || Array.isArray(value)) {
        return 'select is {packages, tests}';
    }
    const fields = value as Record<string, unknown>;
    const packages = fields.packages;
    if (
        !Array.isArray(packages) ||
        packages.length === 0 ||
        new Set(packages).size !== packages.length ||
        !packages.every(function (name) {
            return typeof name === 'string' && name !== '';
        })
    ) {
        return 'select.packages is a non-empty list of import paths, each once';
    }
    const tests = fields.tests ?? {};
    if (typeof tests !== 'object' || tests === null || Array.isArray(tests)) {
        return 'select.tests maps a package to its test names';
    }
    for (const [name, list] of Object.entries(tests as Record<string, unknown>)) {
        if (!packages.includes(name) || !Array.isArray(list) || list.length === 0 || !list.every((test) => typeof test === 'string' && test !== '')) {
            return `select.tests[${JSON.stringify(name)}] names tests of a selected package`;
        }
    }
    return { packages: [...(packages as string[])].sort(), tests: tests as Record<string, string[]> };
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
    // The commit on main whose inverse this change's diff is, when it is a revert (the bridge's fact), else absent.
    revertOf?: string | null;
    // Every path a non-merge commit in base..sha touches (#f8973gv). Absent, the change is refused: no history, no door.
    historyPaths?: string[];
    // Each Python test in the diff that a non-test file names, with those files (#xz7j9ea). Absent, refused too.
    gateNamed?: { path: string; users: string[] }[];
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
    keyParts: Record<string, unknown>;
    decision: 'reuse' | 'run';
    reused: string | null;
    // What the unit needs to be placed (Planner, from cloud/fast-gate/unit-needs.json): placement only, never in the key.
    resources: Resources | null;
    // The tree key of Workshop's build of the future's tree, which a test or product unit runs (Planner, #w7agfa9):
    // placement only, never in the key.
    tree: string | null;
    verdict: UnitVerdict | null;
}

export interface Resources {
    memoryMegabytes: number;
    cpus: number;
}

// A tree to test and the changes in it, in order, keyed by the tree's sha. Slice 1's futures are one change each,
// tree = the change's sha.
export interface FutureEntry {
    tree: string;
    base: string;
    changes: string[];
    // null until the planner posts its units.
    units: Map<string, UnitEntry> | null;
    // The planner's reason when its plan is empty (no unit's key moved, every path Markdown), else null.
    empty: { reason: string } | null;
    // Today's gate's whole verdict, on an unplanned future only.
    whole: Verdict | null;
    // The run that decided the future and how: a judge's batch, today's gate's whole verdict, or a plan that reused
    // every unit. A void lets the next run decide it.
    decided: { run: string; status: Decision['status'] } | null;
    // How many runs have decided it void: the judge's next run is attempt voids + 1.
    voids: number;
    // Whether any verdict was ever logged for it, a unit's or a whole one: after that its plan stands.
    judged: boolean;
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
    // Whether git's facts have cleared it. With no GitHub credential here, the bridge on Kirk's Mac reads them from
    // git and posts them (POST /submissions/<change>/facts); a change has no future until they clear it.
    checked: boolean;
    // Every sha the change was ever tested on, so a red one never returns (#qvcm8ez).
    shas: string[];
    // The block it joined, or null: with blocks on, a cleared change waits for the next block.
    block: number | null;
    // The main commit this change reverts, from git's facts, or null: a revert lands while main is red.
    revertOf: string | null;
}

// The rules a rule.changed event moves, each naming the landed commit that changed it (contracts v1, section 4).
export interface Rules {
    // Blocks (#j3t4qbg), off until after cutover: with them on, a cleared change waits, and the instant no block is in
    // flight the queue opens one with every waiting change in line order, up to the budget. No clock.
    blocks: { on: boolean; budget: number };
    // Outside verdicts (#srsq44p): while refused, today's gate can't decide a future with a whole verdict, and only
    // Judge's batches decide. It turns on only after Judge decided a real change that landed (Loom, 01:25Z).
    outsideVerdicts: { refused: boolean };
}

export interface BlockEntry {
    block: number;
    changes: string[];
    built: boolean;
    // Decided: the change whose prefix lands (the longest green one), or null when none does; the slot is free.
    resolved: boolean;
    landing: string | null;
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
    // main as the lander last reported it (the newest change.landed's main), or null before any landing.
    landedMain: string | null;
    rules: Rules;
    blocks: Map<number, BlockEntry>;
    // Main held red by the newest decided witness of its tip, or null (#3ypyka5): while set, only a fix-forward naming
    // that main or a revert gets a landing order, and every other green change waits.
    mainRed: { witness: string; main: string; units: string[] } | null;
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
        if (!['sha', 'base', 'owner', 'paths', 'parent', 'fixesRed', 'parity', 'select', 'witness'].includes(key)) {
            return `unknown field ${JSON.stringify(key)}; a change is sha, base, owner, paths, and optionally parent, fixesRed, parity, select and witness`;
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
    const witness = witnessRefusal(parsed);
    if (witness !== null) {
        return witness;
    }
    if (
        !Array.isArray(parsed.paths) ||
        (parsed.paths.length === 0 && parsed.witness !== true) ||
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
    if (parsed.parity !== undefined && typeof parsed.parity !== 'boolean') {
        return 'parity is true for a parity run, or absent';
    }
    if (parsed.parity === true && parent !== null) {
        return 'a parity run tests one tree alone, so it has no parent';
    }
    if (parsed.select !== undefined && parsed.parity !== true) {
        return "select is a parity run's: a real change runs what the planner selects";
    }
    const select = parsed.select === undefined ? null : checkParitySelect(parsed.select);
    if (typeof select === 'string') {
        return select;
    }
    return {
        sha: parsed.sha,
        base: parsed.base,
        owner: parsed.owner,
        paths: [...(parsed.paths as string[])].sort(),
        parent: parent,
        fixesRed: fixesRed,
        ...(parsed.parity === true ? { parity: true as const } : {}),
        ...(select === null ? {} : { select: select }),
        ...(parsed.witness === true ? { witness: true as const } : {}),
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
    return gitRefusalOf(request, facts) ?? lineRefusalOf(request, state);
}

// What git rules out: the sha missing, a base that isn't its ancestor or isn't on main, a path outside the diff.
export function gitRefusalOf(request: Omit<ChangeRecord, 'change' | 'submittedAt'>, facts: GitFacts): string | null {
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
    // Every path of the diff is declared (#969a59c): every path rule below and after reads the declared paths, so a
    // change can't leave out the one a rule would refuse.
    const declared = new Set(request.paths);
    const undeclared = facts.diffPaths.filter(function (path) {
        return !declared.has(path);
    });
    if (undeclared.length > 0) {
        return `the paths leave out ${undeclared.length} of the diff base..sha: ${undeclared.slice(0, 5).join(', ')}; declare every path`;
    }
    // A harness change can hide a check that can't fail, so it comes with its mutant evidence (push-main l.806-811).
    const harness = request.paths.filter(function (path) {
        return /^stage3\/(fixtures|meter)\//.test(path) && !/(_test\.go$|\/testdata\/)/.test(path);
    });
    if (harness.length > 0 && !request.paths.some((path) => /mutant/i.test(path))) {
        return `it changes test harness (${harness.slice(0, 3).join(', ')}) with no mutant evidence among its paths`;
    }
    // Its history, not only its tree (push-main l.796-805): a commit that touches a path the final diff drops is still
    // recorded as merged, and a later plain merge of its branch then deletes the change.
    if (facts.historyPaths === undefined || facts.gateNamed === undefined) {
        return "git's facts carry no history or gate-logic check, so the change can't be cleared";
    }
    const history = facts.historyPaths.filter(function (path) {
        return !diff.has(path);
    });
    if (history.length > 0) {
        return `its commits beyond base touch paths its diff doesn't: ${history.slice(0, 5).join(', ')}; cherry-pick its own commits onto main instead`;
    }
    // A Python test a non-test file names is gate logic, and gate logic gets a gate (push-main l.787-795).
    const named = facts.gateNamed[0];
    if (named !== undefined) {
        return `${named.path} is named by ${named.users.slice(0, 3).join(', ')}, which isn't a test, so it's gate logic: send it to Loom for a gate`;
    }
    return null;
}

// What the line rules out, with no git: an unknown parent, a stack too deep, a sha already on its way.
export function lineRefusalOf(request: Omit<ChangeRecord, 'change' | 'submittedAt'>, state: QueueState): string | null {
    if (request.parent !== null && !state.changes.has(request.parent)) {
        return `parent ${request.parent} is not a change this queue holds`;
    }
    if (stackDepth(state, request.parent) + 1 > MaximumStackDepth) {
        return `a stack is at most ${MaximumStackDepth} deep`;
    }
    // A future is keyed by its tree, so one sha is in the line once.
    for (const [change, entry] of state.changes) {
        if (entry.record.sha === request.sha && isLive(entry)) {
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
        return future.whole !== null && decisionOf(future.whole) === 'green' && future.whole.future === future.tree;
    }
    // An empty plan has no unit to recompute: Judge's green under the docs rule (the only batch it takes) decides it.
    if (future.units.size === 0) {
        return future.empty !== null;
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

// Whether a future is a parity run's: tested on exactly its tree, never landed.
export function parityOf(state: QueueState, future: FutureEntry): boolean {
    return future.changes.some(function (change) {
        return state.changes.get(change)?.record.parity === true;
    });
}

export function emptyState(): QueueState {
    return { changes: new Map(), futures: new Map(), verdicts: new Map(), line: [], refused: 0, head: GenesisHash, seq: 0, landedMain: null, rules: { blocks: { on: false, budget: 8 }, outsideVerdicts: { refused: false } }, blocks: new Map(), mainRed: null };
}

// A whole verdict's decision, by the judge's rule: a failure that is main's red (excused, as Green excuses it) doesn't
// make the change red.
function decisionOf(verdict: Verdict): Decision['status'] {
    if (verdict.status === 'passed' || (verdict.status === 'failed' && verdict.cause === 'mainRed')) {
        return 'green';
    }
    return verdict.status === 'failed' ? 'red' : 'void';
}

// Applies one event to the state. Replay is this over the log in order; nothing here reads a clock or asks git.
export function apply(state: QueueState, event: QueueEvent, hash: string): void {
    const entry = state.changes.get(event.subject.change ?? '');
    if (event.type === 'change.submitted') {
        const record = event.data.record as ChangeRecord;
        state.changes.set(record.change, {
            record: record,
            state: 'queued',
            position: state.line.length,
            future: null,
            verdict: null,
            landed: null,
            checked: event.data.facts !== null,
            shas: [record.sha],
            block: null,
            revertOf: ((event.data.facts ?? null) as GitFacts | null)?.revertOf ?? null,
        });
        state.line.push(record.change);
    }
    else if (event.type === 'change.checked' && entry !== undefined) {
        entry.checked = true;
        entry.revertOf = ((event.data.facts ?? null) as GitFacts | null)?.revertOf ?? null;
    }
    else if (event.type === 'main.red') {
        state.mainRed = { witness: event.data.witness as string, main: event.data.main as string, units: event.data.units as string[] };
    }
    else if (event.type === 'main.green') {
        state.mainRed = null;
    }
    else if (event.type === 'rule.changed') {
        if (event.data.rule === 'blocks') {
            state.rules.blocks = event.data.value as Rules['blocks'];
        }
        else if (event.data.rule === 'outsideVerdicts') {
            state.rules.outsideVerdicts = event.data.value as Rules['outsideVerdicts'];
        }
    }
    else if (event.type === 'block.opened') {
        const changes = event.data.changes as string[];
        state.blocks.set(event.data.block as number, { block: event.data.block as number, changes: changes, built: false, resolved: false, landing: null });
        for (const change of changes) {
            const member = state.changes.get(change);
            if (member !== undefined) {
                member.block = event.data.block as number;
            }
        }
    }
    else if (event.type === 'block.decided') {
        const block = state.blocks.get(event.data.block as number);
        if (block !== undefined) {
            block.resolved = true;
            block.landing = (event.data.lands ?? null) as string | null;
        }
        for (const change of event.data.back as string[]) {
            const member = state.changes.get(change);
            if (member !== undefined) {
                member.block = null;
                member.future = null;
                member.verdict = null;
            }
        }
    }
    else if (event.type === 'block.built') {
        const block = state.blocks.get(event.data.block as number);
        if (block !== undefined) {
            block.built = true;
        }
    }
    else if (event.type === 'change.refused') {
        state.refused++;
        // A change refused once git's facts arrived leaves the line.
        if (entry !== undefined) {
            entry.state = 'refused';
            entry.verdict = { reason: event.data.reason };
            state.line = state.line.filter(function (id) {
                return id !== entry.record.change;
            });
        }
    }
    else if (event.type === 'future.built') {
        const tree = event.subject.future ?? '';
        const changes = event.data.changes as string[];
        state.futures.set(tree, { tree: tree, base: event.data.base as string, changes: changes, units: null, empty: null, whole: null, decided: null, voids: 0, judged: false });
        // A block's prefix future (main, +A, +B) is the newest change's own; the changes ahead of it keep theirs.
        const tested = event.data.block === undefined ? changes : changes.slice(-1);
        for (const change of tested) {
            const member = state.changes.get(change);
            if (member !== undefined) {
                member.future = tree;
                member.checked = true;
                if (event.data.facts !== undefined) {
                    member.revertOf = ((event.data.facts ?? null) as GitFacts | null)?.revertOf ?? null;
                }
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
                keyParts: event.data.keyParts as Record<string, unknown>,
                decision: event.data.decision as UnitEntry['decision'],
                reused: (event.data.reused ?? null) as string | null,
                resources: (event.data.resources ?? null) as Resources | null,
                tree: (event.data.tree ?? null) as string | null,
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
    else if (event.type === 'future.planned') {
        const future = state.futures.get(event.subject.future ?? '');
        if (future !== undefined) {
            future.units = new Map();
            future.empty = { reason: event.data.reason as string };
            for (const change of future.changes) {
                const member = state.changes.get(change);
                if (member !== undefined && member.state === 'queued') {
                    member.state = 'testing';
                }
            }
        }
    }
    else if (event.type === 'future.unplanned') {
        const future = state.futures.get(event.subject.future ?? '');
        if (future !== undefined) {
            future.units = null;
            future.empty = null;
            for (const change of future.changes) {
                const member = state.changes.get(change);
                if (member !== undefined && member.state === 'testing') {
                    member.state = 'queued';
                }
            }
        }
    }
    else if (event.type === 'verdict.decided') {
        const future = state.futures.get(event.subject.future ?? '');
        if (future !== undefined) {
            future.judged = true;
        }
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
            future.voids += future.decided.status === 'void' ? 1 : 0;
        }
        else if (event.data.verdict !== undefined && future !== undefined) {
            const verdict = event.data.verdict as Verdict;
            future.whole = verdict;
            future.decided = { run: verdict.run, status: decisionOf(verdict) };
            future.voids += future.decided.status === 'void' ? 1 : 0;
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
    else if (event.type === 'change.restacked' && entry !== undefined) {
        // The same change on a new sha: back in the line where it was, unchecked until git's facts clear the new sha.
        entry.record = { ...entry.record, sha: event.data.to as string, base: event.data.base as string, paths: event.data.paths as string[] };
        entry.shas.push(entry.record.sha);
        entry.state = 'queued';
        entry.future = null;
        entry.verdict = null;
        entry.checked = false;
    }
    else if (event.type === 'change.parked' && entry !== undefined) {
        entry.state = 'parked';
    }
    else if (event.type === 'change.landed' && entry !== undefined) {
        entry.state = 'landed';
        entry.landed = event.data.main as string;
        state.landedMain = entry.landed;
        state.line = state.line.filter(function (id) {
            return id !== entry.record.change;
        });
    }
    state.head = hash;
    state.seq = event.seq;
}

// The first real change that landed on a future Judge decided by its units, not a whole verdict, or null: the proof
// outside verdicts wait on before they can be refused.
export function judgeLanded(state: QueueState): string | null {
    for (const entry of state.changes.values()) {
        const future = state.futures.get(entry.future ?? '');
        if (entry.state === 'landed' && entry.record.parity !== true && future !== undefined && future.whole === null && future.units !== null) {
            return entry.record.change;
        }
    }
    return null;
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

// A whole verdict's shape as today's gate's adapter posts it for one change: {change, verdict}, and gateMerge {base}
// when today's gate tested the change merged onto a newer main (a gate merge, first parent base, second the change's
// sha) rather than the sha itself. The adapter read those parents from git, as it reads every git fact.
export function checkVerdict(body: string): { change: string; verdict: Verdict; gateMerge: { base: string } | null } | string {
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
    const gateMerge = parsed.gateMerge ?? null;
    if (gateMerge !== null && (!isPlainObject(gateMerge) || typeof gateMerge.base !== 'string' || !shaPattern.test(gateMerge.base))) {
        return 'gateMerge is {base}, the main the gate merged the change onto, or absent';
    }
    return {
        change: parsed.change,
        verdict: { future: verdict.future, run: verdict.run, status: verdict.status as VerdictStatus, cause: cause as VerdictCause, rule: verdict.rule },
        gateMerge: gateMerge === null ? null : { base: gateMerge.base as string },
    };
}

export interface PlannedUnit {
    name: string;
    unitKey: string;
    keyParts: Record<string, unknown>;
    decision: 'reuse' | 'run';
    reason: string;
    reused: string | null;
    resources: Resources | null;
    tree: string | null;
}

function isResources(value: unknown): value is Resources {
    return (
        isPlainObject(value) &&
        Object.keys(value).sort().join(',') === 'cpus,memoryMegabytes' &&
        Number.isSafeInteger(value.memoryMegabytes) &&
        (value.memoryMegabytes as number) > 0 &&
        Number.isSafeInteger(value.cpus) &&
        (value.cpus as number) > 0
    );
}

// The planner's body for POST /futures/<tree>/plan: the unit list, each unit once, its key computed from its parts, or
// {empty: true, reason} when no unit's key moved.
export async function checkPlan(body: string): Promise<PlannedUnit[] | { reason: string } | string> {
    const parsed = parseJson(body);
    if (isPlainObject(parsed) && parsed.empty === true) {
        return typeof parsed.reason === 'string' && parsed.reason !== '' ? { reason: parsed.reason } : 'an empty plan names its reason';
    }
    if (!Array.isArray(parsed) || parsed.length === 0) {
        return 'the plan is a non-empty JSON list of units, or {empty: true, reason}';
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
        if (item.resources !== undefined && item.resources !== null && !isResources(item.resources)) {
            return `unit ${item.name}'s resources is {memoryMegabytes, cpus}, each a positive whole number`;
        }
        if (item.tree !== undefined && item.tree !== null && (typeof item.tree !== 'string' || !hashPattern.test(item.tree))) {
            return `unit ${item.name}'s tree is a tree key, 64 lowercase hex digits`;
        }
        if (typeof item.tree === 'string' && item.keyParts.kind !== 'test' && item.keyParts.kind !== 'product') {
            return `unit ${item.name} is a ${String(item.keyParts.kind)} unit, and only a test or product unit runs its tree's build`;
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
            resources: isResources(item.resources) ? { memoryMegabytes: item.resources.memoryMegabytes, cpus: item.resources.cpus } : null,
            tree: typeof item.tree === 'string' ? item.tree : null,
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
    // The changes moved since the last board push, each with the seq of its newest event: kept in memory so a push
    // reads nothing from storage. After an eviction it is empty, and the alarm falls back to the events after boardSeq.
    private owed = new Map<string, number>();
    // Whether blocks may turn on: not until a block can resolve (#6d7179w), or one rule.changed would stall every change
    // in a block nothing lands (Loom, 01:11Z). A test turns it on for itself.
    blocksReady = false;
    // The git facts' source: GitHub when the Worker holds a read token, else null, and the bridge posts them later.
    // A test sets its own on this object.
    history: History | null;
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
            CREATE TABLE IF NOT EXISTS facts (name TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
        `);
        const token = (environment as unknown as { GITHUB_TOKEN?: string }).GITHUB_TOKEN ?? '';
        this.history = token === '' ? null : new GitHubHistory(token);
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
        if (path === '/rules' && method === 'POST') {
            return this.changeRule(request);
        }
        if (path === '/blocks' && method === 'GET') {
            return this.unbuiltBlocks(url);
        }
        const builtMatch = /^\/blocks\/([1-9][0-9]{0,8})\/built$/.exec(path);
        if (builtMatch !== null && method === 'POST') {
            return this.blockBuilt(request, Number(builtMatch[1]));
        }
        if (path === '/head' && method === 'GET') {
            return this.readHead();
        }
        if (path === '/log' && method === 'GET') {
            return this.events(request);
        }
        if (path === '/events' && method === 'GET') {
            return url.searchParams.get('owners') === '1' ? this.ownerEvents(request) : this.events(request);
        }
        if (path === '/futures' && method === 'GET') {
            return this.listFutures(url);
        }
        if (path === '/landings' && method === 'GET') {
            return this.landings();
        }
        if (path === '/submissions' && method === 'GET') {
            return this.uncheckedChanges(url);
        }
        const factsMatch = /^\/submissions\/(chg_[0-9a-z]{26})\/facts$/.exec(path);
        if (factsMatch !== null && method === 'POST') {
            return this.takeFacts(request, factsMatch[1] ?? '');
        }
        const futureMatch = /^\/futures\/([0-9a-f]{40})\/(plan|verdicts|unplan)$/.exec(path);
        if (futureMatch !== null && method === 'POST') {
            if (futureMatch[2] === 'unplan') {
                return this.unplan(request, futureMatch[1] ?? '');
            }
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
        const resubmitMatch = /^\/changes\/(chg_[0-9a-z]{26})\/sha$/.exec(path);
        if (resubmitMatch !== null && method === 'POST') {
            return this.resubmit(request, resubmitMatch[1] ?? '');
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
        if (subject.change !== undefined) {
            this.owed.set(subject.change, event.seq);
            await this.scheduleBoardPush();
        }
        return event;
    }

    // ---------- The board of changes ----------

    private fact(name: string): string | null {
        return this.sql.exec<{ value: string }>('SELECT value FROM facts WHERE name = ?', name).toArray()[0]?.value ?? null;
    }

    private setFact(name: string, value: string): void {
        this.sql.exec('INSERT INTO facts (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value', name, value);
    }

    // Makes sure an alarm will push the moved changes, no sooner than a second after the last push. Only storage is
    // touched, so the request that moved a change never waits on the board and can't fail because of it.
    private async scheduleBoardPush(): Promise<void> {
        if ((await this.ctx.storage.getAlarm()) === null) {
            await this.ctx.storage.setAlarm(Math.max(Date.now(), Number(this.fact('boardPushedAt') ?? '0') + BoardPushMilliseconds));
        }
    }

    // Pushes every change an event touched since the last push, each as its summary now. Nothing is lost: until a push
    // succeeds, the sequence it covers stays owed and the next alarm tries again.
    override async alarm(): Promise<void> {
        const evicted = this.state === null;
        const state = await this.current();
        const pushedSeq = Number(this.fact('boardSeq') ?? '0');
        // Normally the moved changes are in memory, and the push reads no rows. After an eviction the events after
        // boardSeq (a primary-key range) name them.
        const moved = evicted
            ? this.sql.exec<{ change: string }>('SELECT DISTINCT change FROM events WHERE seq > ? AND change IS NOT NULL', pushedSeq).toArray()
            : [...this.owed.keys()].map(function (change) {
                  return { change: change };
              });
        const head = state.seq;
        this.owed.clear();
        this.setFact('boardPushedAt', String(Date.now()));
        try {
            const board = changeBoardOf(this.env);
            for (const row of moved) {
                const entry = state.changes.get(row.change);
                if (entry === undefined) {
                    continue;
                }
                const response = await board.fetch('https://board/change', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({
                        change: row.change,
                        owner: entry.record.owner,
                        sha: entry.record.sha,
                        state: entry.state,
                        future: entry.future,
                        units: unitCounts(state.futures.get(entry.future ?? '')),
                        updatedAt: this.now(),
                    }),
                });
                await response.body?.cancel();
                if (!response.ok) {
                    throw new Error(`the board answered ${response.status}`);
                }
            }
        }
        catch {
            // Still owed: put them back, so the retry pushes them from memory too.
            for (const row of moved) {
                if (!this.owed.has(row.change)) {
                    this.owed.set(row.change, head);
                }
            }
            await this.ctx.storage.setAlarm(Date.now() + BoardRetryMilliseconds);
            return;
        }
        this.setFact('boardSeq', String(head));
        // An event appended while the push ran is owed; make sure an alarm comes for it.
        if (state.seq > head) {
            await this.ctx.storage.setAlarm(Date.now() + BoardPushMilliseconds);
        }
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
        // nothing is decided, so nothing is logged: the submitter tries again. With no history here, the change joins
        // the line unchecked and git's facts come from the bridge.
        let facts: GitFacts | null = null;
        if (this.history !== null) {
            try {
                facts = await this.history.facts(checked.sha, checked.base);
            }
            catch (error) {
                return jsonResponse(503, { reason: `git facts are unavailable, try again: ${(error as Error).message}` });
            }
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const reason = facts === null ? lineRefusalOf(checked, state) : refusalOf(checked, facts, state);
            if (reason !== null) {
                await this.append('change.refused', {}, { request: checked, facts: facts, reason: reason });
                return jsonResponse(422, { reason: reason });
            }
            const record: ChangeRecord = { change: newChangeId(), submittedAt: this.now(), ...checked };
            await this.append('change.submitted', { change: record.change }, { record: record, facts: facts });
            if (facts !== null) {
                await this.clear(record, facts);
            }
            const entry = state.changes.get(record.change);
            return jsonResponse(201, { change: record.change, state: entry?.state ?? 'queued', position: entry?.position ?? 0 });
        });
    }

    // The owner moves a red or parked change to a new sha, keeping its id and its place (#50j0pvg): change.restacked,
    // then git's facts on the new sha exactly as at submit. A sha the change was ever tested on never comes back.
    private async resubmit(request: Request, change: string): Promise<Response> {
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        if (body === null) {
            return jsonResponse(413, { reason: `a change is at most ${MaximumChangeBodyBytes} bytes` });
        }
        const checked = checkChangeRequest(body);
        if (typeof checked === 'string') {
            return jsonResponse(422, { reason: checked });
        }
        if (checked.parent !== null || checked.fixesRed !== null || checked.parity === true || checked.select !== undefined || checked.witness === true) {
            return jsonResponse(422, { reason: 'a resubmit moves the sha, base and paths only; the change keeps everything else' });
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const entry = state.changes.get(change);
            if (entry === undefined) {
                return jsonResponse(404, { reason: `no change ${change}` });
            }
            if (entry.record.owner !== checked.owner) {
                return jsonResponse(403, { reason: `change ${change} is ${entry.record.owner}'s` });
            }
            // Kirk's yes (02:3xZ): a queued change whose future has no plan and no verdict moves too, so it's judged once on
            // main's tip. The queue doesn't see placement, so a planned future is withdrawn first (unplan, which refuses
            // once anything judged it); a plan or a verdict here refuses the move.
            const future = state.futures.get(entry.future ?? '');
            const untested = entry.state === 'queued' && (future === undefined || (future.units === null && !future.judged && future.decided === null));
            if (entry.state !== 'red' && entry.state !== 'parked' && !untested) {
                return jsonResponse(409, { reason: `change ${change} is ${entry.state}; only a red or parked change, or a queued one with no plan and no verdict, moves to a new sha` });
            }
            if (entry.shas.includes(checked.sha)) {
                return jsonResponse(422, { reason: `change ${change} was already tested on ${checked.sha}; a resubmit needs a new sha` });
            }
            const duplicate = lineRefusalOf({ ...checked, parent: entry.record.parent }, state);
            if (duplicate !== null) {
                return jsonResponse(422, { reason: duplicate });
            }
            await this.append('change.restacked', { change: change }, { from: entry.record.sha, to: checked.sha, base: checked.base, paths: checked.paths });
            return jsonResponse(200, { change: change, state: 'queued', sha: checked.sha });
        });
    }

    // A change git's facts cleared: its own future (blocks off, or a parity run), or it waits for the next block.
    private async clear(record: ChangeRecord, facts: GitFacts): Promise<void> {
        const state = await this.current();
        if (!state.rules.blocks.on || record.parity === true) {
            await this.buildFuture(record, facts);
            return;
        }
        await this.append('change.checked', { change: record.change }, { facts: facts });
        await this.openBlock();
    }

    // The instant no block is in flight, every waiting change in line order, up to the budget, becomes one block. A
    // waiting change is checked, on its way, in no block and no future yet.
    private async openBlock(): Promise<void> {
        const state = await this.current();
        // Lockstep: a block holds the slot until it's decided and its landing has landed (or left the line), so the next
        // chain is built on the main that landing made.
        const inFlight = [...state.blocks.values()].some(function (block) {
            return !block.resolved || (block.landing !== null && isLive(state.changes.get(block.landing)));
        });
        if (!state.rules.blocks.on || inFlight) {
            return;
        }
        const waiting = state.line.filter(function (change) {
            const entry = state.changes.get(change);
            return entry !== undefined && isLive(entry) && entry.checked && entry.future === null && entry.block === null && entry.record.parity !== true;
        });
        if (waiting.length === 0) {
            return;
        }
        await this.append('block.opened', {}, { block: state.blocks.size + 1, changes: waiting.slice(0, state.rules.blocks.budget) });
    }

    // Decides a block once its prefixes are decided in order up to the first red (lockstep): the longest green prefix
    // before any red lands as one landing order (its newest change's), a void waits for its rerun, and every change
    // behind the red goes back to wait for the next block, which opens at once. A parked change (a conflict) was never
    // a prefix. Logged as block.decided {block, lands, back}.
    private async decideBlock(number: number | null): Promise<void> {
        const state = await this.current();
        const block = number === null ? undefined : state.blocks.get(number);
        if (block === undefined || block.resolved || !block.built) {
            return;
        }
        const prefixes = block.changes.filter(function (change) {
            const entry = state.changes.get(change);
            return entry !== undefined && entry.block === block.block && entry.future !== null;
        });
        let lands: string | null = null;
        let back: string[] = [];
        for (const [index, change] of prefixes.entries()) {
            const status = state.futures.get(state.changes.get(change)?.future ?? '')?.decided?.status;
            if (status === undefined || status === 'void') {
                return;
            }
            if (status === 'red') {
                back = prefixes.slice(index + 1);
                break;
            }
            lands = change;
        }
        await this.append('block.decided', {}, { block: block.block, lands: lands, back: back });
        await this.openBlock();
    }

    // A rule moves only by a rule.changed naming the landed commit that changed it: blocks, or outside verdicts.
    private async changeRule(request: Request): Promise<Response> {
        const parsed = parseJson((await readBodyText(request, MaximumChangeBodyBytes)) ?? '');
        if (isPlainObject(parsed) && parsed.rule === 'outsideVerdicts') {
            return this.changeOutsideVerdicts(parsed);
        }
        if (
            !isPlainObject(parsed) ||
            parsed.rule !== 'blocks' ||
            !isPlainObject(parsed.value) ||
            typeof parsed.value.on !== 'boolean' ||
            !Number.isSafeInteger(parsed.value.budget) ||
            (parsed.value.budget as number) < 1 ||
            typeof parsed.commit !== 'string' ||
            !shaPattern.test(parsed.commit)
        ) {
            return jsonResponse(400, { error: 'the body is {rule: blocks, value: {on, budget}, commit: the landed commit that changed it}' });
        }
        const value = { on: parsed.value.on, budget: parsed.value.budget as number };
        if (value.on && !this.blocksReady) {
            return jsonResponse(422, { error: "blocks can't turn on until a block can resolve and free its slot (#6d7179w, not landed)" });
        }
        const commit = parsed.commit;
        return this.ctx.blockConcurrencyWhile(async () => {
            await this.append('rule.changed', {}, { rule: 'blocks', value: value, commit: commit });
            await this.openBlock();
            return jsonResponse(200, { rules: (await this.current()).rules });
        });
    }

    private async changeOutsideVerdicts(parsed: Record<string, unknown>): Promise<Response> {
        if (!isPlainObject(parsed.value) || typeof parsed.value.refused !== 'boolean' || typeof parsed.commit !== 'string' || !shaPattern.test(parsed.commit)) {
            return jsonResponse(400, { error: 'the body is {rule: outsideVerdicts, value: {refused}, commit: the landed commit that changed it}' });
        }
        const value = { refused: parsed.value.refused };
        const commit = parsed.commit;
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            if (value.refused && judgeLanded(state) === null) {
                return jsonResponse(422, { error: "outside verdicts can't be refused until Judge has decided a real change that landed (Loom, 01:25Z)" });
            }
            await this.append('rule.changed', {}, { rule: 'outsideVerdicts', value: value, commit: commit });
            return jsonResponse(200, { rules: (await this.current()).rules });
        });
    }

    // The workshop builder's chain for a block (#7hn5em0): {base, prefixes: [{tree, change}], conflicts: [{change, paths}]}.
    // Each prefix adds one change onto the one before it, starting from main at base, in the block's order; a change that
    // conflicts with those ahead of it is parked with the paths and left out. Each prefix becomes the newest change's
    // future, carrying every change ahead of it.
    private async blockBuilt(request: Request, block: number): Promise<Response> {
        const parsed = parseJson((await readBodyText(request, MaximumChangeBodyBytes)) ?? '');
        if (!isPlainObject(parsed) || typeof parsed.base !== 'string' || !shaPattern.test(parsed.base) || !Array.isArray(parsed.prefixes) || !Array.isArray(parsed.conflicts)) {
            return jsonResponse(400, { error: 'the body is {base, prefixes: [{tree, change}], conflicts: [{change, paths}]}' });
        }
        const prefixes = parsed.prefixes as { tree?: unknown; change?: unknown }[];
        const conflicts = parsed.conflicts as { change?: unknown; paths?: unknown }[];
        const base = parsed.base;
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const entry = state.blocks.get(block);
            if (entry === undefined) {
                return jsonResponse(404, { error: `no block ${block}` });
            }
            if (entry.built) {
                return jsonResponse(409, { error: `block ${block} is built` });
            }
            const conflicted = conflicts.map((conflict) => String(conflict.change));
            const added = prefixes.map((prefix) => String(prefix.change));
            const expected = entry.changes.filter((change) => !conflicted.includes(change));
            if (canonical(added) !== canonical(expected) || !conflicted.every((change) => entry.changes.includes(change))) {
                return jsonResponse(422, { error: `block ${block}'s prefixes add exactly its changes in order, less the conflicts` });
            }
            if (!prefixes.every((prefix) => typeof prefix.tree === 'string' && shaPattern.test(prefix.tree)) || !conflicts.every((conflict) => Array.isArray(conflict.paths))) {
                return jsonResponse(422, { error: 'each prefix names its tree, each conflict its paths' });
            }
            for (const conflict of conflicts) {
                const change = String(conflict.change);
                await this.append('change.parked', { change: change }, {
                    reason: `it conflicts with the changes ahead of it in block ${block}: ${(conflict.paths as string[]).join(', ')}. Resubmit on main.`,
                    block: block,
                });
                await this.parkDependents(change);
            }
            for (const [index, prefix] of prefixes.entries()) {
                await this.append('future.built', { change: added[index], future: String(prefix.tree) }, { base: base, changes: added.slice(0, index + 1), block: block });
            }
            await this.append('block.built', {}, { block: block, base: base });
            await this.decideBlock(block);
            return jsonResponse(200, { block: block, futures: prefixes.length, parked: conflicts.length });
        });
    }

    // The blocks the workshop builder hasn't built, each with its changes in order.
    private async unbuiltBlocks(url: URL): Promise<Response> {
        if (url.searchParams.get('state') !== 'unbuilt') {
            return jsonResponse(400, { error: 'state=unbuilt is the one listing' });
        }
        const state = await this.current();
        const blocks = [...state.blocks.values()]
            .filter(function (block) {
                return !block.built;
            })
            .map(function (block) {
                return {
                    block: block.block,
                    changes: block.changes.map(function (change) {
                        const record = state.changes.get(change)?.record;
                        return { change: change, sha: record?.sha, base: record?.base, owner: record?.owner };
                    }),
                };
            });
        return jsonResponse(200, { blocks: blocks });
    }

    // Slice 1: a checked change is its own future, its tree its sha. Speculation (#j3t4qbg) builds main+A+B instead.
    private async buildFuture(record: ChangeRecord, facts: GitFacts): Promise<void> {
        await this.append('future.built', { change: record.change, future: record.sha }, { base: record.base, changes: [record.change], facts: facts });
    }

    // The changes waiting for git's facts, oldest first, for the bridge.
    private async uncheckedChanges(url: URL): Promise<Response> {
        if (url.searchParams.get('state') !== 'unchecked') {
            return jsonResponse(400, { error: 'state=unchecked is the one listing' });
        }
        const state = await this.current();
        const changes = state.line.flatMap(function (change) {
            const entry = state.changes.get(change);
            return entry === undefined || entry.checked || !isLive(entry)
                ? []
                : [{ change: change, sha: entry.record.sha, base: entry.record.base, paths: entry.record.paths }];
        });
        return jsonResponse(200, { changes: changes });
    }

    // Git's facts for an unchecked change, from the bridge: they clear it into its future, or refuse it by reason.
    private async takeFacts(request: Request, change: string): Promise<Response> {
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        const parsed = parseJson(body ?? '');
        if (
            !isPlainObject(parsed) ||
            typeof parsed.shaExists !== 'boolean' ||
            typeof parsed.baseIsAncestor !== 'boolean' ||
            typeof parsed.baseOnMain !== 'boolean' ||
            !Array.isArray(parsed.diffPaths) ||
            !parsed.diffPaths.every(function (path) {
                return typeof path === 'string';
            })
        ) {
            return jsonResponse(400, { error: 'the body is git facts: {shaExists, baseIsAncestor, baseOnMain, diffPaths}' });
        }
        if (parsed.revertOf !== undefined && parsed.revertOf !== null && (typeof parsed.revertOf !== 'string' || !shaPattern.test(parsed.revertOf))) {
            return jsonResponse(400, { error: 'revertOf is the main commit the change reverts, or null' });
        }
        if (parsed.historyPaths !== undefined && !(Array.isArray(parsed.historyPaths) && parsed.historyPaths.every((path) => typeof path === 'string'))) {
            return jsonResponse(400, { error: 'historyPaths is every path a non-merge commit in base..sha touches' });
        }
        const isNamed = function (item: unknown): item is { path: string; users: string[] } {
            return isPlainObject(item) && typeof item.path === 'string' && Array.isArray(item.users) && item.users.every((user) => typeof user === 'string');
        };
        if (parsed.gateNamed !== undefined && !(Array.isArray(parsed.gateNamed) && parsed.gateNamed.every(isNamed))) {
            return jsonResponse(400, { error: 'gateNamed is [{path, users}]: each Python test a non-test file names' });
        }
        const facts: GitFacts = {
            shaExists: parsed.shaExists,
            baseIsAncestor: parsed.baseIsAncestor,
            baseOnMain: parsed.baseOnMain,
            diffPaths: [...(parsed.diffPaths as string[])].sort(),
            ...(typeof parsed.revertOf === 'string' ? { revertOf: parsed.revertOf } : {}),
            ...(Array.isArray(parsed.historyPaths) ? { historyPaths: [...(parsed.historyPaths as string[])].sort() } : {}),
            ...(Array.isArray(parsed.gateNamed) ? { gateNamed: parsed.gateNamed as { path: string; users: string[] }[] } : {}),
        };
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const entry = state.changes.get(change);
            if (entry === undefined) {
                return jsonResponse(404, { error: `no change ${change}` });
            }
            if (entry.checked || !isLive(entry)) {
                return jsonResponse(409, { error: `change ${change} is already ${entry.checked ? 'checked' : entry.state}` });
            }
            const reason = gitRefusalOf(entry.record, facts);
            if (reason !== null) {
                await this.append('change.refused', { change: change }, { facts: facts, reason: reason });
                await this.parkDependents(change);
                return jsonResponse(200, { change: change, state: 'refused', reason: reason });
            }
            await this.clear(entry.record, facts);
            return jsonResponse(200, { change: change, state: entry.state, future: entry.future, block: entry.block });
        });
    }

    // The future a change is tested in, while every change in it is still on its way; or why not.
    private liveFuture(state: QueueState, tree: string): FutureEntry | Response {
        const future = state.futures.get(tree);
        if (future === undefined) {
            return jsonResponse(404, { error: `no future ${tree}` });
        }
        const superseded = future.changes.slice(-1).find(function (change) {
            return state.changes.get(change)?.future !== tree;
        });
        if (superseded !== undefined) {
            return jsonResponse(409, { error: `change ${superseded} is tested in ${String(state.changes.get(superseded)?.future)} now, not ${tree}` });
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
            if (state.rules.outsideVerdicts.refused) {
                return jsonResponse(409, { error: 'whole verdicts are refused by rule outsideVerdicts: Judge decides every future' });
            }
            const entry = state.changes.get(checked.change);
            if (entry === undefined) {
                return jsonResponse(404, { error: `no change ${checked.change}` });
            }
            if (entry.future !== checked.verdict.future) {
                // Today's gate tested the change on a newer main: that merge becomes its future, the tree that lands,
                // while the future it replaces is still undecided (the builder's job, stubbed by today's gate).
                const replaced = state.futures.get(entry.future ?? '');
                const open = replaced !== undefined && replaced.units === null && (replaced.decided === null || replaced.decided.status === 'void');
                // A parity run is pinned to merge(base, sha), so nothing moves it to a newer main.
                if (checked.gateMerge === null || !open || !isLive(entry) || entry.record.parity === true) {
                    return jsonResponse(409, { error: `change ${checked.change} is tested in ${String(entry.future)}, not ${checked.verdict.future}` });
                }
                await this.append(
                    'future.built',
                    { change: checked.change, future: checked.verdict.future },
                    { base: checked.gateMerge.base, changes: [checked.change], gateMergeOf: entry.future },
                );
            }
            const future = this.liveFuture(state, checked.verdict.future);
            if (future instanceof Response) {
                return future;
            }
            if (future.units !== null) {
                return jsonResponse(409, { error: `future ${future.tree} is planned, so the judge decides it` });
            }
            // A run already decided is an answer, not a second event; a void lets the next run decide.
            if (future.decided !== null && (future.decided.run === checked.verdict.run || future.decided.status !== 'void')) {
                return jsonResponse(future.decided.run === checked.verdict.run ? 200 : 409, {
                    change: checked.change,
                    decided: future.decided,
                    error: future.decided.run === checked.verdict.run ? undefined : `future ${future.tree} was decided ${future.decided.status} by run ${future.decided.run}`,
                });
            }
            const subject = { change: checked.change, future: future.tree, run: checked.verdict.run };
            await this.append('verdict.decided', subject, { verdict: checked.verdict });
            if (checked.verdict.status === 'failed' && checked.verdict.cause === 'change') {
                await this.append('change.red', { change: checked.change, future: future.tree }, { verdict: checked.verdict });
                await this.parkDependents(checked.change);
            }
            await this.decideBlock(entry.block);
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
        if (!Array.isArray(units)) {
            return this.planEmpty(tree, units.reason);
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
            // A witness runs every unit uncached: nothing it plans is reused.
            if (state.changes.get(future.changes[0] ?? '')?.record.witness === true && units.some((unit) => unit.decision !== 'run')) {
                return jsonResponse(422, { error: 'a witness runs every unit uncached, none reused' });
            }
            // A parity run with the box record's selection runs exactly those packages, uncached.
            const select = state.changes.get(future.changes[0] ?? '')?.record.select;
            if (select !== undefined) {
                // A phase unit (vet, build, smoke) belongs to no selected package, so only test units are compared.
                const planned = units
                    .filter(function (unit) {
                        return unit.keyParts.kind !== 'phase';
                    })
                    .map(function (unit) {
                        return String(unit.keyParts.package);
                    });
                if (!sortedEqual([...new Set(planned)], select.packages) || units.some((unit) => unit.decision !== 'run')) {
                    return jsonResponse(422, { error: `a parity plan runs exactly the ${select.packages.length} selected packages, every unit run, none reused` });
                }
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
                    {
                        name: unit.name,
                        keyParts: unit.keyParts,
                        decision: unit.decision,
                        reason: unit.reason,
                        reused: unit.reused,
                        verdict: reused,
                        ...(unit.resources === null ? {} : { resources: unit.resources }),
                        ...(unit.tree === null ? {} : { tree: unit.tree }),
                    },
                );
            }
            return jsonResponse(200, { future: tree, planned: units.length });
        });
    }

    // The planner's empty plan (#p74thc8): no unit's key moved. The queue locks it a second time, on the paths: every path
    // of every change in the future is Markdown, and no parity run or witness, which plan their units, ever takes one.
    private async planEmpty(tree: string, reason: string): Promise<Response> {
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const future = this.liveFuture(state, tree);
            if (future instanceof Response) {
                return future;
            }
            if (future.units !== null) {
                return jsonResponse(future.empty !== null ? 200 : 409, future.empty !== null ? { future: tree, planned: 0 } : { error: `future ${tree} is already planned` });
            }
            if (future.decided !== null) {
                return jsonResponse(409, { error: `future ${tree} was decided ${future.decided.status} by today's gate, run ${future.decided.run}` });
            }
            const records = future.changes.map(function (change) {
                return state.changes.get(change)?.record;
            });
            if (records.some((record) => record === undefined || record.parity === true || record.witness === true)) {
                return jsonResponse(422, { error: 'a parity run or a witness plans its units' });
            }
            const other = records.flatMap((record) => record?.paths ?? []).filter((path) => !path.endsWith('.md'));
            if (other.length > 0 || records.every((record) => (record?.paths.length ?? 0) === 0)) {
                return jsonResponse(422, { error: `an empty plan is only for a future whose every path is Markdown, not ${other.slice(0, 5).join(' ') || 'no paths'}` });
            }
            await this.append('future.planned', { change: future.changes[0], future: tree }, { empty: true, reason: reason });
            return jsonResponse(200, { future: tree, planned: 0 });
        });
    }

    // Withdraws a future's plan by a ruling ({by, reason}), so the planner plans it again: only while nothing has judged
    // it, so no verdict ever stands on a plan that was withdrawn (Loom, Oct 10 00:3xZ, parity proof 1's replan).
    private async unplan(request: Request, tree: string): Promise<Response> {
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        const parsed = parseJson(body ?? '');
        if (!isPlainObject(parsed) || typeof parsed.by !== 'string' || parsed.by === '' || typeof parsed.reason !== 'string' || parsed.reason === '') {
            return jsonResponse(400, { error: 'the body is {by, reason}: who withdraws the plan and the ruling it follows' });
        }
        return this.ctx.blockConcurrencyWhile(async () => {
            const state = await this.current();
            const future = this.liveFuture(state, tree);
            if (future instanceof Response) {
                return future;
            }
            if (future.units === null) {
                return jsonResponse(409, { error: `future ${tree} has no plan to withdraw` });
            }
            if (future.judged || future.decided !== null) {
                return jsonResponse(409, { error: `future ${tree} has verdicts, so its plan stands` });
            }
            await this.append('future.unplanned', { change: future.changes[0], future: tree }, { by: parsed.by, reason: parsed.reason });
            return jsonResponse(200, { future: tree, planned: false });
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
            if (existing?.decided !== null && existing?.decided !== undefined && (existing.decided.run === batch.run || existing.decided.status !== 'void')) {
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
            if (future.empty !== null && batch.rule !== DocsRule) {
                return jsonResponse(422, { error: `an empty-planned future is decided only by ${DocsRule}, not ${batch.rule}` });
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
                await this.parkDependents(batch.change);
            }
            await this.decideBlock(state.changes.get(batch.change)?.block ?? null);
            await this.witnessMain(batch.change, decision);
            return jsonResponse(200, { future: tree, decided: decision.status, landable: futureLandable(future) });
        });
    }

    // Main's red pause (#3ypyka5, push-main l.25-31): a decided witness of main that's newer than every other decided one
    // holds main red when it's red (the judge's red leaves out what it quarantined), and clears the hold when it's green.
    private async witnessMain(change: string, decision: Decision): Promise<void> {
        const state = await this.current();
        const entry = state.changes.get(change);
        if (entry?.record.witness !== true || decision.status === 'void') {
            return;
        }
        const newer = [...state.changes.values()].some(function (other) {
            const decided = state.futures.get(other.future ?? '')?.decided;
            return other.record.witness === true && other.position > entry.position && decided !== null && decided !== undefined && decided.status !== 'void';
        });
        if (newer) {
            return;
        }
        const future = state.futures.get(entry.future ?? '');
        if (decision.status === 'red') {
            const units = decision.red.map(function (key) {
                return future?.units?.get(key)?.name ?? key;
            });
            await this.append('main.red', { change: change }, { witness: change, main: entry.record.sha, units: units });
        }
        else if (state.mainRed !== null) {
            await this.append('main.green', { change: change }, { witness: change, main: entry.record.sha, cleared: state.mainRed.witness });
        }
    }

    // Every change on its way that stacks on `base`, however deep, parked with the reason (#05b5c2f): a dependent's
    // sha carries its base's commits, so it can't land while its base can't. Restacking it on the base's fix is next.
    private async parkDependents(base: string): Promise<void> {
        const state = await this.current();
        const baseState = state.changes.get(base)?.state ?? 'gone';
        for (const [change, entry] of [...state.changes]) {
            let parent = entry.record.parent;
            while (parent !== null && parent !== base) {
                parent = state.changes.get(parent)?.record.parent ?? null;
            }
            if (parent === base && isLive(entry)) {
                await this.append('change.parked', { change: change, future: entry.future ?? undefined }, { reason: `its base ${base} is ${baseState}`, base: base });
            }
        }
    }

    // The landing orders the pusher pulls: every change whose future may land, in line order. A change that stacks on
    // another lands only after its base has.
    private async landings(): Promise<Response> {
        const state = await this.current();
        const orders = state.line.flatMap(function (change) {
            const entry = state.changes.get(change);
            if (entry === undefined || !isLive(entry) || entry.future === null || !futureLandable(state.futures.get(entry.future))) {
                return [];
            }
            if (entry.record.parent !== null && state.changes.get(entry.record.parent)?.state !== 'landed') {
                return [];
            }
            // A parity run is tested, never landed: no landing order is ever written for one.
            if (entry.record.parity === true) {
                return [];
            }
            // While main is red, only a fix-forward naming that red main or a revert lands; the rest wait, green.
            if (state.mainRed !== null && entry.record.fixesRed !== state.mainRed.main && entry.revertOf === null) {
                return [];
            }
            // In a block, only the decided block's longest green prefix lands, once, carrying every change ahead of it.
            if (entry.block !== null) {
                const block = state.blocks.get(entry.block);
                if (block === undefined || !block.resolved || block.landing !== change) {
                    return [];
                }
            }
            const future = state.futures.get(entry.future);
            return [{ change: change, future: entry.future, base: future?.base ?? entry.record.base, owner: entry.record.owner, run: future?.decided?.run ?? '' }];
        });
        return jsonResponse(200, { landings: orders });
    }

    // What the pusher did with a landing order: {main, from, landed} when it moved main from `from` to `main` and the
    // commit it landed is exactly the change's future (today's push script lands it as main's second parent, its tree
    // the gated tree), or {refused, main} when the push was refused. Until restacking (#05b5c2f), a refused change is
    // parked and its owner resubmits on main.
    private async reportLanding(request: Request, change: string): Promise<Response> {
        const body = await readBodyText(request, MaximumChangeBodyBytes);
        const parsed = parseJson(body ?? '');
        if (!isPlainObject(parsed) || typeof parsed.main !== 'string' || !shaPattern.test(parsed.main)) {
            return jsonResponse(400, { error: 'the body is {main, from, landed} after a push, or {refused, main} after a refusal' });
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
            // A change parked by a push the pusher misread (it landed, and a retry said main already holds it) still takes
            // the landing report: git, through the pusher, is the record of what main holds.
            const misread = entry.state === 'parked' && typeof parsed.landed === 'string';
            if ((!isLive(entry) && !misread) || entry.future === null || entry.record.parity === true || !futureLandable(state.futures.get(entry.future))) {
                return jsonResponse(409, { error: `change ${change} has no landing order${entry.record.parity === true ? ': it is a parity run' : ''}` });
            }
            if (typeof parsed.refused === 'string' && parsed.refused !== '' && !misread) {
                await this.append(
                    'change.parked',
                    { change: change, future: entry.future },
                    { reason: `the push of ${entry.future} onto main ${parsed.main} was refused: ${parsed.refused}. Resubmit on main.`, main: parsed.main },
                );
                await this.parkDependents(change);
                await this.openBlock();
                return jsonResponse(200, { change: change, state: 'parked' });
            }
            if (parsed.landed !== entry.future) {
                return jsonResponse(409, { error: `the push landed ${String(parsed.landed)}, not ${entry.future}, the tree that was tested` });
            }
            if (typeof parsed.from !== 'string' || !shaPattern.test(parsed.from) || parsed.from === parsed.main) {
                return jsonResponse(400, { error: 'from is the main the push moved from, 40 lowercase hex digits, not the new main' });
            }
            // A block's prefix carries every change ahead of it: they land together, the landing change last.
            const landedFuture = state.futures.get(entry.future);
            const carried = entry.block === null ? [] : (landedFuture?.changes ?? []).filter((member) => member !== change && isLive(state.changes.get(member)));
            for (const member of carried) {
                await this.append('change.landed', { change: member, future: entry.future }, { main: parsed.main, from: parsed.from, landed: parsed.landed });
            }
            await this.append('change.landed', { change: change, future: entry.future }, { main: parsed.main, from: parsed.from, landed: parsed.landed });
            // A block's landing frees its slot: the next block opens on the main it made.
            await this.openBlock();
            return jsonResponse(200, { change: change, state: 'landed', landed: parsed.main });
        });
    }

    // The log's head and main as the lander last reported it, for the replay proof (#ey1ay4f): replaying GET /log from
    // genesis must reach exactly this seq and head.
    private async readHead(): Promise<Response> {
        const state = await this.current();
        return jsonResponse(200, { seq: state.seq, head: state.head, landedMain: state.landedMain, mainRed: state.mainRed });
    }

    private async readVerdict(unitKey: string): Promise<Response> {
        const indexed = (await this.current()).verdicts.get(unitKey);
        if (indexed === undefined) {
            return jsonResponse(404, { error: `no verdict for unit ${unitKey}` });
        }
        return jsonResponse(200, indexed.record);
    }

    // The futures waiting for the planner: no plan and no verdict yet, every change in them on its way.
    // The futures waiting for the planner (state=unplanned: no plan, no verdict) or for the judge (state=planned: a
    // plan and no decision but void), oldest first, every change in them on its way and tested in them.
    private async listFutures(url: URL): Promise<Response> {
        const wanted = url.searchParams.get('state');
        if (wanted !== 'unplanned' && wanted !== 'planned') {
            return jsonResponse(400, { error: 'state is unplanned (for the planner) or planned (for the judge)' });
        }
        const state = await this.current();
        // Current: every change in it on its way, and the newest tested in it (a block prefix carries the changes ahead).
        const current = [...state.futures.values()].filter(function (future) {
            return (
                future.changes.every(function (change) {
                    return isLive(state.changes.get(change));
                }) && state.changes.get(future.changes[future.changes.length - 1] ?? '')?.future === future.tree
            );
        });
        if (wanted === 'unplanned') {
            const futures = current
                .filter(function (future) {
                    return future.units === null && future.decided === null;
                })
                .map(function (future) {
                    const select = state.changes.get(future.changes[0] ?? '')?.record.select;
                    return {
                        future: future.tree,
                        tree: future.tree,
                        base: future.base,
                        changes: future.changes,
                        parity: parityOf(state, future),
                        ...(select === undefined ? {} : { select: select }),
                        ...(state.changes.get(future.changes[0] ?? '')?.record.witness === true ? { witness: true, uncached: true } : {}),
                    };
                });
            return jsonResponse(200, { futures: futures });
        }
        const futures = current
            .filter(function (future) {
                return future.units !== null && (future.decided === null || future.decided.status === 'void');
            })
            .map(function (future) {
                const record = state.changes.get(future.changes[future.changes.length - 1] ?? '')?.record;
                return {
                    future: future.tree,
                    base: future.base,
                    parity: parityOf(state, future),
                    attempt: future.voids + 1,
                    ...(future.empty === null ? {} : { empty: true, reason: future.empty.reason, rule: DocsRule }),
                    change: { change: record?.change, sha: record?.sha, base: record?.base, owner: record?.owner },
                    units: [...(future.units?.values() ?? [])].map(function (unit) {
                        return {
                            unitKey: unit.unitKey,
                            name: unit.name,
                            keyParts: unit.keyParts,
                            decision: unit.decision,
                            reused: unit.reused,
                            ...(unit.resources === null ? {} : { resources: unit.resources }),
                            ...(unit.tree === null ? {} : { tree: unit.tree }),
                        };
                    }),
                };
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
