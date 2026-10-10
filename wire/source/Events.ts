// The event schema from docs/protocol.md and protocol.Event. Every event carries run, unit, sequence, time and
// type; the other fields depend on the type. Unknown fields are refused, as everywhere in the protocol.
// The Go side writes a type's fields with omitempty, so a zero is left out (an empty output line has no text,
// a zero-byte upload no bytes); a field the type allows but the line lacks reads as its zero. Exit code 0 is
// still written. So those fields are optional here, but a field that is present must have the right shape.

export interface LoomEvent {
    run: string;
    unit: string;
    sequence: number;
    time: string;
    type: string;
    [field: string]: unknown;
}

export type EventCheck = { valid: true; event: LoomEvent } | { valid: false; problem: string };

type FieldCheck = (value: unknown) => boolean;

const sha256Pattern = /^[0-9a-f]{64}$/;
const utcTimePattern = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(\.\d{1,9})?Z$/;
export const MaximumUnitIdLength = 256;

// protocol.RunIdPattern: safe in a URL path and an R2 key. MintToken and CheckUnit hold run ids to it too.
export const RunIdPattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

function isString(value: unknown): boolean {
    return typeof value === 'string';
}

function isNonEmptyString(value: unknown): boolean {
    return typeof value === 'string' && value !== '';
}

function isBoolean(value: unknown): boolean {
    return typeof value === 'boolean';
}

function isInteger(value: unknown): boolean {
    return typeof value === 'number' && Number.isSafeInteger(value);
}

function isCount(value: unknown): boolean {
    return isInteger(value) && (value as number) >= 0;
}

function isSeconds(value: unknown): boolean {
    return typeof value === 'number' && Number.isFinite(value) && value >= 0;
}

function isSha256(value: unknown): boolean {
    return typeof value === 'string' && sha256Pattern.test(value);
}

function isOneOf(...choices: string[]): FieldCheck {
    return function (value: unknown) {
        return typeof value === 'string' && choices.includes(value);
    };
}

function isInputHashes(value: unknown): boolean {
    if (typeof value !== 'object' || value === null || Array.isArray(value)) {
        return false;
    }
    return Object.values(value).every(isSha256);
}

// RFC 3339 in UTC, the form Go's time.RFC3339Nano gives for a UTC time, and a real calendar instant.
export function isUtcTime(value: unknown): boolean {
    if (typeof value !== 'string') {
        return false;
    }
    const match = utcTimePattern.exec(value);
    if (match === null) {
        return false;
    }
    const milliseconds = Date.parse(value);
    if (Number.isNaN(milliseconds)) {
        return false;
    }
    // Date.parse rolls 2026-02-30 over into March; reading the fields back catches it.
    return new Date(milliseconds).toISOString().slice(0, 19) === value.slice(0, 19);
}

// Per type: each field it may carry, and whether it must be present.
const typeFields: Record<string, Record<string, { check: FieldCheck; required: boolean }>> = {
    started: {
        machine: { check: isString, required: false },
        runnerVersion: { check: isString, required: false },
        // The runner binary's own sha256, which a unit key's runner part names; the judge voids an attempt on another.
        runnerSha256: { check: isSha256, required: false },
        cpus: { check: isCount, required: false },
        memoryMegabytes: { check: isCount, required: false },
        inputs: { check: isInputHashes, required: false },
        // The runner's heartbeat: past this silence it says the unit is still running.
        heartbeatSeconds: { check: isSeconds, required: false },
    },
    output: {
        // runner is the runner's own line about the unit (its heartbeat while the unit is silent), never the unit's.
        stream: { check: isOneOf('stdout', 'stderr', 'runner'), required: true },
        text: { check: isString, required: false },
        replaced: { check: isBoolean, required: false },
    },
    exit: {
        code: { check: isInteger, required: false },
        signal: { check: isString, required: false },
        timedOut: { check: isBoolean, required: false },
        wallSeconds: { check: isSeconds, required: false },
        userSeconds: { check: isSeconds, required: false },
        systemSeconds: { check: isSeconds, required: false },
    },
    uploaded: {
        path: { check: isString, required: true },
        sha256: { check: isSha256, required: true },
        bytes: { check: isCount, required: false },
    },
    error: {
        phase: { check: isOneOf('fetch', 'start', 'run', 'upload', 'wire', 'place'), required: true },
        message: { check: isString, required: false },
    },
    cached: {
        key: { check: isSha256, required: true },
        fromRun: { check: isNonEmptyString, required: true },
        events: { check: isSha256, required: true },
    },
    finished: {
        status: { check: isOneOf('passed', 'failed', 'broken'), required: true },
    },
};

const commonFields = new Set(['run', 'unit', 'sequence', 'time', 'type']);

// Checks one JSON line against the schema and the run named by the URL.
export function checkEventLine(line: string, run: string): EventCheck {
    let parsed: unknown;
    try {
        parsed = JSON.parse(line);
    }
    catch {
        return { valid: false, problem: 'not a JSON value' };
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        return { valid: false, problem: 'not a JSON object' };
    }
    const record = parsed as Record<string, unknown>;
    if (typeof record.run !== 'string') {
        return { valid: false, problem: 'run must be a string' };
    }
    if (record.run !== run) {
        return { valid: false, problem: `event of run ${JSON.stringify(record.run)} posted to run ${run}` };
    }
    if (typeof record.unit !== 'string' || record.unit === '' || record.unit.length > MaximumUnitIdLength) {
        return { valid: false, problem: 'unit must be a non-empty string' };
    }
    if (!isCount(record.sequence)) {
        return { valid: false, problem: 'sequence must be a whole number from 0' };
    }
    if (!isUtcTime(record.time)) {
        return { valid: false, problem: 'time must be RFC 3339 in UTC' };
    }
    if (typeof record.type !== 'string' || !(record.type in typeFields)) {
        return { valid: false, problem: `unknown event type ${JSON.stringify(record.type)}` };
    }
    const fields = typeFields[record.type] ?? {};
    for (const key of Object.keys(record)) {
        if (commonFields.has(key)) {
            continue;
        }
        const field = fields[key];
        if (field === undefined) {
            return { valid: false, problem: `unknown field ${JSON.stringify(key)} on a ${record.type} event` };
        }
        if (!field.check(record[key])) {
            return { valid: false, problem: `${record.type} event has a malformed ${key}` };
        }
    }
    for (const [key, field] of Object.entries(fields)) {
        if (field.required && !(key in record)) {
            return { valid: false, problem: `${record.type} event needs ${key}` };
        }
    }
    return { valid: true, event: record as LoomEvent };
}

// A stable form for comparing two events: keys sorted at every depth.
export function canonicalJson(value: unknown): string {
    if (Array.isArray(value)) {
        return '[' + value.map(canonicalJson).join(',') + ']';
    }
    if (typeof value === 'object' && value !== null) {
        const record = value as Record<string, unknown>;
        return (
            '{' +
            Object.keys(record)
                .sort()
                .map(function (key) {
                    return JSON.stringify(key) + ':' + canonicalJson(record[key]);
                })
                .join(',') +
            '}'
        );
    }
    return JSON.stringify(value);
}
