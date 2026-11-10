package captain

import (
	"io/ioutil"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// writeScratchContext creates a FROM-scratch build context whose content
// differs per marker, so two builds necessarily produce different image IDs.
func writeScratchContext(t *testing.T, marker string) string {
	dir := t.TempDir()
	assert.Nil(t, ioutil.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM scratch\nCOPY dummy.txt /dummy.txt\n"), 0644))
	assert.Nil(t, ioutil.WriteFile(filepath.Join(dir, "dummy.txt"),
		[]byte(marker), 0644))
	return filepath.Join(dir, "Dockerfile")
}

// Every build must claim its own temporary tag and resolve to an immutable
// image ID; concurrent builds therefore never share a mutable tag while
// building.
func TestBuildImageUniqueReturnsIndependentArtifact(t *testing.T) {
	image := "captain_test_identity_unique"
	appA := App{Build: writeScratchContext(t, "marker-A"), Image: image}
	appB := App{Build: writeScratchContext(t, "marker-B"), Image: image}

	recA, errA := buildImageUnique(appA, false)
	assert.Nil(t, errA)
	defer removeImage(appA.Image + ":" + recA.BuildTag)

	recB, errB := buildImageUnique(appB, false)
	assert.Nil(t, errB)
	defer removeImage(appB.Image + ":" + recB.BuildTag)

	assert.True(t, strings.HasPrefix(recA.ID, "sha256:"), "image ID must be an immutable sha256 identity")
	assert.NotEmpty(t, recA.BuildTag)
	assert.NotEqual(t, recA.BuildTag, recB.BuildTag, "each build gets its own temporary tag")
	assert.NotEqual(t, recA.ID, recB.ID, "different content must produce different immutable IDs")

	resolvedA, err := inspectImageID(appA.Image + ":" + recA.BuildTag)
	assert.Nil(t, err)
	assert.Equal(t, recA.ID, resolvedA, "the temporary tag must resolve to the recorded immutable ID")

	// Building twice must not have moved either artifact: both temporary
	// tags still resolve to their own build.
	resolvedB, err := inspectImageID(appB.Image + ":" + recB.BuildTag)
	assert.Nil(t, err)
	assert.Equal(t, recB.ID, resolvedB)
}

// Temporary build tags must be docker-valid and unique per invocation.
func TestNewBuildTagFormat(t *testing.T) {
	tag := newBuildTag()
	assert.True(t, strings.HasPrefix(tag, "captain-b"), "tag %q should carry the captain prefix", tag)
	assert.NotEqual(t, tag, newBuildTag(), "build tags must not repeat")
	for _, r := range tag {
		assert.True(t,
			(r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.',
			"build tag %q contains invalid docker tag character", tag)
	}
}

// The build ledger is the in-command hand-off mechanism between Build and
// later Test/Push steps.
func TestBuildLedgerRoundTrip(t *testing.T) {
	resetLedger()
	defer resetLedger()

	image := "captain_test_identity_ledger"
	_, ok := getBuildRecord(image)
	assert.False(t, ok, "unknown image should have no record")

	rec := buildRecord{Image: image, ID: "sha256:abc", BuildTag: "captain-b1-deadbeef", Rev: "rev1"}
	recordBuild(rec)

	got, ok := getBuildRecord(image)
	assert.True(t, ok)
	assert.Equal(t, rec, got)
}

// verifyTagPointsTo must accept a tag pointing at the recorded build and
// reject - with *ExternalRetagError carrying both identities - a tag moved
// by a non-cooperating actor.
func TestVerifyTagPointsToDetectsExternalRetag(t *testing.T) {
	image := "captain_test_identity_retag"
	app := App{Build: writeScratchContext(t, "own-build"), Image: image}

	own, err := buildImageUnique(app, false)
	assert.Nil(t, err)
	defer removeImage(app.Image + ":" + own.BuildTag)

	// The foreign artifact is another build of the SAME repository name,
	// mirroring a concurrent workspace that an external actor retargets.
	foreignApp := App{Build: writeScratchContext(t, "foreign-build"), Image: image}
	foreign, err := buildImageUnique(foreignApp, false)
	assert.Nil(t, err)
	defer removeImage(foreignApp.Image + ":" + foreign.BuildTag)

	// Shared tag initially points at this command's own artifact.
	assert.Nil(t, tagImage(app, own.BuildTag, "latest"))
	assert.Nil(t, verifyTagPointsTo(app, "latest", own.ID),
		"verification passes while the shared tag resolves to the own image ID")

	// Simulate an external actor (manual "docker tag", another tool)
	// retargeting the shared tag at the foreign artifact.
	assert.Nil(t, tagImage(app, foreign.BuildTag, "latest"))

	verifyErr := verifyTagPointsTo(app, "latest", own.ID)
	assert.NotNil(t, verifyErr, "drift must be detected rather than silently accepted")

	retagErr, ok := verifyErr.(*ExternalRetagError)
	assert.True(t, ok, "drift must be reported as *ExternalRetagError, got %T", verifyErr)
	assert.Equal(t, image, retagErr.Image)
	assert.Equal(t, "latest", retagErr.Tag)
	assert.Equal(t, own.ID, retagErr.ExpectedID)
	assert.Equal(t, foreign.ID, retagErr.ActualID)
	assert.Contains(t, retagErr.Error(), "external retag detected")
}

// resolveRecord prefers the in-command ledger and otherwise adopts the
// current image:latest identity, preserving library callers that invoke
// Test/Push without a preceding Build in the same process.
func TestResolveRecordAdoptsLatestWhenNoLedgerEntry(t *testing.T) {
	resetLedger()
	defer resetLedger()

	image := "captain_test_identity_resolve"
	app := App{Build: writeScratchContext(t, "resolve"), Image: image}
	rec, err := buildImageUnique(app, false)
	assert.Nil(t, err)
	defer removeImage(app.Image + ":" + rec.BuildTag)
	assert.Nil(t, tagImage(app, rec.BuildTag, "latest"))
	defer removeImage(app.Image + ":latest")

	adopted := resolveRecord(app)
	assert.Equal(t, rec.ID, adopted.ID, "without a ledger entry, current latest identity is adopted")

	// A ledger entry always wins over the mutable latest tag.
	other := buildRecord{Image: image, ID: "sha256:ledger-wins"}
	recordBuild(other)
	assert.Equal(t, other.ID, resolveRecord(app).ID)
}

// When no image exists at all, resolveRecord degrades gracefully instead of
// failing, keeping legacy tag-less test invocations working.
func TestResolveRecordWithoutAnyImage(t *testing.T) {
	resetLedger()
	defer resetLedger()

	rec := resolveRecord(App{Image: "captain_test_identity_missing"})
	assert.Empty(t, rec.ID, "missing image must yield an empty identity, not an error")
	assert.Equal(t, "captain_test_identity_missing", rec.Image)
}
