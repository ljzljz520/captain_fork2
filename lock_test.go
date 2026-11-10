package captain

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

// lockName must scope coordination to the (docker daemon endpoint, image
// repository) pair: same pair -> same lock, different endpoint or different
// image -> different lock.
func TestLockNameScoping(t *testing.T) {
	endpoint := "unix:///var/run/docker.sock"

	same := lockName(endpoint, "team/app")
	assert.Equal(t, same, lockName(endpoint, "team/app"),
		"lock name must be stable for the same daemon and image")

	assert.NotEqual(t, same, lockName("unix:///other/docker.sock", "team/app"),
		"different daemons must use different locks")
	assert.NotEqual(t, same, lockName(endpoint, "team/other"),
		"different image repositories must use different locks")

	// Hashed names must be filesystem-safe regardless of input characters.
	name := lockName("tcp://daemon.example.com:2376", "registry.example.com/team/app")
	assert.Equal(t, filepath.Base(name), name, "lock name must not contain path separators")
}

// CAPTAIN_LOCK_DIR overrides the on-disk location, which keeps tests
// isolated from ~/.captain and lets parallel runs use separate directories.
func TestLockDirectoryOverride(t *testing.T) {
	old, had := os.LookupEnv("CAPTAIN_LOCK_DIR")
	defer func() {
		if had {
			os.Setenv("CAPTAIN_LOCK_DIR", old)
		} else {
			os.Unsetenv("CAPTAIN_LOCK_DIR")
		}
	}()

	custom := filepath.Join(t.TempDir(), "nested", "locks")
	os.Setenv("CAPTAIN_LOCK_DIR", custom)
	assert.Equal(t, custom, lockDirectory())
}

// Two coordinators (standing in for two captain processes sharing the same
// lock directory) must mutually exclude each other on one image, while
// releasing the lock makes it immediately acquirable again.
func TestCoordinatorMutualExclusion(t *testing.T) {
	os.Setenv("CAPTAIN_LOCK_DIR", t.TempDir())
	defer os.Unsetenv("CAPTAIN_LOCK_DIR")

	app := App{Image: "captain_test/locking"}
	holder := NewCoordinator()
	contender := NewCoordinator()

	assert.Nil(t, holder.Lock(app), "first coordinator should acquire the lock")

	acquired, err := contender.TryLock(app)
	assert.Nil(t, err)
	assert.False(t, acquired, "second coordinator must not take a held lock")

	assert.Nil(t, holder.Unlock(app), "releasing the lock should not error")

	acquired, err = contender.TryLock(app)
	assert.Nil(t, err)
	assert.True(t, acquired, "lock must be acquirable once released")
	assert.Nil(t, contender.Unlock(app))
}

// Re-acquiring an already held lock within the same process must be a
// no-op, and ReleaseAll must free every held lock.
func TestCoordinatorReentrantAndReleaseAll(t *testing.T) {
	os.Setenv("CAPTAIN_LOCK_DIR", t.TempDir())
	defer os.Unsetenv("CAPTAIN_LOCK_DIR")

	apps := []App{{Image: "captain_test/reentrant-a"}, {Image: "captain_test/reentrant-b"}}
	c := NewCoordinator()

	for _, app := range apps {
		assert.Nil(t, c.Lock(app))
		assert.Nil(t, c.Lock(app), "re-entrant Lock on the same process must succeed")
	}

	other := NewCoordinator()
	for _, app := range apps {
		acquired, err := other.TryLock(app)
		assert.Nil(t, err)
		assert.False(t, acquired, "%s should still be held before ReleaseAll", app.Image)
	}

	c.ReleaseAll()

	for _, app := range apps {
		acquired, err := other.TryLock(app)
		assert.Nil(t, err)
		assert.True(t, acquired, "%s must be free after ReleaseAll", app.Image)
		assert.Nil(t, other.Unlock(app))
	}
}

// Unlock without a prior Lock is a safe no-op.
func TestCoordinatorUnlockWithoutLock(t *testing.T) {
	os.Setenv("CAPTAIN_LOCK_DIR", t.TempDir())
	defer os.Unsetenv("CAPTAIN_LOCK_DIR")

	c := NewCoordinator()
	assert.Nil(t, c.Unlock(App{Image: "captain_test/never-locked"}))
}

// The raw flock on the lock file must be releasable by closing the
// coordinator: this mirrors what happens when a captain process exits and
// the kernel drops its flock, proving no coordination resource survives.
func TestLockFileIsReusableAfterRelease(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("CAPTAIN_LOCK_DIR", dir)
	defer os.Unsetenv("CAPTAIN_LOCK_DIR")

	app := App{Image: "captain_test/release"}
	c := NewCoordinator()
	assert.Nil(t, c.Lock(app))

	path := filepath.Join(dir, lockName(daemonEndpoint(), app.Image))
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	assert.Nil(t, err)
	defer f.Close()

	assert.Equal(t, syscall.EWOULDBLOCK, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB),
		"independent open file description must see the lock as held")

	assert.Nil(t, c.Unlock(app))

	assert.Nil(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB),
		"after release the same lock file must be flockable again")
	assert.Nil(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
}
