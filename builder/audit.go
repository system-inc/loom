package builder

import (
	"sort"
	"strings"
)

// An AuditReport is the store checked against itself, listed through R2's S3 interface (ListObjectsV2) with the
// builder's key. A runner reads a ref, or a tree's index, and then the blob it names, so a ref written within
// FreshFor whose blob is gone is drift: Dangling. So is a ref whose blob was uploaded more than FreshFor before it
// (Stale), since that blob expires days before its ref, which Publish never writes. A ref older than FreshFor is
// near its own end, and the lifecycle may take its blob a little before it, so it is only counted (Expiring).
type AuditReport struct {
	Refs, Blobs, Trees int
	Expiring           int
	Dangling           []string `json:",omitempty"`
	Stale              []string `json:",omitempty"`
}

// Drift reports whether a fresh ref names a blob the store lacks or one about to expire before it.
func (report AuditReport) Drift() bool {
	return len(report.Dangling) > 0 || len(report.Stale) > 0
}

// Audit lists the store's refs, blobs and trees, and reads each ref to check the blob it names.
func Audit(store Store) (AuditReport, error) {
	store.read()
	refs, err := store.Bucket.List("refs/action/")
	if err != nil {
		return AuditReport{}, err
	}
	store.read()
	blobs, err := store.Bucket.List("blobs/")
	if err != nil {
		return AuditReport{}, err
	}
	store.read()
	trees, err := store.Bucket.List("trees/")
	if err != nil {
		return AuditReport{}, err
	}
	uploaded := map[string]int64{}
	for _, blob := range blobs {
		uploaded[strings.TrimPrefix(blob.Key, "blobs/")] = blob.Modified.Unix()
	}
	report := AuditReport{Refs: len(refs), Blobs: len(blobs), Trees: len(trees)}
	for _, ref := range refs {
		key := strings.TrimPrefix(ref.Key, "refs/action/")
		if store.now().Sub(ref.Modified) >= FreshFor {
			report.Expiring++
			continue
		}
		named, err := store.heldRef(key)
		if err != nil {
			return AuditReport{}, err
		}
		blobUploaded, held := uploaded[named.Sum]
		switch {
		case named.Sum == "" || !held:
			report.Dangling = append(report.Dangling, key)
		case ref.Modified.Unix()-blobUploaded > int64(FreshFor.Seconds()):
			report.Stale = append(report.Stale, key)
		}
	}
	sort.Strings(report.Dangling)
	sort.Strings(report.Stale)
	return report, nil
}
