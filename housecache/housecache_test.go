package housecache

import (
	"strings"
	"testing"
)

// A client asks the house cache only for a by-hash path, at the cache's address with the store's path.
func TestThroughMapsOnlyByHashPaths(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	cases := map[string]string{
		"https://artifacts.loom.system.inc/blobs/" + sum:             "http://192.168.1.20:7380/blobs/" + sum,
		"https://artifacts.loom.system.inc/releases/blobs/" + sum:    "http://192.168.1.20:7380/releases/blobs/" + sum,
		"https://artifacts.loom.system.inc/releases/current.txt":     "",
		"https://artifacts.loom.system.inc/trees/" + sum + ".json":   "",
		"https://artifacts.loom.system.inc/refs/action/" + sum:       "",
		"https://artifacts.loom.system.inc/blobs/" + sum + "?x=1":    "",
		"https://artifacts.loom.system.inc/blobs/" + sum[:63]:        "",
		"https://runs.loom.system.inc/public/blobs/" + sum:           "",
		"https://artifacts.loom.system.inc/blobs/" + sum + "/../../": "",
	}
	for upstream, want := range cases {
		if got := Through("http://192.168.1.20:7380/", upstream); got != want {
			t.Errorf("Through(%s) is %q, not %q", upstream, got, want)
		}
	}
	if got := Through("", "https://artifacts.loom.system.inc/blobs/"+sum); got != "" {
		t.Errorf("with no cache Through is %q", got)
	}
}

func TestAClientsSettingIsAPlainAddress(t *testing.T) {
	for _, good := range []string{"http://192.168.1.20:7380", "http://cloud.local:7380/", "http://[fd00::20]:7380"} {
		if err := CheckURL(good); err != nil {
			t.Errorf("%s refused: %v", good, err)
		}
	}
	for _, bad := range []string{"", "192.168.1.20:7380", "https://192.168.1.20:7380", "http://192.168.1.20", "http://192.168.1.20:7380/blobs",
		"http://user@192.168.1.20:7380", "http://192.168.1.20:7380?x=1", "ftp://192.168.1.20:7380"} {
		if err := CheckURL(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}

// The house cache listens on one address on the house's network, never every address and never a public one unless
// forced.
func TestAListenOutsideTheHouseIsRefused(t *testing.T) {
	for _, local := range []string{"192.168.1.20:7380", "10.0.0.5:7380", "172.16.4.2:7380", "127.0.0.1:7380", "[::1]:7380", "[fd12::1]:7380",
		"169.254.10.10:7380", "100.101.102.103:7380"} {
		if err := CheckListen(local, false); err != nil {
			t.Errorf("%s refused: %v", local, err)
		}
	}
	for _, public := range []string{"0.0.0.0:7380", "[::]:7380", ":7380", "8.8.8.8:7380", "[2001:4860::8888]:7380", "cloud.local:7380", "192.168.1.20"} {
		if err := CheckListen(public, false); err == nil {
			t.Errorf("%q taken", public)
		}
	}
	if err := CheckListen("0.0.0.0:7380", false); err == nil || !strings.Contains(err.Error(), "every address") {
		t.Errorf("every address refused as %v, not as every address", err)
	}
	if err := CheckListen("8.8.8.8:7380", true); err != nil {
		t.Errorf("a forced public address refused: %v", err)
	}
	if err := CheckListen("cloud.local:7380", true); err == nil {
		t.Error("a name taken even forced")
	}
}
