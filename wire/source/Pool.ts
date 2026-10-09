// A pool: one Durable Object per pool name, holding a queue of units in its SQLite storage for machines Loom can't
// ssh into (docs/protocol.md, The pool). Units go out highest priority first, and first in, first out within one
// priority, so the star's units pass main's and main's pass a side candidate's on one shared pool. The coordinator puts units in; each instance's
// `loom-runner serve` asks for its next one and waits up to 20 s for it. A waiting ask is a promise this object
// holds, resolved the moment units arrive, oldest ask first, so nothing polls. The Worker has already checked the
// token; this object trusts it.

import { DurableObject } from 'cloudflare:workers';
import { MaximumUnitIdLength, RunIdPattern } from './Events';
import { jsonResponse, readBodyText } from './Http';

// How long a next waits for a unit before answering 204, so an idle instance asks about three times a minute.
export const PoolWaitMilliseconds = 20_000;
// GET /pools/<pool> lists the workers seen this recently: four hours, the longest a unit runs, since a worker running
// one asks nothing until it ends. A reader tells live from gone by seenAt and the runs' active units.
export const WorkerWindowMilliseconds = 4 * 60 * 60 * 1000;
export const MaximumUnitsBodyBytes = 16 * 1024 * 1024;
// A unit is one SQLite row, and a row holds at most 2 MB; a unit past this is refused by name, not by SQLite.
export const MaximumPoolUnitBytes = 1024 * 1024;
export const MaximumAskBodyBytes = 64 * 1024;
export const MaximumWorkerNameLength = 256;
// A priority is a whole number in this range; 0, the default, is the lowest a coordinator sends without asking.
export const MaximumPoolPriority = 1000;

const textEncoder = new TextEncoder();

// A unit as queued: its run and id for cancel and the workers list, and the unit's JSON as the coordinator gave it.
export interface PoolUnit {
    run: string;
    unit: string;
    json: string;
}

// A batch as queued: its units and the priority they all share.
export interface PoolBatch {
    units: PoolUnit[];
    priority: number;
}

export interface PoolAsk {
    worker: string;
    cpus: number;
}

export interface PoolWorker {
    worker: string;
    cpus: number;
    seenAt: string;
    took: string;
}

interface Waiter {
    worker: string;
    resolve: (response: Response) => void;
    timer: ReturnType<typeof setTimeout>;
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
    return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isCount(value: unknown): value is number {
    return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

// The body is exactly {"units": [...]} and an optional "priority", each unit a whole protocol.Unit. The wire reads
// only its run and unit id; the rest is the runner's to check, and goes out exactly as it came in. Go marshals an
// empty slice as null, so null is none, and leaves a zero priority off.
export function checkPoolUnits(body: string): PoolBatch | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the units are not JSON';
    }
    if (!isPlainObject(parsed)) {
        return 'the body is a JSON object';
    }
    for (const key of Object.keys(parsed)) {
        if (key !== 'units' && key !== 'priority') {
            return `unknown field ${JSON.stringify(key)}; the body has units and an optional priority`;
        }
    }
    if (!('units' in parsed)) {
        return 'the body has units';
    }
    const priority = parsed.priority ?? 0;
    if (!isCount(priority) || priority > MaximumPoolPriority) {
        return `priority is a whole number from 0 to ${MaximumPoolPriority}`;
    }
    if (parsed.units === null) {
        return { units: [], priority: priority };
    }
    if (!Array.isArray(parsed.units)) {
        return 'units is a list';
    }
    const units: PoolUnit[] = [];
    for (const [index, unit] of (parsed.units as unknown[]).entries()) {
        if (!isPlainObject(unit)) {
            return `unit ${index} is not a JSON object`;
        }
        if (typeof unit.run !== 'string' || !RunIdPattern.test(unit.run)) {
            return `unit ${index} needs a run, a run id the wire takes`;
        }
        if (
            typeof unit.unit !== 'string' ||
            unit.unit === '' ||
            textEncoder.encode(unit.unit).length > MaximumUnitIdLength
        ) {
            return `unit ${index} needs a unit id of 1 to ${MaximumUnitIdLength} bytes`;
        }
        const json = JSON.stringify(unit);
        if (textEncoder.encode(json).length > MaximumPoolUnitBytes) {
            return `unit ${index} is longer than ${MaximumPoolUnitBytes} bytes`;
        }
        units.push({ run: unit.run, unit: unit.unit, json: json });
    }
    return { units: units, priority: priority };
}

// The ask is exactly {"worker", "cpus"}: a worker name, and the cores it has. A zero cpus may be left off, as Go's
// omitempty would.
export function checkPoolAsk(body: string): PoolAsk | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the ask is not JSON';
    }
    if (!isPlainObject(parsed)) {
        return 'the body is a JSON object';
    }
    for (const key of Object.keys(parsed)) {
        if (key !== 'worker' && key !== 'cpus') {
            return `unknown field ${JSON.stringify(key)}; an ask is exactly worker and cpus`;
        }
    }
    if (
        typeof parsed.worker !== 'string' ||
        parsed.worker === '' ||
        parsed.worker.length > MaximumWorkerNameLength
    ) {
        return `worker is a name of 1 to ${MaximumWorkerNameLength} characters`;
    }
    const cpus = parsed.cpus ?? 0;
    if (!isCount(cpus)) {
        return 'cpus is a whole number, not negative';
    }
    return { worker: parsed.worker, cpus: cpus };
}

// A retire's body is exactly {"worker", "reason"}, a restore's exactly {"worker"}.
export function checkPoolRetire(body: string, retiring: boolean): string | { worker: string; reason: string } {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the body is not JSON';
    }
    if (!isPlainObject(parsed)) {
        return 'the body is a JSON object';
    }
    const wanted = retiring ? ['reason', 'worker'] : ['worker'];
    if (Object.keys(parsed).sort().join(',') !== wanted.join(',')) {
        return retiring ? 'the body has exactly worker and reason' : 'the body has exactly worker';
    }
    if (
        typeof parsed.worker !== 'string' ||
        parsed.worker === '' ||
        parsed.worker.length > MaximumWorkerNameLength
    ) {
        return `worker is a name of 1 to ${MaximumWorkerNameLength} characters`;
    }
    const reason = retiring ? parsed.reason : '';
    if (typeof reason !== 'string' || (retiring && (reason === '' || reason.length > 1024))) {
        return 'reason is a sentence of 1 to 1024 characters';
    }
    return { worker: parsed.worker, reason: reason };
}

// A cancel's or a queued's body is exactly {"run"}.
export function checkPoolRun(body: string): string | { run: string } {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the body is not JSON';
    }
    if (!isPlainObject(parsed)) {
        return 'the body is a JSON object';
    }
    const keys = Object.keys(parsed);
    if (keys.length !== 1 || keys[0] !== 'run') {
        return 'the body has exactly run';
    }
    if (typeof parsed.run !== 'string' || !RunIdPattern.test(parsed.run)) {
        return 'run is a run id the wire takes';
    }
    return { run: parsed.run };
}

function unitResponse(json: string): Response {
    return new Response(json + '\n', {
        headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' },
    });
}

function nothingResponse(): Response {
    return new Response(null, { status: 204, headers: { 'Cache-Control': 'no-store' } });
}

export class Pool extends DurableObject<Env> {
    private readonly sql: SqlStorage;
    // The asks waiting for a unit, oldest first. They live in memory: an evicted object drops them, and each
    // instance just asks again.
    private readonly waiters: Waiter[] = [];
    // How long a next waits. Production keeps PoolWaitMilliseconds; a test shortens it on its own pool's object.
    waitMilliseconds = PoolWaitMilliseconds;

    constructor(context: DurableObjectState, environment: Env) {
        super(context, environment);
        this.sql = context.storage.sql;
        // seenAt is this object's own clock in milliseconds, since the ten-minute window is its call.
        this.sql.exec(`
            CREATE TABLE IF NOT EXISTS units (
                position INTEGER PRIMARY KEY,
                run TEXT NOT NULL,
                unit TEXT NOT NULL,
                json TEXT NOT NULL,
                priority INTEGER NOT NULL DEFAULT 0
            );
            CREATE INDEX IF NOT EXISTS unitsByRun ON units (run);
            CREATE TABLE IF NOT EXISTS workers (
                worker TEXT PRIMARY KEY,
                cpus INTEGER NOT NULL,
                seenAt INTEGER NOT NULL,
                took TEXT NOT NULL DEFAULT ''
            );
            CREATE TABLE IF NOT EXISTS retired (
                worker TEXT PRIMARY KEY,
                reason TEXT NOT NULL
            );
        `);
        // A pool made before priorities has units without the column; it is added in place, its units at 0.
        const columns = this.sql.exec<{ name: string }>('PRAGMA table_info(units)').toArray();
        if (
            !columns.some(function (column) {
                return column.name === 'priority';
            })
        ) {
            this.sql.exec('ALTER TABLE units ADD COLUMN priority INTEGER NOT NULL DEFAULT 0');
        }
        this.sql.exec('CREATE INDEX IF NOT EXISTS unitsByPriority ON units (priority DESC, position)');
    }

    override async fetch(request: Request): Promise<Response> {
        const operation = new URL(request.url).pathname;
        if (operation === '/units' && request.method === 'POST') {
            return this.acceptUnits(request);
        }
        if (operation === '/next' && request.method === 'POST') {
            return this.next(request);
        }
        if (operation === '/cancel' && request.method === 'POST') {
            return this.cancel(request);
        }
        if (operation === '/queued' && request.method === 'POST') {
            return this.queued(request);
        }
        if ((operation === '/retire' || operation === '/restore') && request.method === 'POST') {
            return this.retire(request, operation === '/retire');
        }
        if (operation === '/status' && request.method === 'GET') {
            return jsonResponse(200, { queued: this.queuedCount(), workers: this.workers() });
        }
        return jsonResponse(404, { error: 'no such pool operation' });
    }

    // ---------- Units ----------

    // The units join the end of their priority's queue in the order given, all or none, then go straight to whoever waits.
    private async acceptUnits(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumUnitsBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `the units are at most ${MaximumUnitsBodyBytes} bytes` });
        }
        const batch = checkPoolUnits(body);
        if (typeof batch === 'string') {
            return jsonResponse(400, { error: batch });
        }
        this.ctx.storage.transactionSync(() => {
            for (const unit of batch.units) {
                this.sql.exec(
                    'INSERT INTO units (run, unit, json, priority) VALUES (?, ?, ?, ?)',
                    unit.run,
                    unit.unit,
                    unit.json,
                    batch.priority,
                );
            }
        });
        this.handOut();
        return jsonResponse(200, { queued: this.queuedCount() });
    }

    private queuedCount(): number {
        return this.sql.exec<{ count: number }>('SELECT COUNT(*) AS count FROM units').one().count;
    }

    // Takes the next unit off the queue for this worker, the highest priority's oldest, and records it as the one the
    // worker last took.
    private take(worker: string): string | null {
        return this.ctx.storage.transactionSync(() => {
            const first = this.sql
                .exec<{ position: number; unit: string; json: string }>(
                    'SELECT position, unit, json FROM units ORDER BY priority DESC, position LIMIT 1',
                )
                .toArray()[0];
            if (first === undefined) {
                return null;
            }
            this.sql.exec('DELETE FROM units WHERE position = ?', first.position);
            this.sql.exec('UPDATE workers SET took = ? WHERE worker = ?', first.unit, worker);
            return first.json;
        });
    }

    // Answers the waiting asks, oldest first, while there are units for them.
    private handOut(): void {
        for (;;) {
            const waiter = this.waiters[0];
            if (waiter === undefined) {
                return;
            }
            const json = this.take(waiter.worker);
            if (json === null) {
                return;
            }
            this.waiters.shift();
            clearTimeout(waiter.timer);
            waiter.resolve(unitResponse(json));
        }
    }

    // ---------- Next ----------

    private async next(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumAskBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `an ask is at most ${MaximumAskBodyBytes} bytes` });
        }
        const ask = checkPoolAsk(body);
        if (typeof ask === 'string') {
            return jsonResponse(400, { error: ask });
        }
        this.sql.exec(
            `INSERT INTO workers (worker, cpus, seenAt) VALUES (?, ?, ?)
             ON CONFLICT (worker) DO UPDATE SET cpus = excluded.cpus, seenAt = excluded.seenAt`,
            ask.worker,
            ask.cpus,
            Date.now(),
        );
        // A retired worker is refused, never handed a unit: the runner reads a 403 as the pool turning it away and
        // stops serving, so an instance that can't hold a unit stops taking them (Oct 9: a8ff7263308d, its read-only
        // home under /tmp beside the tree, failed every unit it took for its disk).
        const retired = this.sql
            .exec<{ reason: string }>('SELECT reason FROM retired WHERE worker = ?', ask.worker)
            .toArray()[0];
        if (retired !== undefined) {
            return jsonResponse(403, { error: `worker ${ask.worker} is retired from this pool: ${retired.reason}` });
        }
        // A serve asks one question at a time, so a new ask from the same worker means its last one was dropped on
        // the way (a connection cut, a client timeout). That ask is answered 204 now, before a unit can go to a
        // caller nobody hears.
        for (const stale of this.waiters.filter(function (waiter) {
            return waiter.worker === ask.worker;
        })) {
            this.forget(stale);
            stale.resolve(nothingResponse());
        }
        const json = this.take(ask.worker);
        if (json !== null) {
            return unitResponse(json);
        }
        return new Promise<Response>((resolve) => {
            const waiter: Waiter = {
                worker: ask.worker,
                resolve: resolve,
                timer: setTimeout(() => {
                    this.forget(waiter);
                    resolve(nothingResponse());
                }, this.waitMilliseconds),
            };
            this.waiters.push(waiter);
        });
    }

    private forget(waiter: Waiter): void {
        clearTimeout(waiter.timer);
        const index = this.waiters.indexOf(waiter);
        if (index >= 0) {
            this.waiters.splice(index, 1);
        }
    }

    // ---------- Cancel ----------

    private async cancel(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumAskBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `a cancel is at most ${MaximumAskBodyBytes} bytes` });
        }
        const cancel = checkPoolRun(body);
        if (typeof cancel === 'string') {
            return jsonResponse(400, { error: cancel });
        }
        const dropped = this.sql.exec('DELETE FROM units WHERE run = ? RETURNING position', cancel.run).toArray();
        return jsonResponse(200, { dropped: dropped.length });
    }

    // ---------- Queued ----------

    // The run's units still in the queue, oldest first. A coordinator tells a unit waiting its turn from one a worker
    // took and never started (handed to an ask whose turn had ended), and queues that one again.
    private async queued(request: Request): Promise<Response> {
        const body = await readBodyText(request, MaximumAskBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `a queued is at most ${MaximumAskBodyBytes} bytes` });
        }
        const queued = checkPoolRun(body);
        if (typeof queued === 'string') {
            return jsonResponse(400, { error: queued });
        }
        const rows = this.sql
            .exec<{ unit: string }>('SELECT unit FROM units WHERE run = ? ORDER BY position', queued.run)
            .toArray();
        return jsonResponse(200, {
            units: rows.map(function (row) {
                return row.unit;
            }),
        });
    }

    // ---------- Retire ----------

    // Retires a worker by name, or restores one, so the operator can take an unfit instance out of the pool without
    // reaching it. The body is {"worker", "reason"} to retire and {"worker"} to restore.
    private async retire(request: Request, retiring: boolean): Promise<Response> {
        const body = await readBodyText(request, MaximumAskBodyBytes);
        if (body === null) {
            return jsonResponse(413, { error: `a retire is at most ${MaximumAskBodyBytes} bytes` });
        }
        const retire = checkPoolRetire(body, retiring);
        if (typeof retire === 'string') {
            return jsonResponse(400, { error: retire });
        }
        if (retiring) {
            this.sql.exec(
                'INSERT INTO retired (worker, reason) VALUES (?, ?) ON CONFLICT (worker) DO UPDATE SET reason = excluded.reason',
                retire.worker,
                retire.reason,
            );
        }
        else {
            this.sql.exec('DELETE FROM retired WHERE worker = ?', retire.worker);
        }
        const rows = this.sql.exec<{ worker: string }>('SELECT worker FROM retired ORDER BY worker').toArray();
        return jsonResponse(200, {
            retired: rows.map(function (row) {
                return row.worker;
            }),
        });
    }

    // ---------- Workers ----------

    // Every worker seen in the last four hours, by name; older ones are forgotten here, lazily. A retired worker is
    // no capacity, so it isn't listed (Oct 9: 8 switched-off box workers and 11 silent instances read as 19 of 70).
    private workers(): PoolWorker[] {
        this.sql.exec('DELETE FROM workers WHERE seenAt <= ?', Date.now() - WorkerWindowMilliseconds);
        return this.sql
            .exec<{ worker: string; cpus: number; seenAt: number; took: string }>(
                'SELECT worker, cpus, seenAt, took FROM workers WHERE worker NOT IN (SELECT worker FROM retired) ORDER BY worker',
            )
            .toArray()
            .map(function (row) {
                return { worker: row.worker, cpus: row.cpus, seenAt: new Date(row.seenAt).toISOString(), took: row.took };
            });
    }
}
