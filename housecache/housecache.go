// Package housecache is the house cache (docs/house-cache.md): one box per house keeps the store's by-hash objects on
// its disk and serves them to the house's other boxes, so each blob crosses the house's shared internet link once,
// not once per box. It is a plain content-addressed pull-through cache: a blob's name is its sha256, so it never
// changes and needs no invalidation, and every client checks every hash as it always has, so a bad cache can cost a
// fetch again, never a wrong byte.
//
// It serves exactly two kinds of path, both named by their content: the action store's blobs/<sha256> (test binaries,
// products' archives, a tree's source and module cache) and the release store's releases/blobs/<sha256> (Loom's own
// binaries, a pool's runners). Nothing else: a tree's index trees/<key>.json is written again under the same key when
// a better build replaces it (builder.writeIndex), a ref may be pointed again (builder.replaceGone), and the release
// manifest releases/current.txt changes with every release, so each of those is read from the store, always.
package housecache

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
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

// HeaderTimeout bounds a client's wait for the house cache's answer. A miss is answered once the cache holds the whole
// blob, checked, so this is the time to fetch the largest blob (a tree's source, about 450 MB) over the house's shared
// link with every box asking; past it the client reads from the store, and the cache's fetch goes on for the others.
const HeaderTimeout = 5 * time.Minute

// byHashPattern is every path the house cache serves: the action store's blobs and the release store's, by sha256.
var byHashPattern = regexp.MustCompile(`^/(?:releases/)?blobs/([0-9a-f]{64})$`)

// ByHash is the sha256 a request path names, when it is one the house cache serves.
func ByHash(path string) (string, bool) {
	match := byHashPattern.FindStringSubmatch(path)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// Through is where a client asks the house cache for the object at upstream: the cache's address and upstream's path.
// It is empty when there is no cache, or when upstream isn't a by-hash path the cache serves, so a client asks the
// cache only for what it holds and reads everything else from the store.
func Through(cache, upstream string) string {
	if cache == "" {
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

// Client is an HTTP client for the house cache: a connection within ConnectTimeout, an answer within HeaderTimeout,
// and never a proxy, since the cache is on the house's own network. The body is bounded by the caller's context.
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

// carrierGrade is 100.64.0.0/10, the shared address space a tailnet (Tailscale) gives its machines: reachable only
// from the tailnet, so as local as the house's own network.
var carrierGrade = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// CheckListen refuses an address the house cache mustn't listen on: anything but an IP address and port, and, unless
// public is set, every address but one on a local network (private, loopback, link-local or a tailnet's). Every
// address (0.0.0.0, ::) is refused too, since it includes any public address the box has, an IPv6 one included.
func CheckListen(address string, public bool) error {
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
	case !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !carrierGrade.Contains(ip):
		return fmt.Errorf("listen %q isn't an address on a local network: the house cache serves the house, never the internet (public = yes forces it)", address)
	}
	return nil
}
