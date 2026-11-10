package captain // import "github.com/harbur/captain"

import (
	"fmt"
	"gopkg.in/cheggaaa/pb.v1"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// Debug can be turned on to enable debug mode.
var Debug bool

// StatusError provides error code and id
type StatusError struct {
	error  error
	status int
}

// Pre function executes commands on pre section before build
func Pre(app App) error {
	for _, value := range app.Pre {
		info("Running pre command: %s", value)
		res := execute("bash", "-c", value)
		if res != nil {
			return res
		}
	}
	return nil
}

// Post function executes commands on pre section after build
func Post(app App) error {
	for _, value := range app.Post {
		info("Running post command: %s", value)
		res := execute("bash", "-c", value)
		if res != nil {
			return res
		}
	}
	return nil
}

type BuildOptions struct {
	Config       Config
	Tag          string
	Force        bool
	All_branches bool
	Long_sha     bool
	Branch_tags  bool
	Commit_tags  bool
}

// Build function compiles the Containers of the project.
//
// Every build is claimed under a unique per-build temporary tag and resolved
// to an immutable image ID before any shared tag (latest/rev/branch) is
// touched. Shared tag mutations happen under the daemon+repository
// coordination lock and are immediately verified against the recorded ID,
// so concurrent workspaces can never label another build's artifact as
// their own commit.
func Build(opts BuildOptions) {
	config := opts.Config

	var rev = getRevision(opts.Long_sha)

	// For each App
	for _, app := range config.GetApps() {
		// If no Git repo exist
		if !isGit() {
			// Performing [build unique tag | tag own id@latest]
			debug("No local git repository found, just building latest")

			// Execute Pre commands
			if res := Pre(app); res != nil {
				err("Pre execution returned non-zero status")
				return
			}

			rec, res := buildImageUnique(app, opts.Force)
			if res != nil {
				os.Exit(BuildFailed)
			}
			recordBuild(rec)

			lockOrExit(app)
			tagOrExit(app, rec.BuildTag, "latest", rec.ID)

			// Add additional user-defined Tag
			if opts.Tag != "" {
				tagOrExit(app, rec.BuildTag, opts.Tag, rec.ID)
			}

			releaseBuildTag(app, rec)
		} else {
			// Skip build if there are no local changes and the commit is already built
			if !isDirty() && imageExist(app, rev) && !opts.Force {
				// Performing [skip build | anchor rev identity | tag id@latest|@branch]
				info("Skipping build of %s:%s - image is already built", app.Image, rev)

				// Anchor this command to the immutable identity already
				// representing the revision instead of re-resolving "latest".
				id, res := inspectImageID(app.Image + ":" + rev)
				if res != nil {
					err("Cannot resolve immutable identity of %s:%s: %s", app.Image, rev, res)
					os.Exit(NonExistImage)
				}
				recordBuild(buildRecord{Image: app.Image, ID: id, Rev: rev})

				lockOrExit(app)

				// Tag commit image
				tagOrExit(app, rev, "latest", id)

				// Tag branch image
				for _, branch := range getBranches(opts.All_branches) {
					tagOrExit(app, rev, branch, id)
				}

				// Add additional user-defined Tag
				if opts.Tag != "" {
					tagOrExit(app, rev, opts.Tag, id)
				}
			} else {
				// Performing [build unique tag | tag own id@rev|@latest|@branch]

				// Execute Pre commands
				if res := Pre(app); res != nil {
					err("Pre execution returned non-zero status")
				}

				rec, res := buildImageUnique(app, opts.Force)
				if res != nil {
					os.Exit(BuildFailed)
				}
				rec.Rev = rev
				recordBuild(rec)

				lockOrExit(app)

				if isDirty() {
					debug("Skipping tag of %s:%s - local changes exist", app.Image, rev)
					// Keep "latest" on this build's own image so legacy test
					// scripts referencing image:latest keep working.
					tagOrExit(app, rec.BuildTag, "latest", rec.ID)
				} else {
					// Tag commit image
					tagOrExit(app, rec.BuildTag, rev, rec.ID)

					// Tag latest image
					tagOrExit(app, rec.BuildTag, "latest", rec.ID)

					// Tag branch image
					for _, branch := range getBranches(opts.All_branches) {
						tagOrExit(app, rec.BuildTag, branch, rec.ID)
					}

					// Add additional user-defined Tag
					if opts.Tag != "" {
						tagOrExit(app, rev, opts.Tag, rec.ID)
					}
				}

				releaseBuildTag(app, rec)
			}
		}

		// Execute Post commands
		if res := Post(app); res != nil {
			err("Post execution returned non-zero status")
		}
	}
}

// lockOrExit acquires the daemon+repository coordination lock protecting
// the app's shared tags, terminating with LockFailed when it cannot be taken.
func lockOrExit(app App) {
	if lockErr := coordinator.Lock(app); lockErr != nil {
		err("Failed to acquire coordination lock for %s: %s", app.Image, lockErr)
		os.Exit(LockFailed)
	}
}

// tagOrExit points tag at origin's image while holding the coordination
// lock, then verifies the tag still resolves to expectedID. Any drift
// (external retag) or tagging failure terminates the command instead of
// letting a foreign artifact masquerade as this build.
func tagOrExit(app App, origin, tag, expectedID string) {
	if tag == "" {
		return
	}
	if tagErr := tagImage(app, origin, tag); tagErr != nil {
		err("%s", tagErr)
		os.Exit(TagFailed)
	}
	if verifyErr := verifyTagPointsTo(app, tag, expectedID); verifyErr != nil {
		reportIdentityFailure(app, tag, verifyErr)
	}
}

// reportIdentityFailure maps an identity verification error onto the right
// exit code: external retag drift is explicitly distinguishable from
// ordinary daemon/tag failures.
func reportIdentityFailure(app App, tag string, cause error) {
	if _, ok := cause.(*ExternalRetagError); ok {
		err("%s", cause)
		os.Exit(ExternalRetag)
	}
	err("Cannot validate %s:%s against this build's immutable image: %s", app.Image, tag, cause)
	os.Exit(TagFailed)
}

// releaseBuildTag removes the per-build temporary tag. The image itself is
// retained through its stable tags (latest/rev/...); only the temporary
// alias is dropped, best effort.
func releaseBuildTag(app App, rec buildRecord) {
	if rec.BuildTag == "" {
		return
	}
	if err := removeImage(app.Image + ":" + rec.BuildTag); err != nil {
		debug("Could not remove temporary build tag %s:%s: %s", app.Image, rec.BuildTag, err)
	}
}

// resolveRecord returns the immutable identity recorded for this command,
// adopting the current image:latest identity when Build did not run in the
// same process (direct library use). When no image exists at all, an empty
// record is returned and identity verification is skipped, preserving
// legacy behavior for tag-less invocations.
func resolveRecord(app App) buildRecord {
	if rec, ok := getBuildRecord(app.Image); ok {
		return rec
	}
	debug("No in-command build record for %s, adopting current %s:latest identity", app.Image, app.Image)
	id, err := inspectImageID(app.Image + ":latest")
	if err != nil {
		debug("Cannot resolve %s:latest (%s); continuing without identity verification", app.Image, err)
		return buildRecord{Image: app.Image, Rev: getRevision(false)}
	}
	rec := buildRecord{Image: app.Image, ID: id, Rev: getRevision(false)}
	recordBuild(rec)
	return rec
}

// testEnv exposes the immutable identity of this command's build to test
// scripts, alongside the conventional image name references.
func testEnv(app App, rec buildRecord) []string {
	ref := app.Image + ":latest"
	if rec.Rev != "" {
		ref = app.Image + ":" + rec.Rev
	}
	return []string{
		"CAPTAIN_IMAGE_ID=" + rec.ID,
		"CAPTAIN_IMAGE_NAME=" + app.Image,
		"CAPTAIN_IMAGE_REV=" + rec.Rev,
		"CAPTAIN_IMAGE_REF=" + ref,
	}
}

// Test function executes the tests of the project.
//
// The coordination lock is held for the whole test phase so legacy test
// scripts referencing image:latest observe this command's own artifact. The
// tag is re-verified before and after every test command; an external
// retag during the run aborts with ExternalRetag instead of validating
// foreign content. Test scripts also receive the immutable image identity
// through CAPTAIN_IMAGE_* environment variables.
func Test(opts BuildOptions) {
	config := opts.Config

	for _, app := range config.GetApps() {
		rec := resolveRecord(app)
		lockOrExit(app)

		env := testEnv(app, rec)
		for _, value := range app.Test {
			if rec.ID != "" {
				if err := verifyTagPointsTo(app, "latest", rec.ID); err != nil {
					reportIdentityFailure(app, "latest", err)
				}
			}

			info("Running test command: %s", value)
			res := executeEnv(env, "bash", "-c", value)
			if res != nil {
				err("Test execution returned non-zero status")
				os.Exit(ExecuteFailed)
			}

			if rec.ID != "" {
				if err := verifyTagPointsTo(app, "latest", rec.ID); err != nil {
					reportIdentityFailure(app, "latest", err)
				}
			}
		}
	}
}

// Push function pushes the containers to the remote registry.
//
// Every tag is verified against this command's immutable image ID while
// holding the coordination lock immediately before the push, so an
// externally moved shared tag can never reach the registry under the
// current commit.
func Push(opts BuildOptions) {
	config := opts.Config

	// If no Git repo exist
	if !isGit() {
		err("No local git repository found, cannot push")
		os.Exit(NoGit)
	}

	if isDirty() {
		err("Git repository has local changes, cannot push")
		os.Exit(GitDirty)
	}

	for _, app := range config.GetApps() {
		rec := resolveRecord(app)
		lockOrExit(app)

		for _, branch := range getBranches(opts.All_branches) {
			pushVerified(app, rec, "latest")
			if opts.Branch_tags {
				pushVerified(app, rec, branch)
			}
			if opts.Commit_tags {
				rev := rec.Rev
				if rev == "" {
					rev = getRevision(opts.Long_sha)
				}
				pushVerified(app, rec, rev)
			}

			// Add additional user-defined Tag
			if opts.Tag != "" {
				pushVerified(app, rec, opts.Tag)
			}
		}
	}
}

// pushVerified confirms tag still points at this command's own image and
// only then pushes it. Drift aborts with ExternalRetag; registry failures
// keep the legacy ExecuteFailed semantics.
func pushVerified(app App, rec buildRecord, tag string) {
	info("Pushing image %s:%s", app.Image, tag)
	if rec.ID != "" {
		if err := verifyTagPointsTo(app, tag, rec.ID); err != nil {
			reportIdentityFailure(app, tag, err)
		}
	}
	if res := pushImage(app.Image, tag); res != nil {
		err("Push returned non-zero status")
		os.Exit(ExecuteFailed)
	}
}

// Pull function pulls the containers from the remote registry
func Pull(opts BuildOptions) {
	config := opts.Config

	for _, app := range config.GetApps() {
		for _, branch := range getBranches(opts.All_branches) {
			info("Pulling image %s:%s", app.Image, "latest")
			if res := pullImage(app.Image, "latest"); res != nil {
				err("Pull returned non-zero status")
				os.Exit(ExecuteFailed)
			}
			if opts.Branch_tags {
				info("Pulling image %s:%s", app.Image, branch)
				if res := pullImage(app.Image, branch); res != nil {
					err("Pull returned non-zero status")
					os.Exit(ExecuteFailed)
				}
			}
			if opts.Commit_tags {
				rev := getRevision(opts.Long_sha)
				info("Pulling image %s:%s", app.Image, rev)
				if res := pullImage(app.Image, rev); res != nil {
					err("Pull returned non-zero status")
					os.Exit(ExecuteFailed)
				}
			}

			// Add additional user-defined Tag
			if opts.Tag != "" {
				info("Pulling image %s:%s", app.Image, opts.Tag)
				if res := pullImage(app.Image, opts.Tag); res != nil {
					err("Pull returned non-zero status")
					os.Exit(ExecuteFailed)
				}
			}
		}
	}
}

// Purge function purges the stale images
func Purge(opts BuildOptions) {
	config := opts.Config

	// For each App
	for _, app := range config.GetApps() {
		// Purge mutates the same shared tag set as Build/Push; coordinate
		// with concurrent captain processes on the same daemon and image.
		lockOrExit(app)

		var tags = []string{}

		// Retrieve the list of the existing Image tags
		for _, img := range getImages(app) {
			tags = append(tags, img.RepoTags...)
		}

		// Remove from the list: The latest image
		for key, tag := range tags {
			if tag == app.Image+":latest" {
				tags = append(tags[:key], tags[key+1:]...)
			}
		}

		// Remove from the list: The current commit-id
		for key, tag := range tags {
			if tag == app.Image+":"+getRevision(opts.Long_sha) {
				tags = append(tags[:key], tags[key+1:]...)
			}
		}

		// Remove from the list: The working-dir git branches
		for _, branch := range getBranches(opts.All_branches) {
			for key, tag := range tags {
				if tag == app.Image+":"+branch {
					tags = append(tags[:key], tags[key+1:]...)
				}
			}
		}

		// Proceed with deletion of Images
		for _, tag := range tags {
			info("Deleting image %s", tag)
			res := removeImage(tag)
			if res != nil {
				err("Deleting image failed: %s", res)
				os.Exit(DeleteImageFailed)
			}
		}
	}
}

func SelfUpdate() {
	captainDir := filepath.FromSlash(os.Getenv("HOME") + "/.captain")
	binariesDir := filepath.FromSlash(captainDir + "/binaries")
	binDir := filepath.FromSlash(captainDir + "/bin")

	kernel := runtime.GOOS
	arch := runtime.GOARCH
	captainSymlinkPath := filepath.FromSlash(binDir + "/captain")
	currentVersionPath, _ := os.Readlink(captainSymlinkPath)

	info("Checking the last version of Captain...")
	version := findLastVersion()
	downloadUrl := fmt.Sprintf("https://github.com/harbur/captain/releases/download/%s/captain_%s_%s", version, kernel, arch)
	downloadedVersionPath := filepath.FromSlash(binariesDir + "/captain-" + version)

	if currentVersionPath == downloadedVersionPath {
		info("You have installed the last version of captain (%s)", version)
		return
	}

	info("New version available, start downloading %s", version)

	// create binaries dir
	if err := os.MkdirAll(binariesDir, 0755); err != nil {
		fmt.Println(err)
	}

	// download new version
	if err := downloadFile(downloadedVersionPath, downloadUrl); err != nil {
		fmt.Println(err)
	}

	// grant excution to download version
	if err := os.Chmod(downloadedVersionPath, 0755); err != nil {
		fmt.Println(err)
	}

	info("Downloaded captain %s", version)

	if err := os.MkdirAll(binDir, 0755); err != nil {
		fmt.Println(err)
	}

	if _, err := os.Stat(captainSymlinkPath); err == nil {
		os.Remove(captainSymlinkPath)
	}

	if err := os.Symlink(downloadedVersionPath, captainSymlinkPath); err != nil {
		fmt.Println(err)
	}

	info("Installed captain %s", version)
}

func findLastVersion() string {
	url := "https://raw.githubusercontent.com/harbur/captain/master/VERSION"

	res, err := http.Get(url)

	if err != nil {
		panic(err)
	}
	defer res.Body.Close()

	content, err := ioutil.ReadAll(res.Body)
	if err != nil {
		panic(err)
	}

	return string(content)
}

func downloadFile(filepath string, url string) error {

	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// create and start bar
	bar := pb.New64(resp.ContentLength).SetUnits(pb.U_BYTES).Start()
	defer bar.Finish()

	// create proxy reader
	reader := bar.NewProxyReader(resp.Body)

	// and copy from pb reader
	_, err = io.Copy(out, reader)
	if err != nil {
		return err
	}

	return nil
}
