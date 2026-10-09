// Run tokens, as protocol/token.go mints them:
// <base64url(claims JSON)>.<base64url(HMAC-SHA256(secret, first part))>, base64url without padding.
// Pure Web Crypto, so the Worker, the tests and the smoke script under Node all sign the same bytes.

// The scope strings are the protocol's own spelling, so they stay lowercase here. A board token's run is
// `board`, and it reaches only the board's endpoints. A pool token's run is the pool's name (a run id the wire
// takes), and it reaches only that pool's next.
// A submit token's run is its owner's username, and it reaches only the change endpoints.
export type TokenScope = 'runner' | 'viewer' | 'coordinator' | 'board' | 'pool' | 'publish' | 'publish-candidate' | 'submit';
export const TokenScopes: readonly TokenScope[] = ['runner', 'viewer', 'coordinator', 'board', 'pool', 'publish', 'publish-candidate', 'submit'];

export interface TokenClaims {
    run: string;
    scope: TokenScope;
    expires: number; // Unix seconds
}

export type TokenVerification = { valid: true; claims: TokenClaims } | { valid: false; reason: string };

const textEncoder = new TextEncoder();
const base64UrlPattern = /^[A-Za-z0-9_-]*$/;
const signingKeys = new Map<string, Promise<CryptoKey>>();

export function encodeBase64Url(bytes: Uint8Array): string {
    let binary = '';
    for (const byte of bytes) {
        binary += String.fromCharCode(byte);
    }
    return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// Strict: the URL alphabet, no padding, and only the canonical encoding (unused trailing bits must be zero),
// so exactly one string decodes to a given byte sequence.
export function decodeBase64Url(text: string): Uint8Array | null {
    if (!base64UrlPattern.test(text) || text.length % 4 === 1) {
        return null;
    }
    const standard = text.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (text.length % 4)) % 4);
    let binary: string;
    try {
        binary = atob(standard);
    }
    catch {
        return null;
    }
    const bytes = Uint8Array.from(binary, function (character) {
        return character.charCodeAt(0);
    });
    if (encodeBase64Url(bytes) !== text) {
        return null;
    }
    return bytes;
}

function signingKey(secret: string): Promise<CryptoKey> {
    let key = signingKeys.get(secret);
    if (key === undefined) {
        key = crypto.subtle.importKey('raw', textEncoder.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, [
            'sign',
        ]);
        signingKeys.set(secret, key);
    }
    return key;
}

async function sign(secret: string, firstPart: string): Promise<Uint8Array> {
    const signature = await crypto.subtle.sign('HMAC', await signingKey(secret), textEncoder.encode(firstPart));
    return new Uint8Array(signature);
}

// Compares every byte whatever the first difference, so the time taken says nothing about where it was.
export function constantTimeEqual(left: Uint8Array, right: Uint8Array): boolean {
    if (left.length !== right.length) {
        return false;
    }
    let difference = 0;
    for (let index = 0; index < left.length; index++) {
        difference |= (left[index] ?? 0) ^ (right[index] ?? 0);
    }
    return difference === 0;
}

// Signs claims the way MintToken does: the JSON keys in the struct's order (run, scope, expires).
export async function mintToken(secret: string, claims: TokenClaims): Promise<string> {
    if (claims.run === '' || !Number.isSafeInteger(claims.expires) || claims.expires <= 0) {
        throw new Error('a token needs a run and an expiry');
    }
    if (!TokenScopes.includes(claims.scope)) {
        throw new Error(`unknown token scope ${JSON.stringify(claims.scope)}`);
    }
    const payload = JSON.stringify({ run: claims.run, scope: claims.scope, expires: claims.expires });
    const firstPart = encodeBase64Url(textEncoder.encode(payload));
    return firstPart + '.' + encodeBase64Url(await sign(secret, firstPart));
}

// Checks the signature first, then the claims: exactly run, scope and expires, each well formed, and not expired.
export async function verifyToken(secret: string, token: string, nowSeconds: number): Promise<TokenVerification> {
    const dot = token.indexOf('.');
    if (dot < 0) {
        return { valid: false, reason: 'malformed token' };
    }
    const firstPart = token.slice(0, dot);
    const given = decodeBase64Url(token.slice(dot + 1));
    if (given === null) {
        return { valid: false, reason: 'malformed token signature' };
    }
    if (!constantTimeEqual(given, await sign(secret, firstPart))) {
        return { valid: false, reason: 'bad token signature' };
    }
    const payload = decodeBase64Url(firstPart);
    if (payload === null) {
        return { valid: false, reason: 'malformed token claims' };
    }
    let parsed: unknown;
    try {
        parsed = JSON.parse(new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(payload));
    }
    catch {
        return { valid: false, reason: 'malformed token claims' };
    }
    const claims = checkClaims(parsed);
    if (claims === null) {
        return { valid: false, reason: 'malformed token claims' };
    }
    if (nowSeconds >= claims.expires) {
        return { valid: false, reason: 'token expired' };
    }
    return { valid: true, claims: claims };
}

function checkClaims(parsed: unknown): TokenClaims | null {
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
        return null;
    }
    const keys = Object.keys(parsed).sort();
    if (keys.length !== 3 || keys[0] !== 'expires' || keys[1] !== 'run' || keys[2] !== 'scope') {
        return null;
    }
    const record = parsed as Record<string, unknown>;
    if (typeof record.run !== 'string' || record.run === '') {
        return null;
    }
    if (typeof record.scope !== 'string' || !TokenScopes.includes(record.scope as TokenScope)) {
        return null;
    }
    if (typeof record.expires !== 'number' || !Number.isSafeInteger(record.expires) || record.expires <= 0) {
        return null;
    }
    return { run: record.run, scope: record.scope as TokenScope, expires: record.expires };
}
