# The house cache

The big house (Cloud, Server, Home and Chonchon) shares one internet link, 20 MB/s down. A cold tree costs each runner up to about 3.7 GB of blobs (the source's chunks, test binaries, products), so four boxes fetching the same tree pull 11 to 15 GB through that link. The house cache keeps one copy on one box of the house and serves it to the others over the house's own network, so each blob crosses the link once per house.

It is a plain content-addressed pull-through cache (package `housecache`). Every object it serves is named by its sha256, so it never changes and needs no invalidation, and every client checks every hash exactly as it does without the cache, so a bad cache can cost a download from the store, never a wrong byte.

## What it serves

Exactly three kinds of path, at the store's own paths:

| Path | What it is |
|---|---|
| `/blobs/<sha256>` | The action store's blobs (builder/store.go): test binaries, products' archives, a tree's source chunks and module cache. |
| `/releases/blobs/<sha256>` | The release store's blobs (docs/updater.md): `loom` and `loom-runner` of every release, which the updater installs and serve fetches as a unit's runner. |
| `/gate-inputs/<sha256>` | The gate inputs' chunks, which prepare.sh fetches. |

Nothing else, and nothing else ever reaches the store through it: any other path is 404. `releases/current.txt` changes with every release, a tree's index `trees/<key>.json` is written again under its key when a build with fewer failures replaces it (builder's `writeIndex`), and a ref may be pointed at a rebuilt archive (`replaceGone`), so each of those is read from the store, always, and the cache can never serve one stale. The gate inputs' manifest shares the chunks' prefix but is named by its tar's sha256, not its own, so its bytes never hash to its name: the cache refuses it like any corrupt object and keeps nothing, and prepare.sh never asks it. They are a few kilobytes each; the blobs are the gigabytes. A blob held serves every path naming its sha256, since the bytes are the same; a miss is fetched by its own path, so `/blobs/X`, gone after the action store's 7 days, never makes `/releases/blobs/X` wait or fail.

- **A hit** is served from disk with its length, and hashed as it goes out: a copy the disk corrupted is removed once found, and its client, which refuses it, reads that blob from the store.
- **A miss** is fetched from `https://artifacts.loom.system.inc` once, however many clients ask for its path meanwhile, and streamed to each of them as it arrives: every answer starts as soon as the store answers, so on the big house's shared link no client waits on a whole blob, and the four boxes of a cold tree read it through one fetch. A `HEAD` of a miss is 404 and fetches nothing. The blob goes into a partial file, hashed as it arrives, and only a whole blob that hashes to its name is made read-only and renamed to it. Each answer holds back its last byte until then: bytes that don't hash to the name, or a store that drops midway or sends fewer than 64 KB in any 10 s (stalled or trickling, as a client judges the cache; the big house's downloads go over a cellular line that can stall mid-body), cut every answer short (aborted), so each client's read fails and it reads the store, and nothing is kept. A store that refuses before answering (404, an error, no room) is passed on as a status (404, 502, 503).
- **The disk is bounded**: before a blob is fetched, and after, the least recently served blobs are removed until the cache holds at most `limit-gb`, fetches in flight counted at the length the store announced, and its disk keeps `floor-gb` free once they are whole. A blob that can't fit is never fetched (503), and its clients read from the store.
- **It listens on one address**, an IP address on a local network (private, loopback or link-local), never `0.0.0.0` or `::`, which include any public address the box has, an IPv6 one included. An address in 100.64.0.0/10 is a tailnet's or a carrier's NAT, taken only with `tailnet = yes`; `public = yes` forces anything else.

## The clients

Every Loom program that reads a blob by its sha256 reads one setting, the `house-cache` line of the box's `~/.loom/update.conf`:

```
house-cache = http://10.10.102.20:7380
```

| Client | How it reads the setting | What it asks the cache for |
|---|---|---|
| The updater (`loom-update.sh`) | `update.conf` every run, or `LOOM_UPDATE_HOUSE_CACHE` | each release blob it downloads, `<house><base's path>/blobs/<sha256>` |
| Serve (`loom-runner serve`) | `install-serve` renders it into `loom-serve.service` as `--house-cache` | every blob of a prebuilt unit, every product archive, and the runner a unit names |
| A runner serve hands a unit to | serve passes it as `LOOM_HOUSE_CACHE`, never a flag, so a pinned runner from before the house cache still runs (it ignores the variable) | the same |
| `loom-runner run`, `loom fetch-actions` | `--house-cache`, else `LOOM_HOUSE_CACHE` | the same |
| prepare.sh | the runner passes it as `LOOM_HOUSE_CACHE` | each chunk of the gate inputs, never their manifest |

Each client tries the cache first and reads from the store directly on any failure: no connection within 2 s, no answer within 15 s (a miss is answered as soon as the store answers the cache, so this catches a frozen cache), a body that sends fewer than 64 KB in any 10 s (stalled or trickling), any status but 200, a cut body, or bytes that don't hash to the name. The cache's ask has its own context inside the caller's, so a cache that is down, frozen or crawling costs a blob those seconds, then the store, and never the unit's time. A cache that failed that way is probed in the background; unless it answers within 2 s, as one only slow to fetch a large blob does, every client in the process (serve and its units, a runner, `fetch-actions`) leaves it alone for 3 minutes and reads the store, so an unreachable host costs one connect, not one per blob. The updater and prepare.sh, one short run each, stop asking for the rest of their run after the first file the cache doesn't give. A unit's record says where each blob came from (`from the house cache`, or `from the store, the house cache having failed (<why>)`), the source's chunks are summed with their own house cache figure, and the fetching ends with `N blobs: X bytes from the store, Z bytes in K blobs from the cache, Y bytes from the house cache`, which is the per-box measurement; the updater logs `(N downloaded (M through the house cache), L linked)` and a line for the file the cache didn't give.

## Hosting it

Any Linux box can host. Hosting is two things: `~/.loom/house-cache.conf` on that box, and the `loom-house-cache` systemd user unit, which `loom house-cache install` renders from it.

| Key | Meaning |
|---|---|
| `listen` | Required: `<ip>:<port>`, the box's address on the house's network. |
| `limit-gb` | The most gigabytes of blobs kept, default 100 (about 27 cold trees). |
| `floor-gb` | The gigabytes kept free on the cache's disk, default 20. |
| `directory` | Where the blobs are kept, an absolute path, default `~/loom-house-cache`. |
| `tailnet` | `yes` allows an address in 100.64.0.0/10, a tailnet's; default `no`. |
| `public` | `yes` allows an address off the local network; default `no`. |

`loom house-cache install` reads and checks the settings first, then writes the updater's hook (`~/.loom/updated.d/40-house-cache`, which runs it after every release) and the unit, each only when its text changed, and only then touches the cache: starts it when it isn't running, restarts it when its unit changed or it runs another binary than `~/.loom/bin/loom`, and otherwise leaves it alone. On a box without `house-cache.conf` it stops the cache and removes the unit and the hook. A restart costs the clients only a read of the store for what they ask meanwhile. `journalctl --user -u loom-house-cache` shows each miss served, each refusal and each eviction.

## Installing: Cloud hosts, the house reads through it

Once a release with `loom house-cache` is installed on every box of the house (`~/.loom/bin/loom 2>&1 | grep -q 'house-cache install'`). Cloud's address on the house's network is 10.10.102.20; reserve it in the router's DHCP, since the cache listens on that exact address and every client names it.

On Cloud, the host:

```bash
ip -4 -brief address | grep -q ' 10\.10\.102\.20/' && echo 'Cloud is 10.10.102.20'
printf 'listen = 10.10.102.20:7380\n' > ~/.loom/house-cache.conf     # limit-gb = 100 and floor-gb = 20 unless set
~/.loom/bin/loom house-cache install                                 # writes the hook and the unit, starts the cache
systemctl --user status loom-house-cache
sum=$(sha256sum ~/.loom/bin/loom | cut -c1-64)
curl -fsS -o /dev/null -w '%{http_code} %{size_download}\n' http://10.10.102.20:7380/releases/blobs/${sum}   # 200, loom's size
```

If Cloud runs a firewall, let the house in: `sudo ufw allow from 10.10.102.0/24 to any port 7380 proto tcp` (the house's subnet, if it is a /24).

On Cloud, Server, Home and Chonchon, the clients (Cloud reads through its own cache too):

```bash
echo 'house-cache = http://10.10.102.20:7380' >> ~/.loom/update.conf
~/.loom/bin/loom-runner install-serve                                # rewrites loom-serve with --house-cache, reloads it (a drain)
grep -o -- '--house-cache [^ ]*' ~/.config/systemd/user/loom-serve.service
```

The updater reads the line on its next run; serve takes it once the unit in hand finishes. To measure, compare a cold tree's `N blobs:` lines on each box before the line is added and after.

## Moving it

Moving the house cache is the settings file moving and one line on each client. On the new host, say Server at `<server-ip>`: its `house-cache.conf` and `~/.loom/bin/loom house-cache install`, as above. On every client, the line: `sed -i 's#^house-cache = .*#house-cache = http://<server-ip>:7380#' ~/.loom/update.conf && ~/.loom/bin/loom-runner install-serve`. On the old host, last: `rm ~/.loom/house-cache.conf && ~/.loom/bin/loom house-cache install`, which stops the cache and removes its unit and hook. A client that still names the old host meanwhile reads from the store. Removing the line and running `install-serve` turns the house cache off for that box.

## Tests

`go test ./housecache/ ./runner/ ./builder/ ./serving/ ./gateinputs/ ./cmd/loom/` and `updater/update_test.sh` (gateinputs' prepare.sh tests need coreutils' `sha256sum`, so they run on Linux and skip elsewhere): a miss fills the cache and a hit is served from disk with its length, once the store no longer holds it too; the store's corrupt bytes are refused with nothing kept; eight concurrent misses make one fetch; four askers stream one fetch, each with its first byte long before the blob is whole; a store dropping midway, or a blob that hashes wrong (its length known or not), cuts every answer short before its last byte and keeps nothing; four boxes missing one blob over a slow link ask the store once; the bound evicts the least recently served; the floor evicts, and refuses a blob that can't fit; only by-hash paths are served, so `current.txt`, a tree's index and a ref are 404 and never reach the store; a blob corrupted on disk is removed once served; a stalled or trickling store is abandoned within a window, and the next ask fetches on a new connection; one server per directory; an address off the house, or a tailnet's unasked, is refused; raw request lines reach the store only for a by-hash path; a `HEAD` of a miss fetches nothing; an expired blob's fetch never fails its release's; a gate inputs chunk is served and their manifest never. Clients: three boxes through the cache make one fetch from the store; a dead cache falls back within its connect timeout; a tampered blob from the cache is refused and read from the store, by the runner, the store reader and the runner fetch alike; an unreachable or frozen cache costs one wait for 32 blobs, not one per wave; a trickling or frozen one falls back well within the unit's time; prepare.sh is given the cache unless the process skips it, and asks it for every chunk and never the manifest, falling back once; the store reader's ask ends with its caller's context; serve passes the cache to a named runner by its environment; `install-serve` renders the line and reloads serve when it changes, and refuses a line that isn't an address; `install` starts, restarts on a release or new settings, and removes the cache from a box that no longer hosts. The updater takes blobs from the cache, never `current.txt` (the cache's stale one is ignored), refuses a corrupt one and downloads it and the rest from the base, and falls back once when the cache is down. The unit has run only in these tests, never under systemd: Cloud is its first real run.
