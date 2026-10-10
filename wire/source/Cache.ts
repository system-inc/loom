// The unit cache in R2 under cache/<key>: protocol.CacheEntry, written once by the coordinator after a unit
// passes and read before placing one. The Worker checks the entry's shape the way protocol.CheckCacheEntry
// does, and that every blob it names is in the store, refreshed when stale, since a hit hands those hashes to the next run.

import { holdBlob, Sha256Pattern } from './Blobs';
import { MaximumUnitIdLength, RunIdPattern } from './Events';
import { jsonResponse, readBodyText } from './Http';

export const MaximumCacheEntryBytes = 1024 * 1024;

export interface CacheOutput {
    path: string;
    sha256: string;
    bytes: number;
}

export interface CacheEntry {
    key: string;
    run: string;
    unit: string;
    machine: string;
    runnerVersion: string;
    wallSeconds: number;
    outputs: CacheOutput[];
    events: string;
}

const textEncoder = new TextEncoder();
const entryKeys = ['events', 'key', 'machine', 'outputs', 'run', 'runnerVersion', 'unit', 'wallSeconds'];
const outputKeys = ['bytes', 'path', 'sha256'];

export function cacheKey(key: string): string {
    return `cache/${key}`;
}

function hasExactly(value: unknown, keys: string[]): value is Record<string, unknown> {
    if (typeof value !== 'object' || value === null || Array.isArray(value)) {
        return false;
    }
    const present = Object.keys(value).sort();
    return (
        present.length === keys.length &&
        present.every(function (key, index) {
            return key === keys[index];
        })
    );
}

function isSha256(value: unknown): value is string {
    return typeof value === 'string' && Sha256Pattern.test(value);
}

// The body is exactly the keys protocol.CacheEntry marshals (none is omitempty), with Go's types, and
// CheckCacheEntry's rules on top: a 64-hex key and events, a run id the wire takes, a unit id of 1 to 256 bytes,
// and each output with a path, a sha256 and a size that isn't negative.
export function checkCacheEntry(body: string): CacheEntry | string {
    let parsed: unknown;
    try {
        parsed = JSON.parse(body);
    }
    catch {
        return 'the cache entry is not JSON';
    }
    if (!hasExactly(parsed, entryKeys)) {
        return 'a cache entry has exactly key, run, unit, machine, runnerVersion, wallSeconds, outputs and events';
    }
    if (!isSha256(parsed.key)) {
        return 'the cache key is 64 lowercase hex digits';
    }
    if (typeof parsed.run !== 'string' || !RunIdPattern.test(parsed.run)) {
        return 'a cache entry names the run that proved it, a run id the wire takes';
    }
    if (
        typeof parsed.unit !== 'string' ||
        parsed.unit === '' ||
        textEncoder.encode(parsed.unit).length > MaximumUnitIdLength
    ) {
        return `a cache entry names the unit that proved it, 1 to ${MaximumUnitIdLength} bytes`;
    }
    if (typeof parsed.machine !== 'string' || typeof parsed.runnerVersion !== 'string') {
        return 'machine and runnerVersion are strings';
    }
    if (typeof parsed.wallSeconds !== 'number' || !Number.isFinite(parsed.wallSeconds)) {
        return 'wallSeconds is a number';
    }
    if (!isSha256(parsed.events)) {
        return "a cache entry's events is the sha256 of the unit's event log";
    }
    if (!Array.isArray(parsed.outputs)) {
        return 'outputs is a list, [] when empty';
    }
    const outputs: CacheOutput[] = [];
    for (const output of parsed.outputs as unknown[]) {
        if (
            !hasExactly(output, outputKeys) ||
            typeof output.path !== 'string' ||
            output.path === '' ||
            !isSha256(output.sha256) ||
            typeof output.bytes !== 'number' ||
            !Number.isSafeInteger(output.bytes) ||
            output.bytes < 0
        ) {
            return 'each cache output is exactly a path, a sha256 and a size in bytes';
        }
        outputs.push({ path: output.path, sha256: output.sha256, bytes: output.bytes });
    }
    // Rebuilt in protocol.CacheEntry's field order, so what is stored reads the same as what Go writes.
    return {
        key: parsed.key,
        run: parsed.run,
        unit: parsed.unit,
        machine: parsed.machine,
        runnerVersion: parsed.runnerVersion,
        wallSeconds: parsed.wallSeconds,
        outputs: outputs,
        events: parsed.events,
    };
}

export async function getCacheEntry(store: R2Bucket, key: string): Promise<Response> {
    const object = await store.get(cacheKey(key));
    if (object === null) {
        return jsonResponse(404, { error: `no cache entry ${key}` });
    }
    return new Response(object.body, {
        headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' },
    });
}

export async function putCacheEntry(store: R2Bucket, key: string, request: Request): Promise<Response> {
    const body = await readBodyText(request, MaximumCacheEntryBytes);
    if (body === null) {
        return jsonResponse(413, { error: `a cache entry is at most ${MaximumCacheEntryBytes} bytes` });
    }
    const entry = checkCacheEntry(body);
    if (typeof entry === 'string') {
        return jsonResponse(400, { error: entry });
    }
    if (entry.key !== key) {
        return jsonResponse(400, { error: `the entry's key is ${entry.key}, not the path's ${key}` });
    }
    const named = [
        ...entry.outputs.map(function (output) {
            return output.sha256;
        }),
        entry.events,
    ];
    const held = await Promise.all(
        named.map(function (sha256) {
            return holdBlob(store, sha256);
        }),
    );
    const missing = named.filter(function (_, index) {
        return !held[index];
    });
    if (missing.length > 0) {
        return jsonResponse(409, {
            error: "a cache entry's outputs and event log must be in the store first",
            missing: [...new Set(missing)],
        });
    }
    // Written once: R2 refuses the put when anything is already there, so two writers can't replace each other.
    const written = await store.put(cacheKey(key), JSON.stringify(entry), {
        onlyIf: { etagDoesNotMatch: '*' },
        httpMetadata: { contentType: 'application/json' },
    });
    if (written === null) {
        return jsonResponse(200, { key: key, written: false });
    }
    return jsonResponse(201, { key: key, written: true });
}
