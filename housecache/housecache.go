// Package housecache is the house cache (docs/house-cache.md): one box per house keeps the store's by-hash objects on
// its disk and serves them to the house's other boxes, so each blob crosses the house's shared internet link once,
// not once per box. It is a plain content-addressed pull-through cache: a blob's name is its sha256, so it never
// changes and needs no invalidation, and every client checks every hash as it always has, so a bad cache can cost a
// fetch again, never a wrong byte.
//
// It serves exactly three kinds of path, each named by its content: the action store's blobs/<sha256> (test binaries,
// products' archives, a tree's source chunks and module cache), the release store's releases/blobs/<sha256> (Loom's
// own binaries, a pool's runners) and gate-inputs/<sha256> (the gate inputs' chunks). Nothing else: a tree's index
// trees/<key>.json is written again under the same key when a better build replaces it (builder.writeIndex), a ref may
// be pointed again (builder.replaceGone), the release manifest releases/current.txt changes with every release, and
// the gate inputs' manifest, gate-inputs/<the tar's sha256>, isn't named by its own bytes (the cache refuses it, as
// every object that doesn't hash to its name), so each of those is read from the store, always.
package housecache

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultUpstream is the store the house cache fills from: the action store's public domain, whose releases/ prefix
// is the release store.
const DefaultUpstream = "https://artifacts.loom.system.inc"

// DefaultPort is the house cache's port.
const DefaultPort = 7380

// ConnectTimeout bounds a client's connection to the house cache: one that is down costs a client this, then the
// store.
const ConnectTimeout = 2 * time.Second

// HeaderTimeout bounds a client's wait for the house cache's answer to one ask. A hit is answered at once, and a miss
// as soon as the store answers the cache, its bytes streamed as they arrive (Server), so this bounds a cache that is
// frozen, never one fetching over a slow link. Past it the client reads that blob from the store. A variable only so
// tests can shorten it.
var HeaderTimeout = 15 * time.Second

// IdleWindow and IdleBytes bound a body's trickle: a client gives up on the house cache's answer once a window passes
// in which it sent fewer than IdleBytes (and wasn't done), and reads that blob from the store. On the house's network
// a healthy cache sends megabytes a second. Variables only so tests can shorten them.
var (
	IdleWindow = 10 * time.Second
	IdleBytes  = int64(64 << 10)
)

// SkipFor is how long every client in a process leaves a house cache alone once it failed to answer and then failed a
// probe: an unreachable host costs one connect timeout, not one per blob.
const SkipFor = 3 * time.Minute

// byHashPattern is every path the house cache serves: the action store's blobs, the release store's and the gate
// inputs' chunks, each by its sha256.
var byHashPattern = regexp.MustCompile(`^/(?:releases/blobs|blobs|gate-inputs)/([0-9a-f]{64})$`)

// ByHash is the sha256 a request path names, when it is one the house cache serves.
func ByHash(path string) (string, bool) {
	match := byHashPattern.FindStringSubmatch(path)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// Through is where a client asks the house cache for the object at upstream: the cache's address and upstream's path.
// It is empty when there is no cache, when this process is skipping it (Unanswered), or when upstream isn't a by-hash
// path the cache serves, so a client asks the cache only for what it holds and reads everything else from the store.
func Through(cache, upstream string) string {
	if cache == "" || Skipping(cache) {
		return ""
	}
	parsed, err := url.Parse(upstream)
	if err != nil || parsed.RawQuery != "" {
		return ""
	}
	if _, ok := ByHash(parsed.Path); !ok {
		return ""
	}
	return strings.TrimSuffix(cache, "/") + parsed.Path
}

// skipped holds, by cache address, until when this process leaves that house cache alone.
var skipped sync.Map

// Skipping says whether this process is leaving the house cache at cache alone for now.
func Skipping(cache string) bool {
	until, found := skipped.Load(strings.TrimSuffix(cache, "/"))
	return found && time.Now().Before(until.(time.Time))
}

// probing holds the caches being probed, so concurrent failures probe once.
var probing sync.Map

// Unanswered judges a house cache that didn't answer an ask whole: no connection, no answer within HeaderTimeout, or a
// body cut off or trickling. It probes the cache in the background, never holding the caller, who reads the store at
// once; unless the cache answers within ConnectTimeout, as one that is only slow to fetch a large blob does, every
// client in this process leaves it alone for SkipFor, so a cache that is down or frozen costs one wait, not one per
// blob.
func Unanswered(cache string) {
	cache = strings.TrimSuffix(cache, "/")
	if Skipping(cache) {
		return
	}
	if _, already := probing.LoadOrStore(cache, true); already {
		return
	}
	go func() {
		defer probing.Delete(cache)
		probe := &http.Client{Timeout: ConnectTimeout, Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: ConnectTimeout}).DialContext}}
		if response, err := probe.Get(cache + "/"); err == nil {
			response.Body.Close()
			return
		}
		skipped.Store(cache, time.Now().Add(SkipFor))
	}()
}

// Watch is body, cut off by cancel (the request's own) once a window of IdleWindow passes in which fewer than
// IdleBytes arrived: a house cache that stalls or trickles never holds a client longer than that. Closing it ends the
// watch.
func Watch(body io.ReadCloser, cancel context.CancelFunc) io.ReadCloser {
	watched := &watchedBody{body: body, done: make(chan struct{})}
	go watched.watch(cancel, IdleWindow, IdleBytes)
	return watched
}

type watchedBody struct {
	body  io.ReadCloser
	count atomic.Int64
	done  chan struct{}
	once  sync.Once
}

func (watched *watchedBody) Read(buffer []byte) (int, error) {
	count, err := watched.body.Read(buffer)
	watched.count.Add(int64(count))
	return count, err
}

func (watched *watchedBody) Close() error {
	watched.once.Do(func() { close(watched.done) })
	return watched.body.Close()
}

func (watched *watchedBody) watch(cancel context.CancelFunc, window time.Duration, least int64) {
	ticker := time.NewTicker(window)
	defer ticker.Stop()
	seen := int64(0)
	for {
		select {
		case <-watched.done:
			return
		case <-ticker.C:
			now := watched.count.Load()
			if now-seen < least {
				cancel()
				return
			}
			seen = now
		}
	}
}

// Client is an HTTP client for the house cache: a connection within ConnectTimeout, an answer within HeaderTimeout,
// and never a proxy, since the cache is on the house's own network. Its body is the caller's to Watch.
func Client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: HeaderTimeout,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}}
}

// CheckURL refuses a client's house-cache setting that isn't http://<host>:<port>: a plain address on the house's
// network, with no path, query, user or fragment.
func CheckURL(value string) error {
	parsed, err := url.Parse(value)
	switch {
	case err != nil:
		return fmt.Errorf("house-cache %q: %w", value, err)
	case parsed.Scheme != "http" || parsed.Host == "" || parsed.Port() == "":
		return fmt.Errorf("house-cache %q isn't http://<host>:<port>", value)
	case parsed.User != nil || strings.TrimSuffix(parsed.Path, "/") != "" || parsed.RawQuery != "" || parsed.Fragment != "":
		return fmt.Errorf("house-cache %q is more than http://<host>:<port>", value)
	}
	return nil
}

// Variable carries the house cache's address to a runner serve hands a unit to, and is what a command that reads the
// store (`loom-runner run` and `serve`, `loom fetch-actions`) asks when no --house-cache is given.
const Variable = "LOOM_HOUSE_CACHE"

// SettingKey is a client's setting in ~/.loom/update.conf: house-cache = http://<host>:<port>.
const SettingKey = "house-cache"

// Setting is the house cache a box's update.conf names, read as the updater reads it: the last house-cache = <value>
// line, spaces around the key and the value ignored; empty when there is none. A value that isn't
// http://<host>:<port> is refused.
func Setting(updateConf string) (string, error) {
	value := ""
	for _, line := range strings.Split(updateConf, "\n") {
		key, after, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(key) == SettingKey && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			value = strings.TrimSpace(after)
		}
	}
	if value == "" {
		return "", nil
	}
	return value, CheckURL(value)
}

// carrierGrade is 100.64.0.0/10, the shared address space a tailnet (Tailscale) gives its machines, and a carrier's
// NAT gives its customers: local only on a tailnet, so taken only when the settings say tailnet.
var carrierGrade = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// CheckListen refuses an address the house cache mustn't listen on: anything but an IP address and port, and, unless
// public is set, every address but one on a local network (private, loopback or link-local, or with tailnet set a
// tailnet's 100.64.0.0/10). Every address (0.0.0.0, ::) is refused too, since it includes any public address the box
// has, an IPv6 one included.
func CheckListen(address string, public, tailnet bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen %q isn't <ip>:<port>: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || port == "" {
		return fmt.Errorf("listen %q isn't <ip>:<port>: the house cache listens on one address it is told, never a name", address)
	}
	if public {
		return nil
	}
	switch {
	case ip.IsUnspecified():
		return fmt.Errorf("listen %q is every address this box has, which can include a public one: name its address on the house's network", address)
	case carrierGrade.Contains(ip) && !tailnet:
		return fmt.Errorf("listen %q is in 100.64.0.0/10, a tailnet's or a carrier's NAT: tailnet = yes says it is a tailnet's", address)
	case !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !carrierGrade.Contains(ip):
		return fmt.Errorf("listen %q isn't an address on a local network: the house cache serves the house, never the internet (public = yes forces it)", address)
	}
	return nil
}
