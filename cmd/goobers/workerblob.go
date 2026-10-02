package main

import (
	"context"
	"os"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workerblob"
)

func openWorkerBlobStore(root, directory, endpoint, dispatchNamespace string) (blobstore.Store, error) {
	dispatchEndpoint := ""
	if dispatchNamespace != "" {
		dispatchEndpoint = os.Getenv("GOOBERS_BLOB_ENDPOINT")
	}
	var source func() (string, error)
	if endpoint != "" || dispatchEndpoint != "" {
		cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			return nil, err
		}
		signer, err := podTokenMinter(cfg)
		if err != nil {
			return nil, err
		}
		if signer != nil {
			owner := workerDivergenceWorkerID()
			source = func() (string, error) { return signer.MintWorkerBlob(owner, 2*time.Minute) }
		}
	}
	return workerblob.Open(context.Background(), directory, endpoint, dispatchEndpoint, source)
}
