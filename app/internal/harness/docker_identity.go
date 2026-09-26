package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/dockerutil"
)

// dockerIdentity is what the execution cache records about the sandbox it
// keys: the image ID every keyed run uses, and the Docker server's version,
// OS type and architecture. NCPU and MemTotal are the daemon's capacity.
type dockerIdentity struct {
	ImageID  string
	Server   string
	NCPU     int
	MemTotal int64
}

// dockerRunner runs the two probe commands; tests replace it.
var dockerRunner dockerutil.Runner = dockerutil.DefaultRunner

// probeTimeout bounds the whole probe: both commands share it.
const probeTimeout = 15 * time.Second

// probeDocker resolves ref to its local image ID and reads the server
// identity, with two docker calls (`image inspect`, `info`) that share one
// probeTimeout and never outlive ctx. It never pulls. An image that is not
// present locally, an unreadable answer, or an image whose OS differs from the
// server's is an error: the cache is then disabled, and execution itself is
// unchanged.
func probeDocker(ctx context.Context, run dockerutil.Runner, ref string) (dockerIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	image, found, err := dockerutil.InspectImage(ctx, run, ref)
	if err != nil {
		return dockerIdentity{}, err
	}
	if !found {
		return dockerIdentity{}, fmt.Errorf("the image %s is not present locally", ref)
	}
	info, err := dockerutil.ServerInfo(ctx, run)
	if err != nil {
		return dockerIdentity{}, err
	}
	if image.OS != "" && info.OSType != "" && image.OS != info.OSType {
		return dockerIdentity{}, fmt.Errorf("the image OS %q differs from the Docker server OS %q", image.OS, info.OSType)
	}
	return dockerIdentity{ImageID: image.ID, Server: fmt.Sprintf("%s %s/%s", info.ServerVersion, info.OSType, info.Architecture), NCPU: info.NCPU, MemTotal: info.MemTotal}, nil
}
