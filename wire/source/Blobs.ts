// Blobs in R2 (loom-store), addressed by sha256 under blobs/<sha256>. A PUT streams its body through a
// SHA-256 digest and into R2 at once; R2 is also given the expected hash, so a body that doesn't match is
// never written, and the digest decides the answer. An existing blob is left exactly as it is. Who may reach
// which blob is the run object's decision (docs/protocol.md, Store); this file only moves bytes.

import { jsonResponse } from './Http';

export const MaximumBlobBytes = 100 * 1024 * 1024;
export const Sha256Pattern = /^[0-9a-f]{64}$/;

export function blobKey(sha256: string): string {
    return `blobs/${sha256}`;
}

function hex(bytes: ArrayBuffer): string {
    return Array.from(new Uint8Array(bytes), function (byte) {
        return byte.toString(16).padStart(2, '0');
    }).join('');
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

    const body = request.body ?? new Response('').body;
    if (body === null) {
        return refused(jsonResponse(400, { error: 'no body' }));
    }
    const [forDigest, forStore] = body.tee();
    const digestStream = new crypto.DigestStream('SHA-256');
    const digesting = forDigest.pipeTo(digestStream).then(function () {
        return digestStream.digest;
    });
    const fixedLength = new FixedLengthStream(length);
    const copying = forStore.pipeTo(fixedLength.writable);
    const storing = store.put(blobKey(sha256), fixedLength.readable, {
        sha256: sha256,
        httpMetadata: { contentType: 'application/octet-stream' },
    });
    const [digested, copied, stored] = await Promise.allSettled([digesting, copying, storing]);

    if (digested.status === 'rejected' || copied.status === 'rejected') {
        return refused(jsonResponse(400, { error: 'the body ended early or ran past its Content-Length' }));
    }
    const actual = hex(digested.value);
    if (actual !== sha256) {
        if (stored.status === 'fulfilled') {
            // R2 checks the hash we gave it, so this should never run; if it does, take the wrong bytes back out.
            await store.delete(blobKey(sha256));
        }
        return refused(jsonResponse(400, { error: `the body hashes to ${actual}, not ${sha256}` }));
    }
    if (stored.status === 'rejected') {
        return refused(jsonResponse(502, { error: `the store refused the blob: ${String(stored.reason)}` }));
    }
    return { response: jsonResponse(201, { sha256: sha256, bytes: length, stored: true }), bytes: length };
}

function refused(response: Response): BlobPut {
    return { response: response, bytes: null };
}
