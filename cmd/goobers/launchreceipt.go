package main

import (
	"path/filepath"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/launchreceipt"
	"github.com/goobers/goobers/internal/podauth"
)

func appendLaunchReceiptHandlerOption(options []httpapi.HandlerOption, root string, verifier podauth.Verifier) ([]httpapi.HandlerOption, error) {
	key, ok := verifier.(launchreceipt.Verifier)
	if !ok {
		return options, nil
	} // No remote dispatch without a shared signer.
	// Deliberately outside runs/ and every stage-mounted data directory.
	store, err := launchreceipt.NewStore(filepath.Join(root, "runtime-launch-receipts"), key)
	if err != nil {
		return nil, err
	}
	return append(options, httpapi.WithLaunchReceiptService(store)), nil
}
