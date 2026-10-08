// Blobs in R2 (loom-store), addressed by sha256 under blobs/<sha256>. A PUT hands its body straight to R2
// with the expected hash, and R2 verifies it: a body that doesn't match is never written. No JavaScript
// touches the bytes on the way, because per-chunk work here is CPU time: a tee into a DigestStream used 2.2 s
// of CPU on a 100 MiB PUT and was killed for exceeding the limit whenever the client sent fast (Oct 8, from a
// Codex instance at 13 to 20 MB/s). An existing blob is left exactly as it is. Who may reach
// which blob is the run object's decision (docs/protocol.md, Store); this file only moves bytes.

import { jsonResponse } from './Http';

export const MaximumBlobBytes = 100 * 1024 * 1024;
export const Sha256Pattern = /^[0-9a-f]{64}$/;

export function blobKey(sha256: string): string {
    return `blobs/${sha256}`;
}

// The answer to a PUT, and the blob's size when it is in the store afterwards (stored now or already there),
// which is when the run records it as uploaded.
export interface BlobPut {
    response: Response;
    bytes: number | null;
}

export async function blobExists(store: R2Bucket, sha256: string): Promise<boolean> {
    return (await store.head(blobKey(sha256))) !== null;
}

// HEAD answers from the object's metadata, so asking whether a blob is held never reads its bytes.
export async function headBlob(store: R2Bucket, sha256: string): Promise<Response> {
    const object = await store.head(blobKey(sha256));
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

export async function putBlob(store: R2Bucket, sha256: string, request: Request): Promise<BlobPut> {
    const lengthText = request.headers.get('Content-Length');
    if (lengthText === null || !/^\d+$/.test(lengthText)) {
        return refused(jsonResponse(411, { error: 'a blob PUT needs a Content-Length' }));
    }
    const length = Number(lengthText);
    if (length > MaximumBlobBytes) {
        return refused(jsonResponse(413, { error: `a blob is at most 100 MiB (${MaximumBlobBytes} bytes)` }));
    }
    const existing = await store.head(blobKey(sha256));
    if (existing !== null) {
        await request.body?.cancel();
        return {
            response: jsonResponse(200, { sha256: sha256, bytes: existing.size, stored: false }),
            bytes: existing.size,
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
