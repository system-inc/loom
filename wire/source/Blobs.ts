// Blobs in R2 (loom-runs), addressed by sha256 under blobs/<sha256>. A PUT hands its body straight to R2
// with the expected hash, and R2 verifies it: a body that doesn't match is never written. No JavaScript
// touches the bytes on the way, because per-chunk work here is CPU time: a tee into a DigestStream used 2.2 s
// of CPU on a 100 MiB PUT and was killed for exceeding the limit whenever the client sent fast (Oct 8, from a
// Codex instance at 13 to 20 MB/s). Both buckets delete every blob a fixed time after its upload (their lifecycle
// rules: loom-runs 7 days, loom-artifacts 30), so a blob written or asked for again is kept fresh (refreshBlob), by its
// own bucket's freshness, rather than left to expire under whoever relies on it next. Who may reach which blob is the
// run object's decision (docs/protocol.md, Store); this file only moves bytes.

import { jsonResponse } from './Http';

export const MaximumBlobBytes = 100 * 1024 * 1024;
export const Sha256Pattern = /^[0-9a-f]{64}$/;

const dayMilliseconds = 24 * 60 * 60 * 1000;

// FreshForMilliseconds is how recently an object in loom-artifacts (PublicStore) must have been uploaded to be relied
// on as it is, builder/store.go's FreshFor: the last five of its lifecycle's 30 days are left for whoever reads it, so
// a held artifact is copied once a lifecycle, not every few days.
export const FreshForMilliseconds = 25 * dayMilliseconds;

// RunsFreshForMilliseconds is FreshForMilliseconds for loom-runs (Store), whose lifecycle keeps its 7 days: the last
// two are left for whoever reads a run's blob or a cache entry's.
export const RunsFreshForMilliseconds = 5 * dayMilliseconds;

export function blobKey(sha256: string): string {
    return `blobs/${sha256}`;
}

// fresh says whether an object was uploaded within freshFor, its bucket's FreshForMilliseconds or
// RunsFreshForMilliseconds.
export function fresh(object: R2Object, freshFor: number): boolean {
    return Date.now() - object.uploaded.getTime() < freshFor;
}

// refreshBlob keeps a held blob from expiring: one uploaded more than freshFor ago is written again onto itself, its
// own bytes, which starts its bucket's days over. The bytes stream from R2 back into R2 (the binding has no copy),
// never from the writer and through no JavaScript, and R2 checks them against sha256 again, so a refresh never stores
// anything that doesn't hash to its name. It answers what the store holds afterwards: the object, or null when the blob
// is gone, or what is held doesn't hash to its name, which a writer's verified body then replaces.
export async function refreshBlob(store: R2Bucket, freshFor: number, sha256: string, held: R2Object): Promise<R2Object | null> {
    if (fresh(held, freshFor)) {
        return held;
    }
    const object = await store.get(blobKey(sha256));
    if (object === null) {
        return null;
    }
    try {
        return await store.put(blobKey(sha256), object.body, {
            sha256: sha256,
            httpMetadata: object.httpMetadata,
            customMetadata: object.customMetadata,
        });
    }
    catch (error) {
        if (/checksum|sha-?256|digest/i.test(String(error))) {
            return null;
        }
        throw error;
    }
}

// holdBlob says whether the store holds blob sha256, refreshed when it is stale, so whatever names it next can rely on
// it for the days freshFor leaves of its bucket's lifecycle.
export async function holdBlob(store: R2Bucket, freshFor: number, sha256: string): Promise<boolean> {
    const held = await store.head(blobKey(sha256));
    return held !== null && (await refreshBlob(store, freshFor, sha256, held)) !== null;
}

// The answer to a PUT, and the blob's size when it is in the store afterwards (stored now or already there),
// which is when the run records it as uploaded.
export interface BlobPut {
    response: Response;
    bytes: number | null;
}

// HEAD answers from the object's metadata, so asking whether a blob is held never reads its bytes. A writer asks before
// it skips a PUT, so a held blob gone stale is refreshed first: 200 means the blob is there for the days freshFor leaves
// of its bucket's lifecycle at least.
export async function headBlob(store: R2Bucket, freshFor: number, sha256: string): Promise<Response> {
    const held = await store.head(blobKey(sha256));
    const object = held === null ? null : await refreshBlob(store, freshFor, sha256, held);
    if (object === null) {
        return new Response(null, { status: 404, headers: { 'Cache-Control': 'no-store' } });
    }
    return new Response(null, {
        headers: {
            'Content-Type': 'application/octet-stream',
            'Content-Length': String(object.size),
            'ETag': object.httpEtag,
            'Cache-Control': 'private, max-age=31536000, immutable',
        },
    });
}

export async function getBlob(store: R2Bucket, sha256: string): Promise<Response> {
    const object = await store.get(blobKey(sha256));
    if (object === null) {
        return jsonResponse(404, { error: `no blob ${sha256}` });
    }
    return new Response(object.body, {
        headers: {
            'Content-Type': 'application/octet-stream',
            'Content-Length': String(object.size),
            'ETag': object.httpEtag,
            'Cache-Control': 'private, max-age=31536000, immutable',
        },
    });
}

export async function putBlob(store: R2Bucket, freshFor: number, sha256: string, request: Request): Promise<BlobPut> {
    const lengthText = request.headers.get('Content-Length');
    if (lengthText === null || !/^\d+$/.test(lengthText)) {
        return refused(jsonResponse(411, { error: 'a blob PUT needs a Content-Length' }));
    }
    const length = Number(lengthText);
    if (length > MaximumBlobBytes) {
        return refused(jsonResponse(413, { error: `a blob is at most 100 MiB (${MaximumBlobBytes} bytes)` }));
    }
    // A held blob isn't sent again but refreshed in R2 when stale; one gone meanwhile, or not hashing to its name, is
    // replaced by this body below.
    const existing = await store.head(blobKey(sha256));
    const held = existing === null ? null : await refreshBlob(store, freshFor, sha256, existing);
    if (held !== null) {
        await request.body?.cancel();
        return {
            response: jsonResponse(200, { sha256: sha256, bytes: held.size, stored: false, refreshed: held !== existing }),
            bytes: held.size,
        };
    }

    if (request.body === null) {
        if (length !== 0) {
            return refused(jsonResponse(400, { error: 'no body' }));
        }
    }
    try {
        await store.put(blobKey(sha256), request.body ?? new Uint8Array(0), {
            sha256: sha256,
            httpMetadata: { contentType: 'application/octet-stream' },
        });
    }
    catch (error) {
        const message = String(error);
        // R2 names a checksum it refused; anything else is the store's own failure, or a body cut short.
        if (/checksum|sha-?256|digest/i.test(message)) {
            return refused(jsonResponse(400, { error: `the body doesn't hash to ${sha256}` }));
        }
        if (/length|ended|closed|short|aborted|truncat/i.test(message)) {
            return refused(jsonResponse(400, { error: 'the body ended early or ran past its Content-Length' }));
        }
        return refused(jsonResponse(502, { error: `the store refused the blob: ${message}` }));
    }
    return { response: jsonResponse(201, { sha256: sha256, bytes: length, stored: true }), bytes: length };
}

function refused(response: Response): BlobPut {
    return { response: response, bytes: null };
}
