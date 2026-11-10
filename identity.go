package captain // import "github.com/harbur/captain"

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
)

// buildRecord anchors every step of one captain command invocation to the
// immutable image ID produced by its own build. Once recorded, subsequent
// test and push steps never re-resolve a shared mutable tag (such as
// "latest") to find "their" image: they compare the tag against this ID and
// fail on mismatch instead.
type buildRecord struct {
	// Image is the repository name, e.g. registry.example.com/team/app.
	Image string

	// ID is the immutable image identity ("sha256:...") as reported by the
	// daemon. It never changes for a given build artifact.
	ID string

	// BuildTag is the unique per-build temporary tag used to claim the build
	// output. Empty when the build was skipped in favor of an existing
	// revision image.
	BuildTag string

	// Rev is the git revision the image represents. Empty outside a git
	// repository.
	Rev string
}

var (
	ledgerMu    sync.Mutex
	buildLedger = map[string]buildRecord{}
)

func recordBuild(rec buildRecord) {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	buildLedger[rec.Image] = rec
	debug("Recorded immutable identity %s for %s (build tag %q, rev %q)",
		rec.ID, rec.Image, rec.BuildTag, rec.Rev)
}

func getBuildRecord(image string) (buildRecord, bool) {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	rec, ok := buildLedger[image]
	return rec, ok
}

// resetLedger empties the in-process build ledger. Test helper.
func resetLedger() {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	buildLedger = map[string]buildRecord{}
}

// ExternalRetagError is returned when a shared docker tag no longer points
// at the immutable image produced by this command's own build.
//
// Failure boundary: captain's flock coordination only protects against
// cooperating captain processes. A non-cooperating actor (a manual
// "docker tag", another tool sharing the daemon) can move a shared tag at
// any time. Captain cannot prevent that, but before every test command and
// every push it re-inspects the tag it is about to consume; on drift it
// fails fast with this error instead of testing or publishing another
// build's content under the current commit.
type ExternalRetagError struct {
	Image      string
	Tag        string
	ExpectedID string
	ActualID   string
}

func (e *ExternalRetagError) Error() string {
	return fmt.Sprintf("external retag detected: %s:%s now points to %s, but this command built %s",
		e.Image, e.Tag, shortImageID(e.ActualID), shortImageID(e.ExpectedID))
}

// shortImageID keeps the "sha256:ab12..." prefix but trims long IDs for
// human-readable error messages.
func shortImageID(id string) string {
	const keep = 19
	if len(id) <= keep {
		return id
	}
	return id[:keep] + "..."
}

// newBuildTag returns a docker-valid tag unique per process and invocation,
// so concurrent builds never write to the same tag while building.
func newBuildTag() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// rand.Read failing is not recoverable in any meaningful way; fall
		// back to a pid-only tag which is still unique per process.
		return fmt.Sprintf("captain-b%d", os.Getpid())
	}
	return fmt.Sprintf("captain-b%d-%s", os.Getpid(), hex.EncodeToString(b))
}
