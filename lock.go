package captain // import "github.com/harbur/captain"

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const defaultDockerEndpoint = "unix:///var/run/docker.sock"

// Coordinator serializes captain processes that talk to the same Docker
// daemon and build images sharing the same repository name.
//
// Coordination is implemented with advisory flock(2) locks on regular files
// under the lock directory. The lock scope is (docker daemon endpoint,
// image repository): two workspaces building the same image against one
// daemon contend, while different images or different daemons never block
// each other. The kernel releases flock locks automatically when the holding
// process exits - even after a crash - so no stale coordination state can
// survive a command.
type Coordinator struct {
	dir string

	mu   sync.Mutex
	held map[string]*os.File
}

// coordinator is the package-level instance used by Build/Test/Push/Purge.
var coordinator = NewCoordinator()

// NewCoordinator creates a Coordinator backed by the configured lock
// directory (CAPTAIN_LOCK_DIR or ~/.captain/locks).
func NewCoordinator() *Coordinator {
	return &Coordinator{
		dir:  lockDirectory(),
		held: map[string]*os.File{},
	}
}

// daemonEndpoint returns the effective Docker daemon endpoint, matching the
// resolution performed by go-dockerclient's NewClientFromEnv.
func daemonEndpoint() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}
	return defaultDockerEndpoint
}

// lockDirectory returns the directory holding the coordination lock files.
func lockDirectory() string {
	if d := os.Getenv("CAPTAIN_LOCK_DIR"); d != "" {
		return d
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".captain", "locks")
	}
	return filepath.Join(os.TempDir(), "captain", "locks")
}

// lockName derives a stable, filesystem-safe lock file name from the
// (daemon endpoint, image repository) pair. Hashing keeps image names with
// slashes or registries (e.g. registry.example.com/team/app) usable as
// file names while preserving the scope identity.
func lockName(endpoint, image string) string {
	sum := sha256.Sum256([]byte(endpoint + "\x00" + image))
	return "captain-" + hex.EncodeToString(sum[:16]) + ".lock"
}

func (c *Coordinator) lockPath(app App) string {
	return filepath.Join(c.dir, lockName(daemonEndpoint(), app.Image))
}

// Lock acquires the exclusive coordination lock for app, blocking until it
// becomes available. Re-acquiring a lock already held by this process is a
// no-op, so Build/Test/Push can request it independently within one command.
func (c *Coordinator) Lock(app App) error {
	return c.flock(app, false)
}

// TryLock acquires the coordination lock for app only if it is currently
// free. It returns false when another process holds the lock.
func (c *Coordinator) TryLock(app App) (bool, error) {
	if err := c.flock(app, true); err != nil {
		if err == syscall.EWOULDBLOCK {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *Coordinator) flock(app App, nonBlocking bool) error {
	key := c.lockPath(app)

	c.mu.Lock()
	if _, alreadyHeld := c.held[key]; alreadyHeld {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	if err := os.MkdirAll(c.dir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(key, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}

	flags := syscall.LOCK_EX
	if nonBlocking {
		flags |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), flags); err != nil {
		f.Close()
		return err
	}

	c.mu.Lock()
	// Another code path in this process could theoretically have won the
	// race; flock is per open-file-description, so keep the first handle
	// and drop the duplicate one.
	if _, ok := c.held[key]; ok {
		c.mu.Unlock()
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil
	}
	c.held[key] = f
	c.mu.Unlock()
	return nil
}

// Unlock releases the coordination lock for app. It is a no-op when the
// lock is not held by this process.
func (c *Coordinator) Unlock(app App) error {
	key := c.lockPath(app)

	c.mu.Lock()
	f, ok := c.held[key]
	if ok {
		delete(c.held, key)
	}
	c.mu.Unlock()

	if !ok {
		return nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReleaseAll releases every coordination lock held by this process. It is
// meant to be deferred by CLI command entry points so handles are closed
// deterministically; the kernel would release them anyway at exit.
func (c *Coordinator) ReleaseAll() {
	c.mu.Lock()
	files := make([]*os.File, 0, len(c.held))
	for _, f := range c.held {
		files = append(files, f)
	}
	c.held = map[string]*os.File{}
	c.mu.Unlock()

	for _, f := range files {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}

// Lock acquires the shared-tag coordination lock for app on the default
// coordinator.
func Lock(app App) error {
	return coordinator.Lock(app)
}

// ReleaseLocks releases all coordination locks held via the default
// coordinator.
func ReleaseLocks() {
	coordinator.ReleaseAll()
}
