package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/r2"
)

// storeFlags are how a command that writes the action store finds it: the public domain it reads, and the R2
// bucket it writes straight through the S3 interface, with the key pair in a key = value file (upload.sh's).
type storeFlags struct {
	read        *string
	credentials *string
	bucket      *string
}

func addStoreFlags(flags *flag.FlagSet) storeFlags {
	return storeFlags{
		read:        flags.String("read", "https://artifacts.loom.system.inc", "the public store, read direct"),
		credentials: flags.String("r2", r2.DefaultCredentialsPath(), "the R2 key pair that writes the store: account_id, access_key_id, secret_access_key"),
		bucket:      flags.String("bucket", "loom-artifacts", "the store's R2 bucket"),
	}
}

// open is the store, writable, counting its requests in requests.
func (store storeFlags) open(requests *builder.Requests) (builder.Store, error) {
	credentials, err := r2.ReadCredentials(*store.credentials)
	if err != nil {
		return builder.Store{}, fmt.Errorf("the store's key: %w", err)
	}
	bucket := r2.Open(credentials, *store.bucket)
	return builder.Store{Read: strings.TrimSuffix(*store.read, "/"), Bucket: &bucket, Requests: requests}, nil
}
