package builder

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

// List reads every key the action store holds under prefix ("refs", the action refs' product keys, or "blobs", the
// blobs' sha256s), page by page from loom-pipeline's /actions/list with the build token.
func (store Store) List(prefix string) ([]string, error) {
	keys := []string{}
	cursor := ""
	for {
		request, err := http.NewRequest(http.MethodGet, store.Write+"/list?prefix="+prefix+"&cursor="+url.QueryEscape(cursor), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+store.Token)
		if store.Requests != nil {
			store.Requests.Reads.Add(1)
		}
		response, err := store.client().Do(request)
		if err != nil {
			return nil, err
		}
		var page struct {
			Keys   []string `json:"keys"`
			Cursor *string  `json:"cursor"`
		}
		err = json.NewDecoder(response.Body).Decode(&page)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("listing %s: %s", prefix, response.Status)
		}
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", prefix, err)
		}
		keys = append(keys, page.Keys...)
		if page.Cursor == nil {
			return keys, nil
		}
		cursor = *page.Cursor
	}
}

// An AuditReport compares Workshop's index with the store's own listing. Drift is what the index claims and the
// store lacks: a build trusting the index would skip what isn't there, so it pages. A ref only the store holds is
// unseeded (written before the index existed, or by another writer) and is reported; the public bucket's blobs/
// also holds other tiers' blobs, so blobs outside the index are only counted.
type AuditReport struct {
	IndexRefs, IndexBlobs  int
	StoreRefs, StoreBlobs  int
	MissingRefs            []string `json:",omitempty"`
	MissingBlobs           []string `json:",omitempty"`
	UnindexedRefs          int
	StoreBlobsOutsideIndex int
}

// Drift reports whether the index claims anything the store lacks.
func (report AuditReport) Drift() bool {
	return len(report.MissingRefs) > 0 || len(report.MissingBlobs) > 0
}

// Audit lists the store and checks the index against it.
func Audit(store Store, index *Index) (AuditReport, error) {
	refs, err := store.List("refs")
	if err != nil {
		return AuditReport{}, err
	}
	blobs, err := store.List("blobs")
	if err != nil {
		return AuditReport{}, err
	}
	storeRefs, storeBlobs := map[string]bool{}, map[string]bool{}
	for _, key := range refs {
		storeRefs[key] = true
	}
	for _, sum := range blobs {
		storeBlobs[sum] = true
	}
	index.mutex.Lock()
	defer index.mutex.Unlock()
	report := AuditReport{IndexRefs: len(index.refs), IndexBlobs: len(index.blobs), StoreRefs: len(storeRefs), StoreBlobs: len(storeBlobs)}
	for key := range index.refs {
		if !storeRefs[key] {
			report.MissingRefs = append(report.MissingRefs, key)
		}
	}
	for sum := range index.blobs {
		if !storeBlobs[sum] {
			report.MissingBlobs = append(report.MissingBlobs, sum)
		}
	}
	for key := range storeRefs {
		if _, known := index.refs[key]; !known {
			report.UnindexedRefs++
		}
	}
	for sum := range storeBlobs {
		if !index.blobs[sum] {
			report.StoreBlobsOutsideIndex++
		}
	}
	sort.Strings(report.MissingRefs)
	sort.Strings(report.MissingBlobs)
	return report, nil
}
