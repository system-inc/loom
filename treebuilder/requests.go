package treebuilder

import (
	"fmt"
	"time"

	"github.com/system-inc/loom/jsonlines"
)

// Base trees on demand (#6ygdzat): Judge reruns a failing unit alone on its future's base, main's commit, and the rerun
// runs that commit's tree build (#v03v751), which no planned future may have asked for. So Judge asks the builder in
// a request file beside the ledger, one Request a line, which Judge holds and appends to and the builder reads each pass
// without a lock (jsonlines.Read). A requested tree is built like a listed one, ahead of the listing's, since one base
// tree serves every future on that base; the rerun waits on its index as the placer's units wait on theirs.

// A Request is one tree Judge asked the builder for: its key, the commit to check out, the Go release Judge keyed it
// with (planner.ReadCommitIdentity), which build-tree refuses to build with another, and when it was first asked.
type Request struct {
	Tree   string `json:"tree"`
	Commit string `json:"commit"`
	Go     string `json:"go"`
	At     string `json:"at"`
}

// Asked is when the request was made; an unreadable time is the zero time, as old as can be.
func (request Request) Asked() time.Time {
	at, _ := time.Parse(time.RFC3339, request.At)
	return at
}

// Requests is Judge's request file, held for its process's life, each tree's first request held.
type Requests struct {
	file  *jsonlines.File[Request]
	first map[string]Request
	order []Request
}

// OpenRequests takes the request file at path, refused while another Judge holds it.
func OpenRequests(path string) (*Requests, error) {
	file, records, err := jsonlines.Open[Request](path, "another judge holds it")
	if err != nil {
		return nil, err
	}
	requests := &Requests{file: file, first: map[string]Request{}}
	for _, record := range records {
		requests.hold(record)
	}
	return requests, nil
}

func (requests *Requests) hold(record Request) {
	if _, found := requests.first[record.Tree]; !found {
		requests.first[record.Tree] = record
	}
	requests.order = append(requests.order, record)
}

// Ask asks for the tree once: a tree already asked for keeps its first request, which Ask returns, so a wait is counted
// from it across Judge's restarts.
func (requests *Requests) Ask(request Request) (Request, error) {
	if first, found := requests.first[request.Tree]; found {
		return first, nil
	}
	if err := requests.file.Append(request); err != nil {
		return Request{}, fmt.Errorf("asking for tree %s: %w", request.Tree, err)
	}
	requests.hold(request)
	return request, nil
}

// Compact keeps the requests made within keep of now, so a tree asked for again after them is asked anew.
func (requests *Requests) Compact(keep time.Duration, now time.Time) error {
	kept := []Request{}
	for _, record := range requests.order {
		if now.Sub(record.Asked()) < keep {
			kept = append(kept, record)
		}
	}
	if len(kept) == len(requests.order) {
		return nil
	}
	if err := requests.file.Rewrite(kept); err != nil {
		return err
	}
	requests.first, requests.order = map[string]Request{}, nil
	for _, record := range kept {
		requests.hold(record)
	}
	return nil
}

// Close gives the request file up.
func (requests *Requests) Close() error {
	return requests.file.Close()
}

// ReadRequests reads the request file at path without taking it, as the builder does while Judge holds it: each tree's
// first request, in the order asked, within keep of now (zero: all). A missing file has none.
func ReadRequests(path string, keep time.Duration, now time.Time) ([]Request, error) {
	records, err := jsonlines.Read[Request](path)
	if err != nil {
		return nil, err
	}
	requests, seen := []Request{}, map[string]bool{}
	for _, record := range records {
		if seen[record.Tree] || (keep > 0 && now.Sub(record.Asked()) >= keep) {
			continue
		}
		seen[record.Tree] = true
		requests = append(requests, record)
	}
	return requests, nil
}
