package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func cacheUnit() Unit {
	return Unit{Run: "r-1", Unit: "tests[shard=0]", Argv: []string{"./x.test", "-test.run", "A"},
		Environment: map[string]string{"GOFLAGS": "-count=1", "A": "1"}, Directory: "pkg",
		Inputs:         []Input{{Path: "x.test", Sha256: strings.Repeat("a", 64), Mode: "755"}, {Path: "src", Sha256: strings.Repeat("b", 64), Archive: "tar"}},
		Outputs:        []Output{{Glob: "out/*.json"}},
		TimeoutSeconds: 600, Store: &Endpoint{Url: "https://wire/runs/r-1/blobs"}, Token: "t"}
}

// One change per part of the key. Each must turn a hit into a miss with the real key, and must not with a
// mutant that drops that part: so each part is load-bearing and its test would catch it going missing.
var cacheKeyChanges = map[string]func(unit *Unit, runnerVersion *string, platform *string){
	"argv": func(u *Unit, _ *string, _ *string) { u.Argv[2] = "B" },
	"test": func(u *Unit, _ *string, _ *string) {
		u.Test = &TestJob{Repository: AdamicRepository, Sha: strings.Repeat("c", 40), Packages: []TestPackage{{Package: AdamicModule}}}
	},
	"environment":    func(u *Unit, _ *string, _ *string) { u.Environment["A"] = "2" },
	"directory":      func(u *Unit, _ *string, _ *string) { u.Directory = "other" },
	"inputs":         func(u *Unit, _ *string, _ *string) { u.Inputs[0].Sha256 = strings.Repeat("e", 64) },
	"outputs":        func(u *Unit, _ *string, _ *string) { u.Outputs[0].Glob = "out/*.txt" },
	"timeoutSeconds": func(u *Unit, _ *string, _ *string) { u.TimeoutSeconds = 10 },
	"runnerVersion":  func(_ *Unit, v *string, _ *string) { *v = "v2" },
	"platform":       func(_ *Unit, _ *string, p *string) { *p = "darwin/arm64" },
}

func TestEveryPartOfTheCacheKeyIsLoadBearing(t *testing.T) {
	parts := cacheKeyParts(cacheUnit(), "v1", "linux/amd64")
	if len(parts) != len(cacheKeyChanges) {
		t.Fatalf("the key has %d parts and %d have a change; every part needs one", len(parts), len(cacheKeyChanges))
	}
	for _, part := range parts {
		change, found := cacheKeyChanges[part.name]
		if !found {
			t.Fatalf("part %s has no change that tests it", part.name)
		}
		unit, version, platform := cacheUnit(), "v1", "linux/amd64"
		changed, changedVersion, changedPlatform := cacheUnit(), "v1", "linux/amd64"
		change(&changed, &changedVersion, &changedPlatform)
		if CacheKey(unit, version, platform) == CacheKey(changed, changedVersion, changedPlatform) {
			t.Errorf("%s: changing it still hits", part.name)
		}
		mutant := func(u Unit, v string, p string) string { return cacheKeyOf(cacheKeyParts(u, v, p), part.name) }
		if mutant(unit, version, platform) != mutant(changed, changedVersion, changedPlatform) {
			t.Errorf("%s: a key without it still misses, so this test couldn't see it dropped", part.name)
		}
	}
}

func TestTheCacheKeyIgnoresWhereAUnitRan(t *testing.T) {
	unit := cacheUnit()
	elsewhere := cacheUnit()
	elsewhere.Run, elsewhere.Unit, elsewhere.Token = "r-2", "other", "u"
	elsewhere.Store, elsewhere.Wire = &Endpoint{Url: "https://wire/runs/r-2/blobs"}, &Endpoint{Url: "https://wire/runs/r-2/events"}
	elsewhere.Resources = Resources{Cpus: 64}
	// The same environment written in another order is the same environment.
	elsewhere.Environment = map[string]string{"A": "1", "GOFLAGS": "-count=1"}
	if CacheKey(unit, "v1", "linux/amd64") != CacheKey(elsewhere, "v1", "linux/amd64") {
		t.Fatal("the same work in another run misses")
	}
	if key := CacheKey(unit, "v1", "linux/amd64"); !Sha256Pattern.MatchString(key) {
		t.Fatalf("key %q", key)
	}
}

func TestCacheEntryShape(t *testing.T) {
	entry := CacheEntry{Key: strings.Repeat("a", 64), Run: "r-1", Unit: "u", Events: strings.Repeat("b", 64)}
	if err := CheckCacheEntry(entry); err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(entry)
	if !strings.Contains(string(text), `"outputs":[]`) {
		t.Fatalf("no outputs as []: %s", text)
	}
	for name, breakIt := range map[string]func(*CacheEntry){
		"bad key":    func(e *CacheEntry) { e.Key = "x" },
		"bad run":    func(e *CacheEntry) { e.Run = "a/b" },
		"no unit":    func(e *CacheEntry) { e.Unit = "" },
		"no events":  func(e *CacheEntry) { e.Events = "" },
		"bad output": func(e *CacheEntry) { e.Outputs = []CacheOutput{{Path: "x", Sha256: "y"}} },
	} {
		broken := entry
		breakIt(&broken)
		if CheckCacheEntry(broken) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
