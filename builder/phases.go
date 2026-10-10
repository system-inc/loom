package builder

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// A tree build is measured by phase (#s0cqqhk), so a lever that makes it faster is a number against a number, never
// a guess: build-tree prints its phases in its summary line and keeps them in its live status for `loom top`, and the
// tree builder keeps them on the build's ledger record, so a tree's phases outlive its build.

// TreePhases are one tree build's wall times, in seconds, by phase. Readying through Removal are laps of one clock,
// end to end, so they add up to Total but for the moment between the last lap and the summary; Checkout is the tree
// builder's, before build-tree starts, and outside Total. WarmTests and WarmMains split Warm, the Upload fields that
// name what went up split Upload, and the product counts and their seconds split Products by what buildcache did with
// each product: Fetched, a product the store held, and Built, one it compiled. Those seconds are each product's own,
// summed: products build side by side, so they add up to more than Products' wall time.
type TreePhases struct {
	// Checkout is the tree builder's checkout of the future into its clone (`loom build-trees` only).
	Checkout float64 `json:"checkout,omitempty"`
	// Readying is everything before the first go process: the tree's identity, the store, Go's build cache trimmed,
	// the floors read, the tree's directory and its lock.
	Readying   float64 `json:"readying"`
	NpmInstall float64 `json:"npmInstall"`
	// Listing is go list, of the test packages and of the product tests.
	Listing   float64 `json:"listing"`
	Warm      float64 `json:"warm"`
	WarmTests float64 `json:"warmTests"`
	WarmMains float64 `json:"warmMains"`
	Products  float64 `json:"products"`
	// ProductsFetched and ProductsBuilt count products by their key, once each however many product tests read them.
	ProductsFetched        int     `json:"productsFetched"`
	ProductsFetchedSeconds float64 `json:"productsFetchedSeconds"`
	ProductsBuilt          int     `json:"productsBuilt"`
	ProductsBuiltSeconds   float64 `json:"productsBuiltSeconds"`
	Binaries               float64 `json:"binaries"`
	// SourceChunks is the tree's source and its npm projects cut into chunks; ModuleCache, its module cache archived.
	SourceChunks   float64 `json:"sourceChunks"`
	ModuleCache    float64 `json:"moduleCache"`
	Upload         float64 `json:"upload"`
	UploadModules  float64 `json:"uploadModules"`
	UploadChunks   float64 `json:"uploadChunks"`
	UploadProducts float64 `json:"uploadProducts"`
	UploadBinaries float64 `json:"uploadBinaries"`
	UploadIndex    float64 `json:"uploadIndex"`
	// Removal is the tree's working directory removed once its index is up (TreeDone).
	Removal float64 `json:"removal"`
	Total   float64 `json:"total"`
}

// censusLine is a line buildcache's record writes to ADAMIC_BUILD_LOG (adamic's internal/buildcache record()): build
// <name> <key12> hit|fetched|audited|miss|off <seconds>.
var censusLine = regexp.MustCompile(`(?m)^build \S+ ([0-9a-f]{12}) (hit|fetched|audited|miss|off) ([0-9]+(?:\.[0-9]+)?)$`)

// ProductCensus reads the product tests' build logs (Products' product-<n>.log under logs) and counts the products
// buildcache fetched from the store and those it built (a miss, or an audit, which builds what it fetched to check
// it), each key once, with the seconds every such line took summed. A hit is a product an earlier product test of this
// tree already put in the cache, and costs nothing here; off is no cache at all. A log that can't be read counts
// nothing: the census measures a build and never fails one.
func ProductCensus(logs string) (fetched int, fetchedSeconds float64, built int, builtSeconds float64) {
	files, _ := filepath.Glob(filepath.Join(logs, "product-*.log"))
	fetchedKeys, builtKeys := map[string]bool{}, map[string]bool{}
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, match := range censusLine.FindAllStringSubmatch(string(content), -1) {
			seconds, _ := strconv.ParseFloat(match[3], 64)
			switch match[2] {
			case "fetched":
				fetchedKeys[match[1]] = true
				fetchedSeconds += seconds
			case "miss", "audited":
				builtKeys[match[1]] = true
				builtSeconds += seconds
			}
		}
	}
	return len(fetchedKeys), fetchedSeconds, len(builtKeys), builtSeconds
}
