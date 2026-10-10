// loom's board of changes: one line per change on its way to main. The Queue object pushes each change's
// summary here as it moves (contracts v1.1: {change, owner, sha, state, future, units, updatedAt}, at most once a
// second), and the board never polls. It keeps every change still on its way, and a landed, red, parked or refused
// one for a day after it last moved. One Durable Object, named `board`. Viewers watch it over hibernating WebSockets:
// a snapshot when they connect, then each change as it is recorded, so an open page costs nothing between pushes.

import { DurableObject } from 'cloudflare:workers';
import { ChangeIdPattern, ShaPattern } from './Changes';
import { isUtcTime } from './Events';
import { jsonResponse, readBodyText } from './Http';

export const ChangeBoardName = 'board';
export const MaximumChangeSummaryBytes = 64 * 1024;
export const FinishedChangeMilliseconds = 24 * 60 * 60 * 1000;
export const ChangeStates = ['queued', 'building', 'testing', 'landed', 'red', 'parked', 'refused'] as const;
export type ChangeState = (typeof ChangeStates)[number];
const finishedStates: readonly ChangeState[] = ['landed', 'red', 'parked', 'refused'];

export interface ChangeUnits {
    planned: number;
    passed: number;
    failed: number;
    void: number;
}

// A change as the board shows it: Queue's summary, plus when the board first saw it, when it reached the state it is
// in now and when it finished, on the board's own clock (ISO times), so a page can tell how long a change took from
// arriving to landing and how long it has been setting up, building or testing.
export interface BoardLine extends ChangeSummary {
    firstSeenAt: string;
    stateSince: string;
    finishedAt: string | null;
}

export const ChangeBoardSubprotocol = 'loom';

export interface ChangeSummary {
    change: string;
    owner: string;
    sha: string;
    state: ChangeState;
    future: string | null;
    units: ChangeUnits;
    updatedAt: string;
}

function isCount(value: unknown): value is number {
    return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

// Exactly the seven fields, each well formed, so the page can trust what it draws.
export function checkChangeSummary(body: string): ChangeSummary | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the summary is not JSON';
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        return 'the summary is a JSON object';
    }
    const keys = Object.keys(parsed).sort().join(',');
    if (keys !== 'change,future,owner,sha,state,units,updatedAt') {
        return 'the summary is exactly change, owner, sha, state, future, units and updatedAt';
    }
    const fields = parsed as Record<string, unknown>;
    if (typeof fields.change !== 'string' || !ChangeIdPattern.test(fields.change)) {
        return 'change is a change id';
    }
    if (typeof fields.owner !== 'string' || fields.owner === '' || fields.owner.length > 128) {
        return 'owner is a username';
    }
    if (typeof fields.sha !== 'string' || !ShaPattern.test(fields.sha)) {
        return 'sha is a commit';
    }
    if (typeof fields.state !== 'string' || !(ChangeStates as readonly string[]).includes(fields.state)) {
        return `state is one of ${ChangeStates.join(', ')}`;
    }
    if (fields.future !== null && (typeof fields.future !== 'string' || !ShaPattern.test(fields.future))) {
        return 'future is the tested tree, a commit, or null';
    }
    const units = fields.units as Record<string, unknown> | null;
    if (
        typeof units !== 'object' ||
        units === null ||
        Object.keys(units).sort().join(',') !== 'failed,passed,planned,void' ||
        !isCount(units.planned) ||
        !isCount(units.passed) ||
        !isCount(units.failed) ||
        !isCount(units.void)
    ) {
        return 'units is exactly planned, passed, failed and void, each a count';
    }
    if (!isUtcTime(fields.updatedAt)) {
        return 'updatedAt is an RFC 3339 UTC time';
    }
    return parsed as ChangeSummary;
}

// The board object, by loom's binding (pipeline.jsonc). Env is typed from loom-runs's config, which has none.
export function changeBoardOf(environment: Env): DurableObjectStub {
    const namespace = (environment as unknown as { ChangeBoard: DurableObjectNamespace }).ChangeBoard;
    return namespace.get(namespace.idFromName(ChangeBoardName));
}

export class ChangeBoard extends DurableObject<Env> {
    private readonly sql: SqlStorage;

    constructor(context: DurableObjectState, environment: Env) {
        super(context, environment);
        this.sql = context.storage.sql;
        // finishedAt is the board's own clock when a change reached a finished state, null while it is on its way;
        // firstSeenAt when its first summary arrived; stateSince when it reached the state it is in now. A board from
        // before a column gains it, empty.
        this.sql.exec(`
            CREATE TABLE IF NOT EXISTS changes (
                change TEXT PRIMARY KEY,
                summary TEXT NOT NULL,
                finishedAt INTEGER
            ) WITHOUT ROWID;
        `);
        const columns = this.sql.exec<{ name: string }>('PRAGMA table_info(changes)').toArray();
        for (const name of ['firstSeenAt', 'stateSince']) {
            if (!columns.some(function (column) { return column.name === name; })) {
                this.sql.exec(`ALTER TABLE changes ADD COLUMN ${name} INTEGER`);
            }
        }
        context.setWebSocketAutoResponse(new WebSocketRequestResponsePair('ping', 'pong'));
    }

    override async fetch(request: Request): Promise<Response> {
        const operation = new URL(request.url).pathname;
        if (operation === '/change' && request.method === 'POST') {
            const body = await readBodyText(request, MaximumChangeSummaryBytes);
            if (body === null) {
                return jsonResponse(413, { error: `a summary is at most ${MaximumChangeSummaryBytes} bytes` });
            }
            const summary = checkChangeSummary(body);
            if (typeof summary === 'string') {
                return jsonResponse(400, { error: summary });
            }
            this.record(summary);
            return jsonResponse(200, { recorded: true });
        }
        if (operation === '/changes' && request.method === 'GET') {
            return jsonResponse(200, { changes: this.changes() });
        }
        if (operation === '/stream' && request.method === 'GET') {
            return this.openStream(request);
        }
        return jsonResponse(404, { error: 'no such board operation' });
    }

    // The Worker has checked the board token; the answer names the subprotocol the page offered beside it. The
    // snapshot is sent before this handler yields, so no change can slip between it and the frames after.
    private openStream(request: Request): Response {
        if (request.headers.get('Upgrade')?.toLowerCase() !== 'websocket') {
            return jsonResponse(426, { error: 'this endpoint speaks WebSocket' });
        }
        const pair = new WebSocketPair();
        const server = pair[1];
        this.ctx.acceptWebSocket(server);
        server.send(JSON.stringify({ kind: 'snapshot', changes: this.changes() }));
        return new Response(null, { status: 101, webSocket: pair[0], headers: { 'Sec-WebSocket-Protocol': ChangeBoardSubprotocol } });
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

    // A summary older than the one held (an earlier push arriving late) is dropped, and nobody hears of it.
    private record(summary: ChangeSummary): void {
        const held = this.sql
            .exec<{ summary: string }>('SELECT summary FROM changes WHERE change = ?', summary.change)
            .toArray()[0];
        const before = held === undefined ? undefined : (JSON.parse(held.summary) as ChangeSummary);
        if (before !== undefined && Date.parse(before.updatedAt) > Date.parse(summary.updatedAt)) {
            return;
        }
        const now = Date.now();
        const finishedAt = finishedStates.includes(summary.state) ? now : null;
        // A summary in the state the board already holds keeps the time it got there; a new state starts the clock.
        const sameState = before !== undefined && before.state === summary.state ? 1 : 0;
        const row = this.sql
            .exec<{ firstSeenAt: number | null; stateSince: number | null; finishedAt: number | null }>(
                `INSERT INTO changes (change, summary, finishedAt, firstSeenAt, stateSince) VALUES (?, ?, ?, ?, ?)
                 ON CONFLICT (change) DO UPDATE SET summary = excluded.summary,
                 finishedAt = CASE WHEN excluded.finishedAt IS NULL THEN NULL ELSE COALESCE(changes.finishedAt, excluded.finishedAt) END,
                 stateSince = CASE WHEN ? = 1 THEN COALESCE(changes.stateSince, excluded.stateSince) ELSE excluded.stateSince END
                 RETURNING firstSeenAt, stateSince, finishedAt`,
                summary.change,
                JSON.stringify(summary),
                finishedAt,
                now,
                now,
                sameState,
            )
            .one();
        const firstSeenAt = row.firstSeenAt ?? now;
        const frame = JSON.stringify({ kind: 'change', change: lineOf(summary, firstSeenAt, row.stateSince ?? firstSeenAt, row.finishedAt) });
        for (const socket of this.ctx.getWebSockets()) {
            try {
                socket.send(frame);
            }
            catch {
                // A socket that is closing misses the frame; a reconnect starts from a fresh snapshot.
            }
        }
    }

    // Every change on its way and every one finished in the last day, newest move first.
    private changes(): BoardLine[] {
        this.sql.exec('DELETE FROM changes WHERE finishedAt IS NOT NULL AND finishedAt <= ?', Date.now() - FinishedChangeMilliseconds);
        return this.sql
            .exec<{ summary: string; firstSeenAt: number | null; stateSince: number | null; finishedAt: number | null }>(
                'SELECT summary, firstSeenAt, stateSince, finishedAt FROM changes',
            )
            .toArray()
            .map(function (row) {
                const summary = JSON.parse(row.summary) as ChangeSummary;
                const firstSeenAt = row.firstSeenAt ?? Date.parse(summary.updatedAt);
                return lineOf(summary, firstSeenAt, row.stateSince ?? firstSeenAt, row.finishedAt);
            })
            .sort(function (left, right) {
                return Date.parse(right.updatedAt) - Date.parse(left.updatedAt);
            });
    }
}

function lineOf(summary: ChangeSummary, firstSeenAt: number, stateSince: number, finishedAt: number | null): BoardLine {
    return {
        ...summary,
        firstSeenAt: new Date(firstSeenAt).toISOString(),
        stateSince: new Date(stateSince).toISOString(),
        finishedAt: finishedAt === null ? null : new Date(finishedAt).toISOString(),
    };
}
