// loom-pipeline's board of changes: one line per change on its way to main. The Queue object pushes each change's
// summary here as it moves (contracts v1.1: {change, owner, sha, state, future, units, updatedAt}, at most once a
// second), and the board never polls. It keeps every change still on its way, and a landed, red, parked or refused
// one for a day after it last moved. One Durable Object, named `board`.

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

// The board object, by loom-pipeline's binding (pipeline.jsonc). Env is typed from loom-wire's config, which has none.
export function changeBoardOf(environment: Env): DurableObjectStub {
    const namespace = (environment as unknown as { ChangeBoard: DurableObjectNamespace }).ChangeBoard;
    return namespace.get(namespace.idFromName(ChangeBoardName));
}

export class ChangeBoard extends DurableObject<Env> {
    private readonly sql: SqlStorage;

    constructor(context: DurableObjectState, environment: Env) {
        super(context, environment);
        this.sql = context.storage.sql;
        // finishedAt is the board's own clock when a change reached a finished state, null while it is on its way.
        this.sql.exec(`
            CREATE TABLE IF NOT EXISTS changes (
                change TEXT PRIMARY KEY,
                summary TEXT NOT NULL,
                finishedAt INTEGER
            ) WITHOUT ROWID;
        `);
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
        return jsonResponse(404, { error: 'no such board operation' });
    }

    // A summary older than the one held (an earlier push arriving late) is dropped.
    private record(summary: ChangeSummary): void {
        const held = this.sql
            .exec<{ summary: string }>('SELECT summary FROM changes WHERE change = ?', summary.change)
            .toArray()[0];
        if (held !== undefined && Date.parse((JSON.parse(held.summary) as ChangeSummary).updatedAt) > Date.parse(summary.updatedAt)) {
            return;
        }
        const finishedAt = finishedStates.includes(summary.state) ? Date.now() : null;
        this.sql.exec(
            `INSERT INTO changes (change, summary, finishedAt) VALUES (?, ?, ?)
             ON CONFLICT (change) DO UPDATE SET summary = excluded.summary, finishedAt = excluded.finishedAt`,
            summary.change,
            JSON.stringify(summary),
            finishedAt,
        );
    }

    // Every change on its way and every one finished in the last day, newest move first.
    private changes(): ChangeSummary[] {
        this.sql.exec('DELETE FROM changes WHERE finishedAt IS NOT NULL AND finishedAt <= ?', Date.now() - FinishedChangeMilliseconds);
        return this.sql
            .exec<{ summary: string }>('SELECT summary FROM changes')
            .toArray()
            .map(function (row) {
                return JSON.parse(row.summary) as ChangeSummary;
            })
            .sort(function (left, right) {
                return Date.parse(right.updatedAt) - Date.parse(left.updatedAt);
            });
    }
}
