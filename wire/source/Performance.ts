// Loom's performance (#system_adamic_loom_performance): a stage timeline for every candidate, from its branch's submit
// to its verdict or its landing, computed by rule from what the Queue's log and each run's events say, never estimated,
// and kept on Cloudflare. One Durable Object, named `performance`, keeps facts, never conclusions: when the log said
// each thing of a branch, a candidate and its runs, and when each unit's events said it started, was ready, exited and
// finished. A timeline is a pure function of those facts (timelineOf), computed when it is read, so a better rule
// reads all of history again with no migration.
//
// It fills on an alarm every 30 seconds, armed by its first read. Each round it follows the Queue's log through the
// Queue binding from its cursor, a page at a time, as Headline does; learns each planned candidate's current run from
// the Queue's own GET /futures (the attempt is the Queue's rule, never worked out again here) and each decided run from
// its verdict; and follows each open run's events through the runs Worker (loom-runs), with a coordinator token minted
// here for that one run that lasts five minutes. A run's events are read until a short page, and a read that brings
// nothing is cut off rather than sitting in the runs Worker's 20-second wait. A run is closed once its verdict is in
// and its events are read through. It writes nothing anywhere but its own storage.

import { DurableObject } from 'cloudflare:workers';
import { queueOf } from './Changes';
import { readEvent } from './Headline';
import { jsonResponse } from './Http';
import { MaximumEventsPage, type QueueEvent } from './Queue';
import { mintToken } from './Token';

export const PerformanceName = 'performance';
export const FollowMilliseconds = 30 * 1000;
// The runs Worker's page of a run's events (RunObject.ts's EventsPageSize): a shorter page is the end, for now.
export const RunEventsPage = 1000;
// A run's events read brings what is there at once; one that brings nothing waits in the runs Worker, so it's cut here.
const runReadMilliseconds = 5 * 1000;
const runTokenSeconds = 5 * 60;
// A round follows at most this many pages of the log and this many runs at once.
const maximumLogPagesPerRound = 20;
const maximumRunsAtOnce = 8;
// The runner's line when a prebuilt unit is ready (runner/prebuilt.go): its seconds since started, fetch and unpack.
const readyLinePattern = /^loom-runner: ready in ([\d.]+) s: fetched in ([\d.]+) s, unpacked in ([\d.]+) s/;

// The stages in a candidate's order. ready is inside tests, so it is told with tests (each unit's ready and test
// time), never as its own bar.
export type StageName = 'admission' | 'block' | 'planning' | 'tree' | 'tests' | 'judge' | 'land' | 'wait';

export interface BranchFacts {
    branch: string;
    owner: string;
    sha: string;
    witness: boolean;
    submittedAt: number;
    checkedAt: number | null;
    landedAt: number | null;
    witnessedAt: number | null;
}

export interface UnitFacts {
    unit: string;
    name: string | null;
    kind: string | null;
    machine: string | null;
    firstStartedAt: number | null;
    startedAt: number | null;
    readyAt: number | null;
    readySeconds: number | null;
    fetchSeconds: number | null;
    unpackSeconds: number | null;
    prepareSeconds: number | null;
    testSeconds: number | null;
    exitAt: number | null;
    wallSeconds: number | null;
    finishedAt: number | null;
    status: string | null;
}

export interface RunFacts {
    run: string;
    decidedAt: number | null;
    status: string | null;
    units: UnitFacts[];
}

export interface CandidateFacts {
    candidate: string;
    // Its newest branch, the one it tests on top of the ones before it in its block.
    branch: BranchFacts;
    branches: string[];
    builtAt: number;
    block: number | null;
    // Each plan: when the planner's units were logged, how many, and how many reused a verdict.
    plans: { at: number; units: number; reused: number }[];
    // When a plan was withdrawn, or the candidate built again (a re-witness): planning starts again from there.
    replans: { at: number; why: string }[];
    runs: RunFacts[];
}

export interface Spread {
    units: number;
    p50: number;
    max: number;
}

export interface Stage {
    stage: StageName;
    start: string;
    end: string | null;
    seconds: number;
    // Open: no end yet, and seconds is its age at the reading.
    open: boolean;
    source: string;
    note?: string;
}

export interface RunTimeline {
    run: string;
    status: string | null;
    decidedAt: string | null;
    plannedAt: string | null;
    units: { planned: number; reused: number; started: number; ready: number; finished: number; passed: number; failed: number; broken: number };
    // Each unit's ready (started to ready) and its tests (ready to exit), across the units that have them.
    ready: Spread | null;
    test: Spread | null;
    stages: Stage[];
    start: string;
    end: string | null;
    seconds: number;
}

export interface CandidateTimeline {
    candidate: string;
    branch: string;
    owner: string;
    witness: boolean;
    submittedAt: string;
    runs: RunTimeline[];
}

export function performanceOf(environment: Env): DurableObjectStub {
    const namespace = (environment as unknown as { Performance: DurableObjectNamespace }).Performance;
    return namespace.get(namespace.idFromName(PerformanceName));
}

function iso(milliseconds: number): string {
    return new Date(milliseconds).toISOString();
}

// The nearest-rank percentile of a list, so every figure is one a unit really had.
export function percentile(values: number[], fraction: number): number {
    const sorted = [...values].sort(function (left, right) {
        return left - right;
    });
    return sorted[Math.max(0, Math.ceil(fraction * sorted.length) - 1)] ?? 0;
}

function spreadOf(values: number[]): Spread | null {
    if (values.length === 0) {
        return null;
    }
    return { units: values.length, p50: percentile(values, 0.5), max: Math.max(...values) };
}

function earliest(values: (number | null)[]): number | null {
    const present = values.filter((value): value is number => value !== null);
    return present.length === 0 ? null : Math.min(...present);
}

function latest(values: (number | null)[]): number | null {
    const present = values.filter((value): value is number => value !== null);
    return present.length === 0 ? null : Math.max(...present);
}

// A unit's ready seconds: the runner's own, else its ready line's time after its last start.
function readySecondsOf(unit: UnitFacts): number | null {
    if (unit.readySeconds !== null) {
        return unit.readySeconds;
    }
    return unit.readyAt !== null && unit.startedAt !== null ? (unit.readyAt - unit.startedAt) / 1000 : null;
}

// A unit's test seconds: the runner's timing when it sent one, else from its ready line to its exit.
function testSecondsOf(unit: UnitFacts): number | null {
    if (unit.testSeconds !== null) {
        return unit.testSeconds;
    }
    return unit.exitAt !== null && unit.readyAt !== null ? (unit.exitAt - unit.readyAt) / 1000 : null;
}

// Every run's timeline, by rule. A run's waterfall begins where the candidate's run before it was decided, or at its
// branch's submit for the first, and runs on stage by stage; a span between two stages that nothing accounts for is a
// wait, named, so no time goes missing. A stage with no end yet is open, its age at `now`.
export function timelineOf(facts: CandidateFacts, now: number): CandidateTimeline {
    const plans = [...facts.plans].sort((left, right) => left.at - right.at);
    const runs = [...facts.runs].sort(function (left, right) {
        return attemptOf(left.run) - attemptOf(right.run);
    });
    const timelines: RunTimeline[] = [];
    let previousEnd: number | null = null;
    for (const run of runs) {
        const stages: Stage[] = [];
        const add = function (stage: StageName, start: number, end: number | null, source: string, note?: string): void {
            const last = stages[stages.length - 1];
            const lastEnd = last === undefined ? null : last.end === null ? null : Date.parse(last.end);
            if (lastEnd !== null && start > lastEnd) {
                stages.push({ stage: 'wait', start: iso(lastEnd), end: iso(start), seconds: (start - lastEnd) / 1000, open: false, source: 'none', note: 'nothing in the log or the run says what happened' });
            }
            stages.push({
                stage: stage,
                start: iso(start),
                end: end === null ? null : iso(end),
                seconds: ((end ?? now) - start) / 1000,
                open: end === null,
                source: source,
                ...(note === undefined ? {} : { note: note }),
            });
        };
        const firstStarted = earliest(run.units.map((unit) => unit.firstStartedAt));
        // This run's plan: the newest logged before its first unit started (or its verdict, or now).
        const before = firstStarted ?? run.decidedAt ?? now;
        const plan = plans.filter((candidatePlan) => candidatePlan.at <= before).pop() ?? null;
        let cursor: number;
        if (previousEnd === null) {
            const checked = facts.branch.checkedAt;
            add('admission', facts.branch.submittedAt, checked ?? facts.builtAt, 'log', "the bridge's git facts");
            if (checked !== null) {
                add('block', checked, facts.builtAt, 'log', facts.block === null ? undefined : `block ${facts.block}`);
            }
            cursor = facts.builtAt;
        }
        else {
            cursor = previousEnd;
        }
        if (plan !== null && plan.at >= cursor) {
            const replan = facts.replans.filter((entry) => entry.at <= plan.at && entry.at >= cursor).pop();
            if (replan !== undefined && replan.at > cursor) {
                add('wait', cursor, replan.at, 'log', `until the plan was ${replan.why}`);
            }
            add('planning', replan?.at ?? cursor, plan.at, 'log', `${plan.units} units, ${plan.reused} reused`);
            cursor = plan.at;
        }
        const toRun = plan === null ? null : plan.units - plan.reused;
        const finished = run.units.filter((unit) => unit.finishedAt !== null);
        const lastFinished = latest(run.units.map((unit) => unit.finishedAt));
        // Tests end when every unit the plan runs has finished; until then, and with a verdict in, they ended at the last.
        const testsDone = (toRun !== null && finished.length >= toRun) || run.decidedAt !== null;
        if (firstStarted !== null) {
            add('tree', cursor, firstStarted, 'log, run events', "Workshop's tree build and the placer, not yet told apart");
            add('tests', firstStarted, testsDone ? (lastFinished ?? run.decidedAt) : null, 'run events', `${finished.length} of ${toRun ?? run.units.length} units finished`);
            cursor = lastFinished ?? firstStarted;
        }
        if (testsDone) {
            add('judge', cursor, run.decidedAt, run.decidedAt === null ? 'run events' : 'run events, log', firstStarted === null ? 'no unit started' : undefined);
        }
        else if (firstStarted === null) {
            add('tree', cursor, null, 'log', 'no unit has started');
        }
        if (run.decidedAt !== null && (run.status === 'green' || run.status === 'passed')) {
            const done = facts.branch.witness ? facts.branch.witnessedAt : facts.branch.landedAt;
            add('land', run.decidedAt, done, 'log', facts.branch.witness ? 'a witness of main finishes witnessed, never landed' : undefined);
        }
        const last = stages[stages.length - 1];
        const start = Date.parse(stages[0]?.start ?? iso(cursor));
        const end = last === undefined || last.end === null ? null : Date.parse(last.end);
        const ready = run.units.flatMap((unit) => {
            const seconds = readySecondsOf(unit);
            return seconds === null ? [] : [seconds];
        });
        const test = run.units.flatMap((unit) => {
            const seconds = testSecondsOf(unit);
            return seconds === null ? [] : [seconds];
        });
        timelines.push({
            run: run.run,
            status: run.status,
            decidedAt: run.decidedAt === null ? null : iso(run.decidedAt),
            plannedAt: plan === null ? null : iso(plan.at),
            units: {
                planned: plan?.units ?? 0,
                reused: plan?.reused ?? 0,
                started: run.units.filter((unit) => unit.firstStartedAt !== null).length,
                ready: run.units.filter((unit) => unit.readyAt !== null).length,
                finished: finished.length,
                passed: finished.filter((unit) => unit.status === 'passed').length,
                failed: finished.filter((unit) => unit.status === 'failed').length,
                broken: finished.filter((unit) => unit.status === 'broken').length,
            },
            ready: spreadOf(ready),
            test: spreadOf(test),
            stages: stages,
            start: iso(start),
            end: end === null ? null : iso(end),
            seconds: ((end ?? now) - start) / 1000,
        });
        previousEnd = run.decidedAt ?? end ?? previousEnd;
    }
    return {
        candidate: facts.candidate,
        branch: facts.branch.branch,
        owner: facts.branch.owner,
        witness: facts.branch.witness,
        submittedAt: iso(facts.branch.submittedAt),
        runs: timelines,
    };
}

// A run id's attempt, future-<tree>-<attempt> (docs/queue.md), or 0 for one that isn't.
export function attemptOf(run: string): number {
    const match = /-(\d+)$/.exec(run);
    return match === null ? 0 : Number(match[1]);
}

export class Performance extends DurableObject<Env> {
    private readonly sql: SqlStorage;
    // The runs Worker, through its service binding; a test puts its own in its place.
    runs: Fetcher | null;
    // The runs Worker's page; a test shortens it with its own runs Worker.
    runEventsPage = RunEventsPage;
    private lastRoundAt = 0;
    private round: Promise<void> | null = null;

    constructor(context: DurableObjectState, environment: Env) {
        super(context, environment);
        this.sql = context.storage.sql;
        this.runs = (environment as unknown as { LoomRuns?: Fetcher }).LoomRuns ?? null;
        this.sql.exec(`
            CREATE TABLE IF NOT EXISTS cursor (id INTEGER PRIMARY KEY CHECK (id = 1), seq INTEGER NOT NULL);
            CREATE TABLE IF NOT EXISTS branches (branch TEXT PRIMARY KEY, owner TEXT NOT NULL, sha TEXT NOT NULL, witness INTEGER NOT NULL,
                submittedAt INTEGER NOT NULL, checkedAt INTEGER, landedAt INTEGER, witnessedAt INTEGER);
            CREATE TABLE IF NOT EXISTS candidates (candidate TEXT PRIMARY KEY, branch TEXT NOT NULL, branches TEXT NOT NULL, builtAt INTEGER NOT NULL, block INTEGER);
            CREATE INDEX IF NOT EXISTS candidatesBuilt ON candidates (builtAt);
            CREATE INDEX IF NOT EXISTS candidatesBranch ON candidates (branch);
            CREATE TABLE IF NOT EXISTS plans (candidate TEXT NOT NULL, at INTEGER NOT NULL, units INTEGER NOT NULL, reused INTEGER NOT NULL, PRIMARY KEY (candidate, at));
            CREATE TABLE IF NOT EXISTS replans (candidate TEXT NOT NULL, at INTEGER NOT NULL, why TEXT NOT NULL, PRIMARY KEY (candidate, at));
            CREATE TABLE IF NOT EXISTS planned (candidate TEXT NOT NULL, unit TEXT NOT NULL, name TEXT NOT NULL, kind TEXT, PRIMARY KEY (candidate, unit));
            CREATE TABLE IF NOT EXISTS runs (run TEXT PRIMARY KEY, candidate TEXT NOT NULL, decidedAt INTEGER, status TEXT,
                position INTEGER NOT NULL DEFAULT 0, closed INTEGER NOT NULL DEFAULT 0);
            CREATE INDEX IF NOT EXISTS runsCandidate ON runs (candidate);
            CREATE TABLE IF NOT EXISTS units (run TEXT NOT NULL, unit TEXT NOT NULL, machine TEXT, firstStartedAt INTEGER, startedAt INTEGER,
                readyAt INTEGER, readySeconds REAL, fetchSeconds REAL, unpackSeconds REAL, prepareSeconds REAL, testSeconds REAL,
                exitAt INTEGER, wallSeconds REAL, finishedAt INTEGER, status TEXT, PRIMARY KEY (run, unit));
        `);
    }

    override async fetch(request: Request): Promise<Response> {
        if (request.method !== 'GET') {
            return jsonResponse(405, { error: 'use GET' }, { Allow: 'GET' });
        }
        // The first read arms the follow; it keeps itself going from then on.
        if ((await this.ctx.storage.getAlarm()) === null) {
            await this.ctx.storage.setAlarm(Date.now());
        }
        const url = new URL(request.url);
        const reading = { readAt: this.lastRoundAt === 0 ? null : iso(this.lastRoundAt), seq: this.cursor() };
        const candidateMatch = /^\/candidates\/([0-9a-f]{40})$/.exec(url.pathname);
        if (candidateMatch !== null) {
            const facts = this.facts(candidateMatch[1] ?? '');
            if (facts === null) {
                return jsonResponse(404, { error: 'no candidate with that tree in the log read so far', ...reading });
            }
            return jsonResponse(200, { ...reading, timeline: timelineOf(facts, Date.now()) });
        }
        if (url.pathname === '/candidates') {
            const day = url.searchParams.get('day') ?? iso(Date.now()).slice(0, 10);
            if (!/^\d{4}-\d{2}-\d{2}$/.test(day) || Number.isNaN(Date.parse(`${day}T00:00:00Z`))) {
                return jsonResponse(400, { error: 'day is YYYY-MM-DD, in UTC' });
            }
            const branch = url.searchParams.get('branch');
            const from = Date.parse(`${day}T00:00:00Z`);
            const rows = this.sql
                .exec<{ candidate: string }>(
                    'SELECT candidate FROM candidates WHERE builtAt >= ? AND builtAt < ? AND (? IS NULL OR branch = ?) ORDER BY builtAt',
                    from,
                    from + 24 * 60 * 60 * 1000,
                    branch,
                    branch,
                )
                .toArray();
            const now = Date.now();
            const candidates = rows.flatMap((row) => {
                const facts = this.facts(row.candidate);
                return facts === null ? [] : [summaryOf(timelineOf(facts, now))];
            });
            return jsonResponse(200, { ...reading, day: day, candidates: candidates });
        }
        return jsonResponse(404, { error: 'no such performance read' });
    }

    override async alarm(): Promise<void> {
        try {
            await this.follow();
        }
        finally {
            await this.ctx.storage.setAlarm(Date.now() + FollowMilliseconds);
        }
    }

    // One round: the log, then the planned runs, then every open run's events. Concurrent callers share one round.
    follow(): Promise<void> {
        this.round ??= this.followOnce().finally(() => {
            this.round = null;
        });
        return this.round;
    }

    private async followOnce(): Promise<void> {
        await this.followLog();
        await this.learnPlannedRuns();
        const open = this.sql.exec<{ run: string; position: number; decided: number }>('SELECT run, position, decidedAt IS NOT NULL AS decided FROM runs WHERE closed = 0').toArray();
        for (let index = 0; index < open.length; index += maximumRunsAtOnce) {
            await Promise.all(
                open.slice(index, index + maximumRunsAtOnce).map((run) => {
                    return this.followRun(run.run, run.position, run.decided === 1).catch(function (error: unknown) {
                        console.error(`performance: run ${run.run}'s events couldn't be read: ${error instanceof Error ? error.message : String(error)}`);
                    });
                }),
            );
        }
        this.lastRoundAt = Date.now();
    }

    private cursor(): number {
        return this.sql.exec<{ seq: number }>('SELECT seq FROM cursor WHERE id = 1').toArray()[0]?.seq ?? 0;
    }

    private async followLog(): Promise<void> {
        const queue = queueOf(this.env);
        if (queue === null) {
            throw new Error("the queue isn't on the wire");
        }
        for (let page = 0; page < maximumLogPagesPerRound; page++) {
            const response = await queue.fetch(new Request(`https://queue/log?after=${this.cursor()}`));
            if (!response.ok) {
                await response.body?.cancel();
                throw new Error(`the log answered ${response.status}`);
            }
            const lines = new TextDecoder().decode(await response.arrayBuffer()).split('\n').filter(function (line) {
                return line !== '';
            });
            if (lines.length === 0) {
                break;
            }
            this.ctx.storage.transactionSync(() => {
                let seq = this.cursor();
                for (const line of lines) {
                    const event = readEvent(line);
                    if (event === null) {
                        console.error(`performance: skipped a log line it can't read, after seq ${seq}: ${line.slice(0, 200)}`);
                        continue;
                    }
                    this.keepLogFact(event);
                    seq = event.seq;
                }
                this.sql.exec('INSERT OR REPLACE INTO cursor (id, seq) VALUES (1, ?)', seq);
            });
            if (lines.length < MaximumEventsPage) {
                break;
            }
        }
    }

    // What one log event says that a timeline reads, kept as a fact.
    private keepLogFact(event: QueueEvent): void {
        const at = Date.parse(event.at);
        const change = event.subject.change ?? null;
        const future = event.subject.future ?? null;
        if (event.type === 'change.submitted' && change !== null) {
            const record = (event.data.record ?? {}) as { owner?: unknown; sha?: unknown; witness?: unknown };
            this.sql.exec(
                'INSERT OR IGNORE INTO branches (branch, owner, sha, witness, submittedAt) VALUES (?, ?, ?, ?, ?)',
                change,
                String(record.owner ?? ''),
                String(record.sha ?? ''),
                record.witness === true ? 1 : 0,
                at,
            );
        }
        else if (event.type === 'change.checked' && change !== null) {
            this.sql.exec('UPDATE branches SET checkedAt = ? WHERE branch = ?', at, change);
        }
        else if (event.type === 'change.landed' && change !== null) {
            this.sql.exec('UPDATE branches SET landedAt = ? WHERE branch = ?', at, change);
        }
        else if (event.type === 'change.witnessed' && change !== null) {
            this.sql.exec('UPDATE branches SET witnessedAt = ? WHERE branch = ?', at, change);
        }
        else if (event.type === 'future.built' && future !== null) {
            const changes = Array.isArray(event.data.changes) ? event.data.changes.map(String) : change === null ? [] : [change];
            const block = typeof event.data.block === 'number' ? event.data.block : null;
            const known = this.sql.exec('SELECT 1 FROM candidates WHERE candidate = ?', future).toArray().length > 0;
            if (known) {
                // The same tree built again (a re-witness): planning starts over from here, and the first build stays.
                this.sql.exec('INSERT OR IGNORE INTO replans (candidate, at, why) VALUES (?, ?, ?)', future, at, 'built again');
            }
            else {
                this.sql.exec(
                    'INSERT INTO candidates (candidate, branch, branches, builtAt, block) VALUES (?, ?, ?, ?, ?)',
                    future,
                    change ?? changes[changes.length - 1] ?? '',
                    JSON.stringify(changes),
                    at,
                    block,
                );
            }
        }
        else if (event.type === 'unit.planned' && future !== null) {
            const reused = event.data.decision === 'reuse' ? 1 : 0;
            this.sql.exec(
                'INSERT INTO plans (candidate, at, units, reused) VALUES (?, ?, 1, ?) ON CONFLICT (candidate, at) DO UPDATE SET units = units + 1, reused = reused + excluded.reused',
                future,
                at,
                reused,
            );
            const keyParts = (event.data.keyParts ?? {}) as { kind?: unknown };
            this.sql.exec(
                'INSERT OR REPLACE INTO planned (candidate, unit, name, kind) VALUES (?, ?, ?, ?)',
                future,
                event.subject.unitKey ?? '',
                String(event.data.name ?? ''),
                typeof keyParts.kind === 'string' ? keyParts.kind : null,
            );
        }
        else if (event.type === 'future.planned' && future !== null) {
            this.sql.exec('INSERT OR IGNORE INTO plans (candidate, at, units, reused) VALUES (?, ?, 0, 0)', future, at);
        }
        else if (event.type === 'future.unplanned' && future !== null) {
            this.sql.exec('INSERT OR IGNORE INTO replans (candidate, at, why) VALUES (?, ?, ?)', future, at, 'withdrawn');
        }
        else if (event.type === 'verdict.decided' && future !== null && event.subject.unitKey === undefined) {
            // A judge's decision names its run in the subject; a whole verdict from today's gate, in the verdict.
            const decision = event.data.decision as { status?: unknown } | undefined;
            const verdict = event.data.verdict as { status?: unknown; run?: unknown } | undefined;
            const run = event.subject.run ?? (typeof verdict?.run === 'string' ? verdict.run : null);
            if (run === null) {
                return;
            }
            this.sql.exec(
                'INSERT INTO runs (run, candidate, decidedAt, status) VALUES (?, ?, ?, ?) ON CONFLICT (run) DO UPDATE SET decidedAt = excluded.decidedAt, status = excluded.status',
                run,
                future,
                at,
                String(decision?.status ?? verdict?.status ?? ''),
            );
        }
    }

    // The Queue's planned futures each name the attempt its judge runs now: that run is followed from here on.
    private async learnPlannedRuns(): Promise<void> {
        const queue = queueOf(this.env);
        if (queue === null) {
            return;
        }
        const response = await queue.fetch(new Request('https://queue/futures?state=planned'));
        if (!response.ok) {
            await response.body?.cancel();
            throw new Error(`the planned futures answered ${response.status}`);
        }
        const body = (await response.json()) as { futures?: { future?: unknown; attempt?: unknown }[] };
        for (const future of body.futures ?? []) {
            if (typeof future.future === 'string' && Number.isSafeInteger(future.attempt)) {
                this.sql.exec('INSERT OR IGNORE INTO runs (run, candidate) VALUES (?, ?)', `future-${future.future}-${String(future.attempt)}`, future.future);
            }
        }
    }

    // Reads a run's events from where it left off, page by page, until a short page; closes the run once its verdict
    // is in and the read came to the end.
    private async followRun(run: string, position: number, decided: boolean): Promise<void> {
        const runs = this.runs;
        const secret = this.env.LOOM_TOKEN_SECRET;
        if (runs === null || typeof secret !== 'string' || secret === '') {
            return;
        }
        const token = await mintToken(secret, { run: run, scope: 'coordinator', expires: Math.floor(Date.now() / 1000) + runTokenSeconds });
        let after = position;
        for (;;) {
            let text: string;
            try {
                const response = await runs.fetch(`https://runs.loom.system.inc/runs/${encodeURIComponent(run)}/events?after=${after}`, {
                    headers: { Authorization: `Bearer ${token}` },
                    signal: AbortSignal.timeout(runReadMilliseconds),
                });
                if (!response.ok) {
                    await response.body?.cancel();
                    throw new Error(`the runs Worker answered ${response.status}`);
                }
                text = await response.text();
            }
            catch (error) {
                // Nothing to say within the wait: the run's events are read through, for now.
                if (error instanceof Error && (error.name === 'TimeoutError' || error.name === 'AbortError')) {
                    text = '';
                }
                else {
                    throw error;
                }
            }
            const lines = text.split('\n').filter(function (line) {
                return line !== '';
            });
            this.ctx.storage.transactionSync(() => {
                for (const line of lines) {
                    const row = JSON.parse(line) as { position: number; event: Record<string, unknown> };
                    this.keepRunFact(run, row.event);
                    after = row.position;
                }
                this.sql.exec('UPDATE runs SET position = ? WHERE run = ?', after, run);
            });
            if (lines.length < this.runEventsPage) {
                break;
            }
        }
        if (decided) {
            this.sql.exec('UPDATE runs SET closed = 1 WHERE run = ?', run);
        }
    }

    // What one of a run's events says of its unit, kept as a fact.
    private keepRunFact(run: string, event: Record<string, unknown>): void {
        const unit = typeof event.unit === 'string' ? event.unit : null;
        const at = typeof event.time === 'string' ? Date.parse(event.time) : Number.NaN;
        if (unit === null || Number.isNaN(at)) {
            return;
        }
        this.sql.exec('INSERT OR IGNORE INTO units (run, unit) VALUES (?, ?)', run, unit);
        if (event.type === 'started') {
            // A unit started again (the runner swapping to the release its key names, or a box placing it again) is
            // ready from its last start; its first start is when its run reached it.
            this.sql.exec(
                'UPDATE units SET firstStartedAt = COALESCE(firstStartedAt, ?), startedAt = ?, machine = ? WHERE run = ? AND unit = ?',
                at,
                at,
                typeof event.machine === 'string' ? event.machine : null,
                run,
                unit,
            );
        }
        else if (event.type === 'output' && event.stream === 'runner' && typeof event.text === 'string') {
            // The ready line of the unit's last start; a timing event, which comes after it, overwrites its seconds.
            const match = readyLinePattern.exec(event.text);
            if (match !== null) {
                this.sql.exec(
                    'UPDATE units SET readyAt = ?, readySeconds = ?, fetchSeconds = ?, unpackSeconds = ? WHERE run = ? AND unit = ?',
                    at,
                    Number(match[1]),
                    Number(match[2]),
                    Number(match[3]),
                    run,
                    unit,
                );
            }
        }
        else if (event.type === 'timing' && typeof event.timing === 'object' && event.timing !== null) {
            // The runner's own timing, sent just before finished, wins over its ready line.
            const timing = event.timing as Record<string, unknown>;
            const seconds = function (field: string): number | null {
                return typeof timing[field] === 'number' ? timing[field] : null;
            };
            const fetch = seconds('fetchSeconds');
            const unpack = seconds('unpackSeconds');
            const prepare = seconds('prepareSeconds');
            const ready = fetch === null && unpack === null && prepare === null ? null : (fetch ?? 0) + (unpack ?? 0) + (prepare ?? 0);
            this.sql.exec(
                'UPDATE units SET fetchSeconds = COALESCE(?, fetchSeconds), unpackSeconds = COALESCE(?, unpackSeconds), prepareSeconds = ?, testSeconds = ?, readySeconds = COALESCE(?, readySeconds) WHERE run = ? AND unit = ?',
                fetch,
                unpack,
                prepare,
                seconds('testSeconds'),
                ready,
                run,
                unit,
            );
        }
        else if (event.type === 'exit') {
            this.sql.exec(
                'UPDATE units SET exitAt = ?, wallSeconds = ? WHERE run = ? AND unit = ?',
                at,
                typeof event.wallSeconds === 'number' ? event.wallSeconds : null,
                run,
                unit,
            );
        }
        else if (event.type === 'finished') {
            this.sql.exec('UPDATE units SET finishedAt = ?, status = ? WHERE run = ? AND unit = ?', at, typeof event.status === 'string' ? event.status : null, run, unit);
        }
    }

    // A candidate's facts, gathered from storage, or null when the log hasn't built it.
    private facts(candidate: string): CandidateFacts | null {
        const row = this.sql
            .exec<{ branch: string; branches: string; builtAt: number; block: number | null }>('SELECT branch, branches, builtAt, block FROM candidates WHERE candidate = ?', candidate)
            .toArray()[0];
        if (row === undefined) {
            return null;
        }
        const branch = this.sql
            .exec<{ branch: string; owner: string; sha: string; witness: number; submittedAt: number; checkedAt: number | null; landedAt: number | null; witnessedAt: number | null }>(
                'SELECT branch, owner, sha, witness, submittedAt, checkedAt, landedAt, witnessedAt FROM branches WHERE branch = ?',
                row.branch,
            )
            .toArray()[0];
        if (branch === undefined) {
            return null;
        }
        const names = new Map(
            this.sql
                .exec<{ unit: string; name: string; kind: string | null }>('SELECT unit, name, kind FROM planned WHERE candidate = ?', candidate)
                .toArray()
                .map((planned) => [planned.unit, planned] as const),
        );
        const runs = this.sql
            .exec<{ run: string; decidedAt: number | null; status: string | null }>('SELECT run, decidedAt, status FROM runs WHERE candidate = ?', candidate)
            .toArray()
            .map((run) => {
                const units = this.sql
                    .exec<Omit<UnitFacts, 'name' | 'kind'>>(
                        `SELECT unit, machine, firstStartedAt, startedAt, readyAt, readySeconds, fetchSeconds, unpackSeconds, prepareSeconds,
                            testSeconds, exitAt, wallSeconds, finishedAt, status FROM units WHERE run = ?`,
                        run.run,
                    )
                    .toArray()
                    .map((unit) => ({ ...unit, name: names.get(unit.unit)?.name ?? null, kind: names.get(unit.unit)?.kind ?? null }));
                return { run: run.run, decidedAt: run.decidedAt, status: run.status, units: units };
            });
        return {
            candidate: candidate,
            branch: { ...branch, witness: branch.witness === 1 },
            branches: JSON.parse(row.branches) as string[],
            builtAt: row.builtAt,
            block: row.block,
            plans: this.sql.exec<{ at: number; units: number; reused: number }>('SELECT at, units, reused FROM plans WHERE candidate = ? ORDER BY at', candidate).toArray(),
            replans: this.sql.exec<{ at: number; why: string }>('SELECT at, why FROM replans WHERE candidate = ? ORDER BY at', candidate).toArray(),
            runs: runs,
        };
    }
}

// One line of a day's list: a candidate's runs, each with its status and every stage's seconds.
export function summaryOf(timeline: CandidateTimeline): Record<string, unknown> {
    return {
        candidate: timeline.candidate,
        branch: timeline.branch,
        owner: timeline.owner,
        witness: timeline.witness,
        submittedAt: timeline.submittedAt,
        runs: timeline.runs.map(function (run) {
            return {
                run: run.run,
                status: run.status,
                seconds: run.seconds,
                open: run.end === null,
                stages: run.stages.map(function (stage) {
                    return { stage: stage.stage, seconds: stage.seconds, open: stage.open };
                }),
            };
        }),
    };
}
