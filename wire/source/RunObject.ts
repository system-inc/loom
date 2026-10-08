// One Durable Object per run. It holds the run's plan, its ordered event log, the events waiting for a gap to
// fill, and the verdict, all in the object's SQLite storage, and fans every accepted event out to the run's
// viewers over hibernating WebSockets. The Worker has already checked the token; this object trusts it.

import { DurableObject } from 'cloudflare:workers';
import { canonicalJson, checkEventLine, MaximumUnitIdLength } from './Events';
import { jsonResponse, readBodyText } from './Http';

export const RunHeader = 'Loom-Run';
export const MaximumEventsBodyBytes = 16 * 1024 * 1024;
export const MaximumEventLineBytes = 1024 * 1024;
export const MaximumHeldEvents = 10_000;
export const MaximumPlannedUnits = 10_000;

const textEncoder = new TextEncoder();

export interface Verdict {
    status: 'green' | 'red' | 'void';
    failed: string[];
    problems: string[];
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
        return jsonResponse(404, { error: 'no such run operation' });
    }

    // ---------- Plan ----------

    private async acceptPlan(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumEventsBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: 'the plan is too large' });
        }
        const units = checkPlan(body);
        if (typeof units === 'string') {
            return jsonResponse(400, { error: units });
        }
        const planText = JSON.stringify(units);
        const existing = this.fact('plan');
        if (existing !== null) {
            if (existing === planText) {
                return jsonResponse(200, { plan: 'unchanged', units: units.length });
            }
            return jsonResponse(409, { error: 'this run already has a different plan' });
        }
        this.setFact('plan', planText);
        this.broadcast(planFrame(planText));
        return jsonResponse(201, { plan: 'set', units: units.length });
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
        return eventFrame(position, event.line);
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
}

function planFrame(planText: string): string {
    return `{"kind":"plan","units":${planText}}`;
}

function eventFrame(position: number, line: string): string {
    return `{"kind":"event","position":${position},"event":${line}}`;
}

function verdictFrame(verdictText: string): string {
    return `{"kind":"verdict","verdict":${verdictText}}`;
}

// The plan body is {"units": ["<id>", ...]}: at least one id, each a non-empty string, none twice.
export function checkPlan(body: string): string[] | string {
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
    const keys = Object.keys(parsed);
    if (keys.length !== 1 || keys[0] !== 'units') {
        return 'the plan has exactly one field, units';
    }
    const units = (parsed as { units: unknown }).units;
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
    return units as string[];
}

// The verdict is protocol.Verdict as Go marshals it: exactly status, failed and problems, in lowercase, with an
// empty list written as [].
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
    if (keys.length !== 3 || keys[0] !== 'failed' || keys[1] !== 'problems' || keys[2] !== 'status') {
        return 'the verdict has exactly status, failed and problems';
    }
    const fields = parsed as Record<string, unknown>;
    if (fields.status !== 'green' && fields.status !== 'red' && fields.status !== 'void') {
        return 'status is green, red or void';
    }
    if (!isStringList(fields.failed) || !isStringList(fields.problems)) {
        return 'failed and problems are lists of strings, [] when empty';
    }
    return { status: fields.status, failed: fields.failed, problems: fields.problems };
}

function isStringList(value: unknown): value is string[] {
    return (
        Array.isArray(value) &&
        value.every(function (item) {
            return typeof item === 'string';
        })
    );
}
