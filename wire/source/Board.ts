// The board: one Durable Object, named `board`, holding a summary of every run and every machine in its SQLite
// storage and streaming changes to its viewers over hibernating WebSockets (docs/protocol.md, The board). Each
// run's object pushes its summary here; the coordinator posts its machines. Broadcasts wait for an alarm, at most
// one flush every 250 ms, so fifty busy runs send a viewer a few frames a second rather than hundreds.

import { DurableObject } from 'cloudflare:workers';
import { jsonResponse, readBodyText } from './Http';

export const BoardName = 'board';
export const BoardSubprotocol = 'loom';
export const MaximumMachinesBodyBytes = 1024 * 1024;
export const MaximumMachines = 1000;
export const MaximumMachineNameLength = 256;
export const MaximumSummaryBytes = 16 * 1024 * 1024;
export const BroadcastMilliseconds = 250;
export const FinishedWindowMilliseconds = 60 * 1000;
export const RetentionMilliseconds = 24 * 60 * 60 * 1000;

// A run id the coordinator names is <job>-<YYYYMMDDTHHMMSS>-<8 hex>; coordinator.RunId writes it.
const runIdSuffixPattern = /^(.+)-\d{8}T\d{6}-[0-9a-f]{8}$/;

export interface ActiveUnit {
    unit: string;
    machine: string;
    since: string;
}

export interface FailedUnit {
    unit: string;
    at: string;
    status: 'failed' | 'broken';
    lines: string[];
}

// A run's summary, in the doc's key order.
export interface RunSummary {
    run: string;
    job: string;
    plannedAt: string | null;
    updatedAt: string;
    units: number;
    queued: number;
    running: number;
    passed: number;
    failed: number;
    broken: number;
    cached: number;
    verdict: 'green' | 'red' | 'void' | null;
    active: ActiveUnit[];
    failures: FailedUnit[];
}

export interface Machine {
    name: string;
    cores: number;
    slots: number;
}

export interface BoardMachine extends Machine {
    running: number;
}

export interface LongestUnit {
    unit: string;
    run: string;
    machine: string;
    since: string;
}

export interface Pulse {
    running: number;
    finishedLastMinute: number;
    queued: number;
    longest: LongestUnit | null;
}

export interface Snapshot {
    kind: 'snapshot';
    runs: RunSummary[];
    machines: BoardMachine[];
    pulse: Pulse;
}

// The run id without its time and random suffix, or the whole id when it has none.
export function jobOfRun(run: string): string {
    const match = runIdSuffixPattern.exec(run);
    return match === null ? run : (match[1] ?? run);
}

// A run with a verdict is over: whatever it still lists as running or queued will never run, so it counts toward
// no machine and no pulse, though its summary still says what it last saw.
function liveRuns(summaries: RunSummary[]): RunSummary[] {
    return summaries.filter(function (summary) {
        return summary.verdict === null;
    });
}

export function machinesOf(machines: Machine[], summaries: RunSummary[]): BoardMachine[] {
    const running = new Map<string, number>();
    for (const summary of liveRuns(summaries)) {
        for (const active of summary.active) {
            running.set(active.machine, (running.get(active.machine) ?? 0) + 1);
        }
    }
    return machines.map(function (machine) {
        return {
            name: machine.name,
            cores: machine.cores,
            slots: machine.slots,
            running: running.get(machine.name) ?? 0,
        };
    });
}

export function pulseOf(summaries: RunSummary[], finishedLastMinute: number): Pulse {
    let running = 0;
    let queued = 0;
    let longest: LongestUnit | null = null;
    let longestSince = Infinity;
    for (const summary of liveRuns(summaries)) {
        running += summary.running;
        queued += summary.queued;
        for (const active of summary.active) {
            // Parsed, not compared as text: RFC 3339 with fractional seconds doesn't sort as a string.
            const since = Date.parse(active.since);
            if (!Number.isNaN(since) && since < longestSince) {
                longestSince = since;
                longest = { unit: active.unit, run: summary.run, machine: active.machine, since: active.since };
            }
        }
    }
    return { running: running, finishedLastMinute: finishedLastMinute, queued: queued, longest: longest };
}

function finishedOf(summary: RunSummary): number {
    return summary.passed + summary.failed + summary.broken;
}

// The machines body is exactly {"machines": [{"name", "cores", "slots"}, ...]}: a non-empty name, none twice,
// and counts that are whole and not negative. Go marshals an empty slice of machines as null, so null is none.
export function checkMachines(body: string): Machine[] | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the machines are not JSON';
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        return 'the body is a JSON object';
    }
    const keys = Object.keys(parsed);
    if (keys.length !== 1 || keys[0] !== 'machines') {
        return 'the body has exactly machines';
    }
    const listed = (parsed as { machines: unknown }).machines;
    if (listed === null) {
        return [];
    }
    if (!Array.isArray(listed)) {
        return 'machines is a list';
    }
    if (listed.length > MaximumMachines) {
        return `at most ${MaximumMachines} machines`;
    }
    const machines: Machine[] = [];
    const seen = new Set<string>();
    for (const machine of listed as unknown[]) {
        if (typeof machine !== 'object' || machine === null || Array.isArray(machine)) {
            return 'each machine is exactly name, cores and slots';
        }
        const machineKeys = Object.keys(machine).sort();
        if (
            machineKeys.length !== 3 ||
            machineKeys[0] !== 'cores' ||
            machineKeys[1] !== 'name' ||
            machineKeys[2] !== 'slots'
        ) {
            return 'each machine is exactly name, cores and slots';
        }
        const fields = machine as Record<string, unknown>;
        if (typeof fields.name !== 'string' || fields.name === '' || fields.name.length > MaximumMachineNameLength) {
            return `a machine's name is 1 to ${MaximumMachineNameLength} characters`;
        }
        if (!isCount(fields.cores) || !isCount(fields.slots)) {
            return "a machine's cores and slots are whole numbers, not negative";
        }
        if (seen.has(fields.name)) {
            return `the body names machine ${fields.name} twice`;
        }
        seen.add(fields.name);
        machines.push({ name: fields.name, cores: fields.cores, slots: fields.slots });
    }
    return machines;
}

function isCount(value: unknown): value is number {
    return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

export class Board extends DurableObject<Env> {
    private readonly sql: SqlStorage;

    constructor(context: DurableObjectState, environment: Env) {
        super(context, environment);
        this.sql = context.storage.sql;
        // Times here are the board's own clock in milliseconds, since retention and the last minute are its call.
        this.sql.exec(`
            CREATE TABLE IF NOT EXISTS facts (name TEXT PRIMARY KEY, value TEXT NOT NULL);
            CREATE TABLE IF NOT EXISTS runs (
                run TEXT PRIMARY KEY,
                summary TEXT NOT NULL,
                finished INTEGER NOT NULL,
                touchedAt INTEGER NOT NULL,
                verdictAt INTEGER
            );
            CREATE TABLE IF NOT EXISTS machines (
                name TEXT PRIMARY KEY,
                cores INTEGER NOT NULL,
                slots INTEGER NOT NULL
            );
            CREATE TABLE IF NOT EXISTS finishes (
                position INTEGER PRIMARY KEY,
                at INTEGER NOT NULL,
                count INTEGER NOT NULL
            );
            CREATE TABLE IF NOT EXISTS changed (run TEXT PRIMARY KEY);
        `);
        context.setWebSocketAutoResponse(new WebSocketRequestResponsePair('ping', 'pong'));
    }

    override async fetch(request: Request): Promise<Response> {
        this.forgetOldRuns(Date.now());
        const operation = new URL(request.url).pathname;
        if (operation === '/run' && request.method === 'POST') {
            const body = await readBodyText(request, MaximumSummaryBytes);
            if (body === null) {
                return jsonResponse(413, { error: 'the summary is too large' });
            }
            await this.recordRun(JSON.parse(body) as RunSummary);
            return jsonResponse(200, { recorded: true });
        }
        if (operation === '/machines' && request.method === 'POST') {
            return this.acceptMachines(request);
        }
        if (operation === '/snapshot' && request.method === 'GET') {
            return jsonResponse(200, this.snapshot());
        }
        if (operation === '/stream' && request.method === 'GET') {
            return this.openStream(request);
        }
        return jsonResponse(404, { error: 'no such board operation' });
    }

    // ---------- Runs ----------

    // One run's summary, pushed by its object. Units it finished since its last push count toward the last minute.
    private async recordRun(summary: RunSummary): Promise<void> {
        const now = Date.now();
        const previous = this.sql
            .exec<{ finished: number; verdictAt: number | null }>(
                'SELECT finished, verdictAt FROM runs WHERE run = ?',
                summary.run,
            )
            .toArray()[0];
        const finished = finishedOf(summary);
        const newlyFinished = finished - (previous?.finished ?? 0);
        const verdictAt = previous?.verdictAt ?? (summary.verdict === null ? null : now);
        this.ctx.storage.transactionSync(() => {
            this.sql.exec(
                `INSERT INTO runs (run, summary, finished, touchedAt, verdictAt) VALUES (?, ?, ?, ?, ?)
                 ON CONFLICT (run) DO UPDATE SET summary = excluded.summary, finished = excluded.finished,
                 touchedAt = excluded.touchedAt, verdictAt = excluded.verdictAt`,
                summary.run,
                JSON.stringify(summary),
                finished,
                now,
                verdictAt,
            );
            if (newlyFinished > 0) {
                this.sql.exec('INSERT INTO finishes (at, count) VALUES (?, ?)', now, newlyFinished);
            }
            if (this.ctx.getWebSockets().length > 0) {
                this.sql.exec('INSERT INTO changed (run) VALUES (?) ON CONFLICT (run) DO NOTHING', summary.run);
            }
        });
        await this.scheduleFlush();
    }

    private summaries(): RunSummary[] {
        return this.sql
            .exec<{ summary: string }>('SELECT summary FROM runs ORDER BY touchedAt DESC, run')
            .toArray()
            .map(function (row) {
                return JSON.parse(row.summary) as RunSummary;
            });
    }

    // Retention: a run leaves 24 hours after its verdict, or 24 hours after its last push when it has none.
    // Done lazily, on every call and every flush, so nothing needs to wake the board just to forget.
    private forgetOldRuns(now: number): void {
        const cutoff = now - RetentionMilliseconds;
        this.sql.exec(
            `DELETE FROM runs
             WHERE (verdictAt IS NOT NULL AND verdictAt <= ?) OR (verdictAt IS NULL AND touchedAt <= ?)`,
            cutoff,
            cutoff,
        );
        this.sql.exec('DELETE FROM changed WHERE run NOT IN (SELECT run FROM runs)');
        this.sql.exec('DELETE FROM finishes WHERE at <= ?', now - FinishedWindowMilliseconds);
    }

    // ---------- Machines ----------

    private async acceptMachines(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumMachinesBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `the machines are at most ${MaximumMachinesBodyBytes} bytes` });
        }
        const machines = checkMachines(body);
        if (typeof machines === 'string') {
            return jsonResponse(400, { error: machines });
        }
        this.ctx.storage.transactionSync(() => {
            for (const machine of machines) {
                this.sql.exec(
                    `INSERT INTO machines (name, cores, slots) VALUES (?, ?, ?)
                     ON CONFLICT (name) DO UPDATE SET cores = excluded.cores, slots = excluded.slots`,
                    machine.name,
                    machine.cores,
                    machine.slots,
                );
            }
        });
        await this.scheduleFlush();
        return jsonResponse(200, { machines: machines.length });
    }

    private machines(summaries: RunSummary[]): BoardMachine[] {
        const posted = this.sql.exec<{ name: string; cores: number; slots: number }>(
            'SELECT name, cores, slots FROM machines ORDER BY name',
        );
        return machinesOf(posted.toArray(), summaries);
    }

    // ---------- Pulse and snapshot ----------

    private finishedLastMinute(): number {
        const since = Date.now() - FinishedWindowMilliseconds;
        return this.sql
            .exec<{ total: number }>('SELECT COALESCE(SUM(count), 0) AS total FROM finishes WHERE at > ?', since)
            .one().total;
    }

    private snapshot(): Snapshot {
        const summaries = this.summaries();
        return {
            kind: 'snapshot',
            runs: summaries,
            machines: this.machines(summaries),
            pulse: pulseOf(summaries, this.finishedLastMinute()),
        };
    }

    // ---------- Stream ----------

    // The Worker has checked the board token, which came as a subprotocol beside `loom`; the answer names `loom`,
    // as a browser requires when it offered subprotocols. The snapshot is sent before this handler yields.
    private openStream(request: Request): Response {
        if (request.headers.get('Upgrade')?.toLowerCase() !== 'websocket') {
            return jsonResponse(426, { error: 'this endpoint speaks WebSocket' });
        }
        const pair = new WebSocketPair();
        const server = pair[1];
        this.ctx.acceptWebSocket(server);
        server.send(JSON.stringify(this.snapshot()));
        return new Response(null, {
            status: 101,
            webSocket: pair[0],
            headers: { 'Sec-WebSocket-Protocol': BoardSubprotocol },
        });
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

    // Asks for a flush no sooner than 250 ms after the last one. Nobody watching means nothing to send.
    private async scheduleFlush(): Promise<void> {
        if (this.ctx.getWebSockets().length === 0) {
            return;
        }
        if ((await this.ctx.storage.getAlarm()) !== null) {
            return;
        }
        const flushedAt = Number(this.fact('flushedAt') ?? '0');
        await this.ctx.storage.setAlarm(Math.max(Date.now(), flushedAt + BroadcastMilliseconds));
    }

    // The flush: a run frame for each run that changed, a machines frame when the machines read differently from
    // the last one sent (a post, or a unit starting or finishing on one), then one pulse after them.
    override async alarm(): Promise<void> {
        const now = Date.now();
        this.forgetOldRuns(now);
        this.setFact('flushedAt', String(now));
        const changed = this.sql
            .exec<{ summary: string }>(
                'SELECT runs.summary FROM changed JOIN runs ON runs.run = changed.run ORDER BY runs.touchedAt',
            )
            .toArray();
        this.sql.exec('DELETE FROM changed');
        const sockets = this.ctx.getWebSockets();
        if (sockets.length === 0) {
            return;
        }
        const frames = changed.map(function (row) {
            return `{"kind":"run","run":${row.summary}}`;
        });
        const summaries = this.summaries();
        const machinesText = JSON.stringify(this.machines(summaries));
        if (machinesText !== this.fact('machinesSent')) {
            this.setFact('machinesSent', machinesText);
            frames.push(`{"kind":"machines","machines":${machinesText}}`);
        }
        if (frames.length === 0) {
            return;
        }
        frames.push(JSON.stringify({ kind: 'pulse', ...pulseOf(summaries, this.finishedLastMinute()) }));
        for (const socket of sockets) {
            for (const frame of frames) {
                try {
                    socket.send(frame);
                }
                catch {
                    // A socket that is closing misses the frame; a reconnect starts from a fresh snapshot.
                    break;
                }
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
