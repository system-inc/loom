// One Durable Object per run. It holds the run's plan, its ordered event log, the events waiting for a gap to
// fill, the blobs the run has uploaded, and the verdict, all in the object's SQLite storage, and fans every
// accepted event out to the run's viewers over hibernating WebSockets. The Worker has already checked the
// token; this object trusts it, and decides which blobs that token's scope may reach. It also keeps each unit's
// state for the board, and pushes the run's summary there from its alarm, never on a request's way out.

import { DurableObject } from 'cloudflare:workers';
import { Sha256Pattern } from './Blobs';
import { BoardName, jobOfRun, type ActiveUnit, type FailedUnit, type RunSummary } from './Board';
import { canonicalJson, checkEventLine, MaximumUnitIdLength, type LoomEvent } from './Events';
import { jsonResponse, readBodyText } from './Http';
import { TokenScopes, type TokenScope } from './Token';

export const RunHeader = 'Loom-Run';
// The token's scope, for the blob operations; like RunHeader, only the Worker sets it.
export const ScopeHeader = 'Loom-Scope';
export const MaximumEventsBodyBytes = 16 * 1024 * 1024;
export const MaximumEventLineBytes = 1024 * 1024;
export const MaximumHeldEvents = 10_000;
export const MaximumPlannedUnits = 10_000;
// At most one push of the summary to the board per this many milliseconds, so a busy run feeds it twice a second.
export const BoardPushMilliseconds = 500;
export const BoardRetryMilliseconds = 2000;
export const UnitLines = 5;
export const BoardFailures = 20;

const textEncoder = new TextEncoder();

export interface Verdict {
    status: 'green' | 'red' | 'void';
    failed: string[];
    problems: string[];
    cached: string[];
}

export interface Plan {
    units: string[];
    inputs: string[];
}

interface EventLine {
    unit: string;
    sequence: number;
    line: string;
}

interface Tally {
    accepted: number;
    replays: number;
    conflicts: string[];
    frames: string[]; // released to the log, in order, for the viewers
}

class HeldLimitError extends Error {}

export class RunObject extends DurableObject<Env> {
    private readonly sql: SqlStorage;

    constructor(context: DurableObjectState, environment: Env) {
        super(context, environment);
        this.sql = context.storage.sql;
        this.sql.exec(`
            CREATE TABLE IF NOT EXISTS facts (name TEXT PRIMARY KEY, value TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS events (
                position INTEGER PRIMARY KEY,
                unit TEXT NOT NULL,
                sequence INTEGER NOT NULL,
                line TEXT NOT NULL,
                UNIQUE (unit, sequence)
            );
            CREATE TABLE IF NOT EXISTS held (
                unit TEXT NOT NULL,
                sequence INTEGER NOT NULL,
                line TEXT NOT NULL,
                PRIMARY KEY (unit, sequence)
            );
            CREATE TABLE IF NOT EXISTS cursors (unit TEXT PRIMARY KEY, next INTEGER NOT NULL);
            CREATE TABLE IF NOT EXISTS inputs (sha256 TEXT PRIMARY KEY);
            CREATE TABLE IF NOT EXISTS uploads (
                position INTEGER PRIMARY KEY,
                sha256 TEXT NOT NULL UNIQUE,
                bytes INTEGER NOT NULL,
                scope TEXT NOT NULL,
                time TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS units (
                unit TEXT PRIMARY KEY,
                status TEXT NOT NULL DEFAULT 'queued',
                machine TEXT NOT NULL DEFAULT '',
                since TEXT,
                cached INTEGER NOT NULL DEFAULT 0,
                lines TEXT NOT NULL DEFAULT '[]',
                finishedAt TEXT,
                finishedPosition INTEGER
            );
        `);
        context.setWebSocketAutoResponse(new WebSocketRequestResponsePair('ping', 'pong'));
    }

    override async fetch(request: Request): Promise<Response> {
        const run = request.headers.get(RunHeader);
        if (run === null || run === '') {
            return jsonResponse(500, { error: 'the run object was called without a run' });
        }
        const knownRun = this.fact('run');
        if (knownRun === null) {
            this.setFact('run', run);
        }
        else if (knownRun !== run) {
            return jsonResponse(500, { error: `this object holds run ${knownRun}, not ${run}` });
        }
        const operation = new URL(request.url).pathname;
        if (operation === '/plan') {
            return this.acceptPlan(request);
        }
        if (operation === '/events') {
            return this.acceptEvents(request, run);
        }
        if (operation === '/verdict') {
            return this.acceptVerdict(request, run);
        }
        if (operation === '/stream') {
            return this.openStream(request);
        }
        if (operation === '/blob-access' || operation === '/blob-uploaded') {
            const scope = request.headers.get(ScopeHeader);
            if (scope === null || !TokenScopes.includes(scope as TokenScope)) {
                return jsonResponse(500, { error: 'a blob operation was called without a scope' });
            }
            const blob = (await request.json()) as { sha256: string; action?: 'read' | 'write'; bytes?: number };
            if (!Sha256Pattern.test(blob.sha256)) {
                return jsonResponse(500, { error: 'a blob operation was called without a sha256' });
            }
            if (operation === '/blob-access') {
                return this.blobAccess(scope as TokenScope, blob.sha256, blob.action === 'write' ? 'write' : 'read');
            }
            return this.recordUpload(scope as TokenScope, blob.sha256, blob.bytes ?? 0);
        }
        return jsonResponse(404, { error: 'no such run operation' });
    }

    // ---------- Plan ----------

    private async acceptPlan(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumEventsBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: 'the plan is too large' });
        }
        const plan = checkPlan(body);
        if (typeof plan === 'string') {
            return jsonResponse(400, { error: plan });
        }
        // The whole body is compared, inputs too, each list in the order given.
        const planText = JSON.stringify(plan);
        const counts = { units: plan.units.length, inputs: plan.inputs.length };
        const existing = this.fact('plan');
        if (existing !== null) {
            if (existing === planText) {
                return jsonResponse(200, { plan: 'unchanged', ...counts });
            }
            return jsonResponse(409, { error: 'this run already has a different plan' });
        }
        this.ctx.storage.transactionSync(() => {
            this.setFact('plan', planText);
            for (const input of plan.inputs) {
                this.sql.exec('INSERT INTO inputs (sha256) VALUES (?)', input);
            }
            // Events may have come first, so a unit already seen keeps its state.
            for (const unit of plan.units) {
                this.sql.exec('INSERT INTO units (unit) VALUES (?) ON CONFLICT (unit) DO NOTHING', unit);
            }
            this.setFact('plannedAt', new Date().toISOString());
        });
        this.broadcast(planFrame(planText));
        await this.scheduleBoardPush();
        return jsonResponse(201, { plan: 'set', ...counts });
    }

    // ---------- Blobs ----------

    // The Store table in docs/protocol.md. A runner reads the plan's inputs and what this run uploaded; a viewer
    // only what this run uploaded; a coordinator anything. A runner writes until the run has its verdict.
    private blobAccess(scope: TokenScope, sha256: string, action: 'read' | 'write'): Response {
        if (action === 'write') {
            if (scope === 'viewer') {
                return jsonResponse(403, { error: "a viewer token can't upload a blob" });
            }
            if (scope === 'runner' && this.fact('verdict') !== null) {
                return jsonResponse(409, { error: 'this run has its verdict; a runner uploads nothing more' });
            }
            return jsonResponse(200, { allowed: true });
        }
        if (scope === 'coordinator' || this.uploaded(sha256)) {
            return jsonResponse(200, { allowed: true });
        }
        if (scope === 'runner' && this.plannedInput(sha256)) {
            return jsonResponse(200, { allowed: true });
        }
        const readable = scope === 'runner' ? "the plan's inputs and what this run uploaded" : 'what this run uploaded';
        return jsonResponse(403, { error: `a ${scope} token reads only ${readable}` });
    }

    // A PUT that left the blob in the store, stored now or found there, counts as the run's upload. The first
    // upload of a hash is the one kept.
    private recordUpload(scope: TokenScope, sha256: string, bytes: number): Response {
        if (scope === 'runner' && this.fact('verdict') !== null) {
            return jsonResponse(409, { error: 'this run has its verdict; a runner uploads nothing more' });
        }
        this.sql.exec(
            'INSERT INTO uploads (sha256, bytes, scope, time) VALUES (?, ?, ?, ?) ON CONFLICT (sha256) DO NOTHING',
            sha256,
            bytes,
            scope,
            new Date().toISOString(),
        );
        return jsonResponse(200, { recorded: true });
    }

    private uploaded(sha256: string): boolean {
        return this.sql.exec('SELECT 1 FROM uploads WHERE sha256 = ?', sha256).toArray().length > 0;
    }

    private plannedInput(sha256: string): boolean {
        return this.sql.exec('SELECT 1 FROM inputs WHERE sha256 = ?', sha256).toArray().length > 0;
    }

    // ---------- Events ----------

    private async acceptEvents(request: Request, run: string): Promise<Response> {
        if (this.fact('verdict') !== null) {
            return jsonResponse(409, { error: 'this run has its verdict; it takes no more events' });
        }
        const body = await readBodyText(request, MaximumEventsBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `an events batch is at most ${MaximumEventsBodyBytes} bytes` });
        }
        // Check every line before storing any, so a batch lands whole or not at all.
        const incoming: EventLine[] = [];
        const lines = body.split('\n');
        for (let index = 0; index < lines.length; index++) {
            const line = (lines[index] ?? '').replace(/\r$/, '').trim();
            if (line === '') {
                continue;
            }
            if (line.length > MaximumEventLineBytes / 3 && textEncoder.encode(line).length > MaximumEventLineBytes) {
                return jsonResponse(413, { error: `line ${index + 1} is longer than ${MaximumEventLineBytes} bytes` });
            }
            const check = checkEventLine(line, run);
            if (!check.valid) {
                return jsonResponse(400, { error: `line ${index + 1}: ${check.problem}` });
            }
            incoming.push({ unit: check.event.unit, sequence: check.event.sequence, line: line });
        }
        if (incoming.length === 0) {
            return jsonResponse(400, { error: 'no events in the body' });
        }

        const tally: Tally = { accepted: 0, replays: 0, conflicts: [], frames: [] };
        try {
            this.ctx.storage.transactionSync(() => {
                for (const event of incoming) {
                    this.applyEvent(event, tally);
                }
            });
        }
        catch (error) {
            // transactionSync rolled the whole batch back.
            if (error instanceof HeldLimitError) {
                return jsonResponse(429, { error: error.message });
            }
            throw error;
        }
        for (const frame of tally.frames) {
            this.broadcast(frame);
        }
        if (tally.frames.length > 0) {
            await this.scheduleBoardPush();
        }
        const summary = {
            accepted: tally.accepted,
            released: tally.frames.length,
            holding: this.heldCount(),
            replays: tally.replays,
            conflicts: tally.conflicts,
        };
        if (tally.conflicts.length > 0) {
            return jsonResponse(409, {
                error: 'some events reuse a unit and sequence already held, with different content; the first was kept',
                ...summary,
            });
        }
        return jsonResponse(200, summary);
    }

    // One event, inside the batch's transaction. An event already held (in the log or waiting) is a replay
    // when its content matches and a conflict when it doesn't; either way the first one stays.
    private applyEvent(event: EventLine, tally: Tally): void {
        const held = this.heldOrLogged(event.unit, event.sequence);
        if (held !== null) {
            if (canonicalJson(JSON.parse(held)) === canonicalJson(JSON.parse(event.line))) {
                tally.replays++;
            }
            else {
                tally.conflicts.push(`${event.unit} ${event.sequence}`);
            }
            return;
        }
        const next = this.nextSequence(event.unit);
        if (event.sequence < next) {
            // Below the cursor yet missing from the log cannot happen; refuse it as a conflict rather than guess.
            tally.conflicts.push(`${event.unit} ${event.sequence}`);
            return;
        }
        tally.accepted++;
        if (event.sequence > next) {
            if (this.heldCount() >= MaximumHeldEvents) {
                throw new HeldLimitError(`more than ${MaximumHeldEvents} events are waiting for a gap to fill`);
            }
            this.sql.exec(
                'INSERT INTO held (unit, sequence, line) VALUES (?, ?, ?)',
                event.unit,
                event.sequence,
                event.line,
            );
            return;
        }
        // In order: append it, then release whatever was waiting behind it.
        tally.frames.push(this.append(event));
        for (let cursor = event.sequence + 1; ; cursor++) {
            const waiting = this.sql
                .exec<{ line: string }>('SELECT line FROM held WHERE unit = ? AND sequence = ?', event.unit, cursor)
                .toArray()[0];
            if (waiting === undefined) {
                return;
            }
            this.sql.exec('DELETE FROM held WHERE unit = ? AND sequence = ?', event.unit, cursor);
            tally.frames.push(this.append({ unit: event.unit, sequence: cursor, line: waiting.line }));
        }
    }

    private heldOrLogged(unit: string, sequence: number): string | null {
        const logged = this.sql
            .exec<{ line: string }>('SELECT line FROM events WHERE unit = ? AND sequence = ?', unit, sequence)
            .toArray()[0];
        if (logged !== undefined) {
            return logged.line;
        }
        const held = this.sql
            .exec<{ line: string }>('SELECT line FROM held WHERE unit = ? AND sequence = ?', unit, sequence)
            .toArray()[0];
        return held === undefined ? null : held.line;
    }

    private nextSequence(unit: string): number {
        const row = this.sql.exec<{ next: number }>('SELECT next FROM cursors WHERE unit = ?', unit).toArray()[0];
        return row === undefined ? 0 : row.next;
    }

    private heldCount(): number {
        return this.sql.exec<{ count: number }>('SELECT COUNT(*) AS count FROM held').one().count;
    }

    // Appends one in-order event to the log, moves the unit's cursor, and returns the frame for viewers.
    private append(event: EventLine): string {
        const position = this.sql
            .exec<{ position: number }>(
                'INSERT INTO events (unit, sequence, line) VALUES (?, ?, ?) RETURNING position',
                event.unit,
                event.sequence,
                event.line,
            )
            .one().position;
        this.sql.exec(
            'INSERT INTO cursors (unit, next) VALUES (?, ?) ON CONFLICT (unit) DO UPDATE SET next = excluded.next',
            event.unit,
            event.sequence + 1,
        );
        this.trackUnit(JSON.parse(event.line) as LoomEvent, position);
        return eventFrame(position, event.line);
    }

    // Moves the unit's board state on by one logged event, inside the batch's transaction. A unit nobody planned
    // gets a row too, so the board shows what the wire was sent.
    private trackUnit(logged: LoomEvent, position: number): void {
        this.sql.exec('INSERT INTO units (unit) VALUES (?) ON CONFLICT (unit) DO NOTHING', logged.unit);
        if (logged.type === 'started') {
            this.sql.exec(
                `UPDATE units SET status = 'running', machine = ?, since = ?
                 WHERE unit = ? AND status IN ('queued', 'running')`,
                typeof logged.machine === 'string' ? logged.machine : '',
                logged.time,
                logged.unit,
            );
        }
        else if (logged.type === 'cached') {
            // Served from the cache, never run: it goes from queued straight to its finished event.
            this.sql.exec('UPDATE units SET cached = 1 WHERE unit = ?', logged.unit);
        }
        else if (logged.type === 'error' && logged.phase === 'place') {
            // The coordinator took the unit off a box that dropped it; it waits to be placed again.
            this.sql.exec(
                "UPDATE units SET status = 'queued', machine = '', since = NULL WHERE unit = ? AND status = 'running'",
                logged.unit,
            );
        }
        else if (logged.type === 'output') {
            const row = this.sql.exec<{ lines: string }>('SELECT lines FROM units WHERE unit = ?', logged.unit).one();
            const lines = JSON.parse(row.lines) as string[];
            lines.push(typeof logged.text === 'string' ? logged.text : '');
            this.sql.exec(
                'UPDATE units SET lines = ? WHERE unit = ?',
                JSON.stringify(lines.slice(-UnitLines)),
                logged.unit,
            );
        }
        else if (logged.type === 'finished') {
            this.sql.exec(
                'UPDATE units SET status = ?, finishedAt = ?, finishedPosition = ? WHERE unit = ?',
                logged.status as string,
                logged.time,
                position,
                logged.unit,
            );
        }
    }

    // ---------- Verdict ----------

    private async acceptVerdict(request: Request, run: string): Promise<Response> {
        const body = await readBodyText(request, MaximumEventsBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: 'the verdict is too large' });
        }
        const verdict = checkVerdict(body);
        if (typeof verdict === 'string') {
            return jsonResponse(400, { error: verdict });
        }
        if (this.fact('plan') === null) {
            return jsonResponse(409, { error: 'a verdict needs the plan it was decided against' });
        }
        const verdictText = JSON.stringify(verdict);
        const existing = this.fact('verdict');
        if (existing !== null && existing !== verdictText) {
            return jsonResponse(409, { error: 'this run already has a different verdict' });
        }
        if (existing === null) {
            this.setFact('verdict', verdictText);
            this.broadcast(verdictFrame(verdictText));
            await this.scheduleBoardPush();
        }
        // Archive once; a repeat of the same verdict retries an archive that failed.
        if (this.fact('archived') === null) {
            try {
                await this.archive(run);
            }
            catch (error) {
                return jsonResponse(502, { error: `the verdict is held but archiving failed: ${String(error)}` });
            }
        }
        return jsonResponse(existing === null ? 201 : 200, {
            verdict: verdict.status,
            archive: `runs/${run}/events.jsonl`,
            uploads: `runs/${run}/blobs.jsonl`,
        });
    }

    private async archive(run: string): Promise<void> {
        const logged = this.sql.exec<{ line: string }>('SELECT line FROM events ORDER BY position').toArray();
        const archive = logged
            .map(function (row) {
                return row.line + '\n';
            })
            .join('');
        await this.env.Store.put(`runs/${run}/events.jsonl`, archive, {
            httpMetadata: { contentType: 'application/x-ndjson' },
        });
        // Events still waiting on a gap never reached the log; keep them beside it so a void run can be read.
        const held = this.sql
            .exec<{ line: string }>('SELECT line FROM held ORDER BY unit, sequence')
            .toArray()
            .map(function (row) {
                return row.line + '\n';
            })
            .join('');
        if (held !== '') {
            await this.env.Store.put(`runs/${run}/held.jsonl`, held, {
                httpMetadata: { contentType: 'application/x-ndjson' },
            });
        }
        // Written even when empty, so a reader can tell a run that uploaded nothing from one never archived.
        const uploads = this.sql
            .exec<{ sha256: string; bytes: number; scope: string }>(
                'SELECT sha256, bytes, scope FROM uploads ORDER BY position',
            )
            .toArray()
            .map(function (row) {
                return JSON.stringify({ sha256: row.sha256, bytes: row.bytes, scope: row.scope }) + '\n';
            })
            .join('');
        await this.env.Store.put(`runs/${run}/blobs.jsonl`, uploads, {
            httpMetadata: { contentType: 'application/x-ndjson' },
        });
        this.setFact('archived', new Date().toISOString());
    }

    // ---------- Stream ----------

    // A viewer gets the plan, every event in the log after `after` in order, a caughtUp marker, then the
    // verdict if there is one. All of it is sent before this handler yields, so no event can slip between
    // the backlog and the live stream.
    private openStream(request: Request): Response {
        if (request.headers.get('Upgrade')?.toLowerCase() !== 'websocket') {
            return jsonResponse(426, { error: 'this endpoint speaks WebSocket' });
        }
        const afterText = new URL(request.url).searchParams.get('after');
        const after = afterText !== null && /^\d+$/.test(afterText) ? Number(afterText) : 0;
        const pair = new WebSocketPair();
        const server = pair[1];
        this.ctx.acceptWebSocket(server);
        const plan = this.fact('plan');
        if (plan !== null) {
            server.send(planFrame(plan));
        }
        let position = after;
        for (const row of this.sql.exec<{ position: number; line: string }>(
            'SELECT position, line FROM events WHERE position > ? ORDER BY position',
            after,
        )) {
            server.send(eventFrame(row.position, row.line));
            position = row.position;
        }
        server.send(JSON.stringify({ kind: 'caughtUp', position: position }));
        const verdict = this.fact('verdict');
        if (verdict !== null) {
            server.send(verdictFrame(verdict));
        }
        return new Response(null, { status: 101, webSocket: pair[0] });
    }

    override webSocketMessage(): void {
        // Viewers only listen; "ping" is answered by the auto response without waking this object.
    }

    override webSocketClose(socket: WebSocket, code: number, reason: string): void {
        try {
            socket.close(code, reason);
        }
        catch {
            // Already closed.
        }
    }

    private broadcast(frame: string): void {
        for (const socket of this.ctx.getWebSockets()) {
            try {
                socket.send(frame);
            }
            catch {
                // A socket that is closing misses the frame; it catches up with ?after= when it reconnects.
            }
        }
    }

    // ---------- Board ----------

    // Marks the summary as owed to the board and makes sure an alarm will push it, no sooner than 500 ms after the
    // last push. Only storage is touched here, so the request that changed the run answers without waiting on the
    // board, and nothing here can fail that request.
    private async scheduleBoardPush(): Promise<void> {
        this.setFact('updatedAt', new Date().toISOString());
        this.setFact('boardOwed', '1');
        try {
            if ((await this.ctx.storage.getAlarm()) === null) {
                await this.ctx.storage.setAlarm(this.nextBoardPush());
            }
        }
        catch {
            // The next change sets the alarm again; the summary it pushes includes this one.
        }
    }

    private nextBoardPush(): number {
        return Math.max(Date.now(), Number(this.fact('boardPushedAt') ?? '0') + BoardPushMilliseconds);
    }

    // Pushes the summary when one is owed. A push that fails stays owed and is tried again on the next alarm.
    override async alarm(): Promise<void> {
        const run = this.fact('run');
        if (run === null || this.fact('boardOwed') === null) {
            return;
        }
        // Cleared before the push, so a change that lands while it is in flight is owed again.
        this.deleteFact('boardOwed');
        this.setFact('boardPushedAt', String(Date.now()));
        try {
            const board = this.env.Board.get(this.env.Board.idFromName(BoardName));
            const response = await board.fetch('https://board/run', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(this.boardSummary(run)),
            });
            await response.body?.cancel();
            if (!response.ok) {
                throw new Error(`the board answered ${response.status}`);
            }
        }
        catch {
            this.setFact('boardOwed', '1');
            await this.ctx.storage.setAlarm(Date.now() + BoardRetryMilliseconds);
            return;
        }
        // A change during the push asked for an alarm while this one was running; make sure one is set.
        if (this.fact('boardOwed') !== null) {
            await this.ctx.storage.setAlarm(this.nextBoardPush());
        }
    }

    // The run's summary as the board holds it (docs/protocol.md, The board), built from the units table.
    private boardSummary(run: string): RunSummary {
        const counts: Record<string, number> = {};
        for (const row of this.sql.exec<{ status: string; count: number }>(
            'SELECT status, COUNT(*) AS count FROM units GROUP BY status',
        )) {
            counts[row.status] = row.count;
        }
        const active: ActiveUnit[] = this.sql
            .exec<{ unit: string; machine: string; since: string | null }>(
                "SELECT unit, machine, since FROM units WHERE status = 'running' ORDER BY since, unit",
            )
            .toArray()
            .map(function (row) {
                return { unit: row.unit, machine: row.machine, since: row.since ?? '' };
            });
        const failures: FailedUnit[] = this.sql
            .exec<{ unit: string; finishedAt: string | null; status: 'failed' | 'broken'; lines: string }>(
                `SELECT unit, finishedAt, status, lines FROM units WHERE status IN ('failed', 'broken')
                 ORDER BY finishedPosition DESC LIMIT ?`,
                BoardFailures,
            )
            .toArray()
            .map(function (row) {
                return {
                    unit: row.unit,
                    at: row.finishedAt ?? '',
                    status: row.status,
                    lines: JSON.parse(row.lines) as string[],
                };
            });
        const verdict = this.fact('verdict');
        const cached = this.sql.exec<{ count: number }>('SELECT COUNT(*) AS count FROM units WHERE cached = 1').one();
        return {
            run: run,
            job: jobOfRun(run),
            plannedAt: this.fact('plannedAt'),
            updatedAt: this.fact('updatedAt') ?? new Date().toISOString(),
            units: this.sql.exec<{ count: number }>('SELECT COUNT(*) AS count FROM units').one().count,
            queued: counts.queued ?? 0,
            running: counts.running ?? 0,
            passed: counts.passed ?? 0,
            failed: counts.failed ?? 0,
            broken: counts.broken ?? 0,
            cached: cached.count,
            verdict: verdict === null ? null : (JSON.parse(verdict) as Verdict).status,
            active: active,
            failures: failures,
        };
    }

    // ---------- Facts ----------

    private fact(name: string): string | null {
        const row = this.sql.exec<{ value: string }>('SELECT value FROM facts WHERE name = ?', name).toArray()[0];
        return row === undefined ? null : row.value;
    }

    private setFact(name: string, value: string): void {
        this.sql.exec(
            'INSERT INTO facts (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value',
            name,
            value,
        );
    }

    private deleteFact(name: string): void {
        this.sql.exec('DELETE FROM facts WHERE name = ?', name);
    }
}

// Viewers get the plan's units; its inputs are for the blob check.
function planFrame(planText: string): string {
    const plan = JSON.parse(planText) as Plan;
    return JSON.stringify({ kind: 'plan', units: plan.units });
}

function eventFrame(position: number, line: string): string {
    return `{"kind":"event","position":${position},"event":${line}}`;
}

function verdictFrame(verdictText: string): string {
    return `{"kind":"verdict","verdict":${verdictText}}`;
}

// The plan body is protocol.Plan, exactly {"units": ["<id>", ...], "inputs": ["<sha256>", ...]}: at least one
// unit id, each a non-empty string, none twice; every input 64 lowercase hex digits, none twice, [] when none.
export function checkPlan(body: string): Plan | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the plan is not JSON';
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        return 'the plan is not a JSON object';
    }
    const keys = Object.keys(parsed).sort();
    if (keys.length !== 2 || keys[0] !== 'inputs' || keys[1] !== 'units') {
        return 'the plan has exactly units and inputs';
    }
    const units = (parsed as { units: unknown }).units;
    const inputs = (parsed as { inputs: unknown }).inputs;
    if (!Array.isArray(units) || units.length === 0) {
        return 'units must be a non-empty array';
    }
    if (units.length > MaximumPlannedUnits) {
        return `a plan has at most ${MaximumPlannedUnits} units`;
    }
    const seen = new Set<string>();
    for (const unit of units) {
        if (typeof unit !== 'string' || unit === '' || unit.length > MaximumUnitIdLength) {
            return 'every unit id is a non-empty string';
        }
        if (seen.has(unit)) {
            return `the plan names ${unit} twice`;
        }
        seen.add(unit);
    }
    if (!Array.isArray(inputs)) {
        return 'inputs must be an array, [] when the plan has none';
    }
    const seenInputs = new Set<string>();
    for (const input of inputs) {
        if (typeof input !== 'string' || !Sha256Pattern.test(input)) {
            return 'every input is a sha256, 64 lowercase hex digits';
        }
        if (seenInputs.has(input)) {
            return `the plan names input ${input} twice`;
        }
        seenInputs.add(input);
    }
    return { units: units as string[], inputs: inputs as string[] };
}

// The verdict is protocol.Verdict as Go marshals it: exactly status, failed, problems and cached, in lowercase,
// with an empty list written as [].
export function checkVerdict(body: string): Verdict | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the verdict is not JSON';
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        return 'the verdict is not a JSON object';
    }
    const keys = Object.keys(parsed).sort();
    if (
        keys.length !== 4 ||
        keys[0] !== 'cached' ||
        keys[1] !== 'failed' ||
        keys[2] !== 'problems' ||
        keys[3] !== 'status'
    ) {
        return 'the verdict has exactly status, failed, problems and cached';
    }
    const fields = parsed as Record<string, unknown>;
    if (fields.status !== 'green' && fields.status !== 'red' && fields.status !== 'void') {
        return 'status is green, red or void';
    }
    if (!isStringList(fields.failed) || !isStringList(fields.problems) || !isStringList(fields.cached)) {
        return 'failed, problems and cached are lists of strings, [] when empty';
    }
    return { status: fields.status, failed: fields.failed, problems: fields.problems, cached: fields.cached };
}

function isStringList(value: unknown): value is string[] {
    return (
        Array.isArray(value) &&
        value.every(function (item) {
            return typeof item === 'string';
        })
    );
}
