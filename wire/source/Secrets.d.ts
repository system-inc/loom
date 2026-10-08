// The Worker's secret, set with `wrangler secret put LOOM_TOKEN_SECRET`; `wrangler types` can't see it.
interface Env {
    LOOM_TOKEN_SECRET: string;
}
declare namespace Cloudflare {
    interface Env {
        LOOM_TOKEN_SECRET: string;
    }
}
