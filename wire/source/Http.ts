// Small HTTP helpers shared by the Worker and the run object.

export function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}): Response {
    return new Response(JSON.stringify(body) + '\n', {
        status: status,
        headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store', ...headers },
    });
}

// Reads the whole body as UTF-8, or returns null once it passes the limit, without buffering past it.
export async function readBodyText(request: Request, limit: number): Promise<string | null> {
    const declared = request.headers.get('Content-Length');
    if (declared !== null && Number(declared) > limit) {
        return null;
    }
    if (request.body === null) {
        return '';
    }
    const reader = request.body.getReader();
    const chunks: Uint8Array[] = [];
    let total = 0;
    for (;;) {
        const { done, value } = await reader.read();
        if (done) {
            break;
        }
        total += value.byteLength;
        if (total > limit) {
            await reader.cancel();
            return null;
        }
        chunks.push(value);
    }
    const bytes = new Uint8Array(total);
    let offset = 0;
    for (const chunk of chunks) {
        bytes.set(chunk, offset);
        offset += chunk.byteLength;
    }
    return new TextDecoder().decode(bytes);
}
