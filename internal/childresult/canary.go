package childresult

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// ErrBlobPlaneUnverified withholds engine child workflows from a worker that
// cannot read what control writes: the canary control put in its object
// store is not readable through the blob plane the worker's stage pods use,
// so a resumed parent could never read its child's result (#8085 d, #8188).
var ErrBlobPlaneUnverified = errors.New("child workflows withheld: the stage blob plane does not serve control's object store (control's canary blob is not readable through it; #8188)")

// BlobPlaneCanary is the instance's fixed canary blob: control puts it in its
// object store at boot, and a worker fetches it by digest through its stage
// blob plane before lending stages child-workflow authority. Both sides
// derive it from the instance identity alone.
func BlobPlaneCanary(instanceID string) (digest string, data []byte) {
	data = []byte("goobers child-continuation canary " + instanceID)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), data
}
