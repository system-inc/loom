package builder

import (
	"strings"
	"testing"
)

func TestTheAuditFindsWhatTheIndexClaimsAndTheStoreLacks(t *testing.T) {
	store := newFakeStore()
	product := keyOf("product")
	runs := 0
	index, err := OpenIndex(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	served := serve(t, store, "workshop")
	builder := Builder{Store: served, Scratch: t.TempDir(), Cache: t.TempDir(), Index: index,
		Run: fakeRun(map[string]map[string]string{product: {"bin/tool": "t", "data": "d"}}, &runs), Key: func(Action) (string, error) { return keyOf("A"), nil }}
	builder.Build([]Action{{Directory: "x", Test: "TestProduct_A"}})
	// Another tier's blob and a ref from before the index: reported, never drift.
	store.blobs[keyOf("floor1 blob")] = []byte("x")
	store.refs[keyOf("written before the index")] = keyOf("some manifest")

	report, err := Audit(served, index)
	if err != nil {
		t.Fatal(err)
	}
	// One ref; the tool, the data, the .inputs and the manifest.
	if report.Drift() || report.IndexRefs != 1 || report.IndexBlobs != 4 || report.UnindexedRefs != 1 || report.StoreBlobsOutsideIndex != 1 {
		t.Fatalf("an honest store: %+v", report)
	}

	// The store loses a blob and a ref the index claims: drift, named.
	var lostBlob string
	for sum := range index.blobs {
		lostBlob = sum
		break
	}
	delete(store.blobs, lostBlob)
	delete(store.refs, keyOf("A"))
	report, err = Audit(served, index)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Drift() || len(report.MissingRefs) != 1 || report.MissingRefs[0] != keyOf("A") || len(report.MissingBlobs) != 1 || report.MissingBlobs[0] != lostBlob {
		t.Fatalf("a store that lost what the index claims: %+v", report)
	}

	if _, err = Audit(Store{Read: served.Read, Write: served.Write, Token: "publish:box"}, index); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a listing without a build token: %v", err)
	}
}
