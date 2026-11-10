package captain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This file exercises the real captain CLI as two independent processes
// sharing one Docker daemon, proving that concurrent workspaces always keep
// their commits bound to their own build artifacts, and that coordination
// locks are released when the processes exit.

func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// waitFor polls cond until it becomes true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", message)
}

// startLocalRegistry runs an ephemeral registry:2 on the daemon's host
// network loopback (127.0.0.0/8 is preconfigured as an insecure registry on
// Docker Desktop) so plain HTTP pushes work without touching daemon or
// certificate configuration. It returns the chosen port.
func startLocalRegistry(t *testing.T) string {
	t.Helper()
	name := "captain-it-registry-" + randomSuffix()
	rb := make([]byte, 2)
	if _, err := rand.Read(rb); err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprintf("%d", 40000+int(uint16(rb[0])<<8|uint16(rb[1]))%20000)

	run := exec.Command("docker", "run", "-d", "--name", name,
		"--network", "host",
		"-e", "REGISTRY_HTTP_ADDR=0.0.0.0:"+port,
		"registry:2")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("failed to start local registry: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	// The port lives inside the daemon VM, so readiness is observed through
	// the container logs rather than a host-side HTTP probe.
	waitFor(t, 30*time.Second, func() bool {
		out, err := exec.Command("docker", "logs", name).CombinedOutput()
		return err == nil && strings.Contains(string(out), "listening on")
	}, "local registry to start listening")

	return port
}

func buildCaptainBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "captain")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/captain")
	cmd.Dir = basedir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build captain binary: %v\n%s", err, out)
	}
	return bin
}

// makeWorkspace creates a git repository containing a busybox-based
// Dockerfile that bakes marker into the image, plus a captain.yml whose
// legacy name-based test asserts image:latest carries this workspace's own
// marker and that the immutable identity env vars are present.
func makeWorkspace(t *testing.T, dir, marker, image string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	dockerfile := "FROM busybox:latest\nCOPY marker /marker\nCMD [\"cat\", \"/marker\"]\n"
	if err := ioutil.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(dir, "marker"), []byte(marker+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	yml := fmt.Sprintf(`app:
  build: Dockerfile
  image: %s
  test:
    - 'for i in $(seq 1 150); do [ -f .hold ] || break; sleep 0.2; done'
    - 'docker run --rm %s:latest | grep -q %q'
    - 'test "$CAPTAIN_IMAGE_ID" != "" -a "$CAPTAIN_IMAGE_NAME" = %q -a "$CAPTAIN_IMAGE_REF" != ""'
`, image, image, marker, image)
	if err := ioutil.WriteFile(filepath.Join(dir, "captain.yml"), []byte(yml), 0644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "captain-it@example.com"},
		{"config", "user.name", "Captain IT"},
		{"add", "."},
		{"commit", "-m", "commit " + marker},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	revOut, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(revOut))
}

func captainEnviron(lockDir string) []string {
	return append(append([]string{}, os.Environ()...), "CAPTAIN_LOCK_DIR="+lockDir)
}

// runCaptain executes the captain CLI synchronously and returns its output.
func runCaptain(t *testing.T, bin, dir, lockDir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = captainEnviron(lockDir)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// startCaptain launches captain in the background writing to logPath.
func startCaptain(t *testing.T, bin, dir, lockDir, logPath string, args ...string) *exec.Cmd {
	t.Helper()
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = captainEnviron(lockDir)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start captain %v: %v", args, err)
	}
	return cmd
}

func processAlive(cmd *exec.Cmd) bool {
	if cmd.ProcessState != nil {
		return false
	}
	return cmd.Process.Signal(syscall.Signal(0)) == nil
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// runContainerOutput runs an image reference and returns its trimmed output.
func runContainerOutput(t *testing.T, ref string) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", ref).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run %s failed: %v\n%s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestTwoProcessesInterleaveBuildTestPush is the end-to-end concurrency
// proof: two workspaces with different commits but the same image name run
// real captain processes against one daemon and one local registry.
func TestTwoProcessesInterleaveBuildTestPush(t *testing.T) {
	port := startLocalRegistry(t)
	bin := buildCaptainBinary(t)

	root := t.TempDir()
	lockDir := filepath.Join(root, "locks")
	wsA := filepath.Join(root, "ws-a")
	wsB := filepath.Join(root, "ws-b")
	image := fmt.Sprintf("127.0.0.1:%s/captain-it-conc-%s", port, randomSuffix())

	revA := makeWorkspace(t, wsA, "marker-workspace-A", image)
	revB := makeWorkspace(t, wsB, "marker-workspace-B", image)
	t.Cleanup(func() {
		for _, tag := range []string{revA, revB, "latest", "master", "main"} {
			_ = exec.Command("docker", "rmi", image+":"+tag).Run()
		}
	})

	// Phase 1: workspace A builds first and records its immutable identity.
	out, err := runCaptain(t, bin, wsA, lockDir, "build")
	if err != nil {
		t.Fatalf("workspace A build failed: %v\n%s", err, out)
	}
	idA, err := inspectImageID(image + ":" + revA)
	if err != nil {
		t.Fatalf("revision A tag missing after build: %v", err)
	}

	// Phase 2: A starts `captain test` and parks inside its (legacy,
	// name-based) test command while holding the coordination lock.
	holdA := filepath.Join(wsA, ".hold")
	if err := ioutil.WriteFile(holdA, []byte("hold"), 0644); err != nil {
		t.Fatal(err)
	}
	logA := filepath.Join(root, "a.log")
	procA := startCaptain(t, bin, wsA, lockDir, logA, "test")
	waitFor(t, 30*time.Second, func() bool {
		return strings.Contains(readLog(t, logA), "Running test command")
	}, "workspace A to enter its parked test command while holding the lock")

	// Phase 3: B starts concurrently. It may finish its own independent
	// build (which needs no shared tag), but must block before tagging
	// shared labels: revB must not appear while A holds the lock.
	logB := filepath.Join(root, "b.log")
	procB := startCaptain(t, bin, wsB, lockDir, logB, "test")

	// Give B time to perform its lock-free unique build and reach the
	// coordination lock, where it must demonstrably stall.
	waitFor(t, 15*time.Second, func() bool {
		return strings.Contains(readLog(t, logB), "Building image")
	}, "workspace B to start its independent build")
	time.Sleep(3 * time.Second)

	if !processAlive(procB) {
		t.Fatalf("workspace B exited instead of blocking on the lock:\n%s", readLog(t, logB))
	}
	if strings.Contains(readLog(t, logB), "Running test command") {
		t.Fatalf("workspace B entered test phase while A still holds the lock:\n%s", readLog(t, logB))
	}
	if _, err := inspectImageID(image + ":" + revB); err == nil {
		t.Fatalf("revision B tag was created while workspace A held the coordination lock")
	}

	// Phase 4: release A; B then acquires the lock, tags its own artifact
	// and runs its own test against image:latest.
	if err := os.Remove(holdA); err != nil {
		t.Fatal(err)
	}
	if err := procA.Wait(); err != nil {
		t.Fatalf("workspace A test failed: %v\n%s", err, readLog(t, logA))
	}
	if err := procB.Wait(); err != nil {
		t.Fatalf("workspace B test failed: %v\n%s", err, readLog(t, logB))
	}

	// Invariant 1: each revision tag is permanently bound to its own build.
	idAAfter, err := inspectImageID(image + ":" + revA)
	if err != nil {
		t.Fatalf("revision A tag disappeared: %v", err)
	}
	if idAAfter != idA {
		t.Fatalf("revision A identity changed: was %s, now %s", idA, idAAfter)
	}
	idB, err := inspectImageID(image + ":" + revB)
	if err != nil {
		t.Fatalf("revision B tag missing after interleaved test: %v", err)
	}
	if idB == idA {
		t.Fatalf("both revisions resolve to the same image ID %s", idA)
	}
	if got := runContainerOutput(t, image+":"+revA); got != "marker-workspace-A" {
		t.Fatalf("revision A runs foreign content: %q", got)
	}
	if got := runContainerOutput(t, image+":"+revB); got != "marker-workspace-B" {
		t.Fatalf("revision B runs foreign content: %q", got)
	}

	// No per-build temporary tags survive a successful command.
	listOut, err := exec.Command("docker", "images", "--filter=reference="+image+":captain-b*", "-q").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(listOut)) != "" {
		t.Fatalf("temporary build tags were not cleaned up: %s", listOut)
	}

	// Phase 5: both processes publish concurrently; the lock serializes
	// the shared-tag push and each push is verified against the own ID.
	pushA := startCaptain(t, bin, wsA, lockDir, filepath.Join(root, "push-a.log"), "push", "-c")
	pushB := startCaptain(t, bin, wsB, lockDir, filepath.Join(root, "push-b.log"), "push", "-c")
	if err := pushA.Wait(); err != nil {
		t.Fatalf("workspace A push failed: %v\n%s", err, readLog(t, filepath.Join(root, "push-a.log")))
	}
	if err := pushB.Wait(); err != nil {
		t.Fatalf("workspace B push failed: %v\n%s", err, readLog(t, filepath.Join(root, "push-b.log")))
	}

	// Invariant 2: the registry itself stores each commit's own artifact.
	// Remove every local tag first so success below proves the bytes came
	// from the registry, not from the local image store.
	for _, tag := range []string{revA, revB, "latest", "master", "main"} {
		_ = exec.Command("docker", "rmi", image+":"+tag).Run()
	}
	if out, err := exec.Command("docker", "pull", image+":"+revA).CombinedOutput(); err != nil {
		t.Fatalf("pull revision A from registry failed: %v\n%s", err, out)
	}
	if out, err := exec.Command("docker", "pull", image+":"+revB).CombinedOutput(); err != nil {
		t.Fatalf("pull revision B from registry failed: %v\n%s", err, out)
	}
	if got := runContainerOutput(t, image+":"+revA); got != "marker-workspace-A" {
		t.Fatalf("registry revision A serves foreign content: %q", got)
	}
	if got := runContainerOutput(t, image+":"+revB); got != "marker-workspace-B" {
		t.Fatalf("registry revision B serves foreign content: %q", got)
	}

	// Invariant 3: after every captain process exited, the coordination
	// resource is free and can be acquired immediately by a new opener.
	lockPath := filepath.Join(lockDir, lockName(daemonEndpoint(), image))
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("lock file missing after run: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("coordination lock still held after processes exited: %v", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// TestExternalRetagDuringTestFailsFast defines the failure boundary: a
// non-cooperating external retag of image:latest while a test command runs
// must abort the command with ExternalRetag (13) instead of letting foreign
// content pass validation.
func TestExternalRetagDuringTestFailsFast(t *testing.T) {
	bin := buildCaptainBinary(t)
	root := t.TempDir()
	lockDir := filepath.Join(root, "locks")
	ws := filepath.Join(root, "ws-retag")

	image := "captain-it-retag-" + randomSuffix()
	foreign := "captain-it-retag-foreign-" + randomSuffix()
	t.Cleanup(func() {
		_ = exec.Command("docker", "rmi", image+":latest").Run()
		_ = exec.Command("docker", "rmi", foreign+":latest").Run()
	})

	// Build a foreign image that an external actor will force onto :latest.
	foreignDir := filepath.Join(root, "foreign-ctx")
	if err := os.MkdirAll(foreignDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(foreignDir, "Dockerfile"),
		[]byte("FROM busybox:latest\nRUN echo foreign > /marker\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "build", "-t", foreign+":latest", foreignDir).CombinedOutput(); err != nil {
		t.Fatalf("foreign image build failed: %v\n%s", err, out)
	}

	// Non-git workspace: no revision tags, latest is the shared label.
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(filepath.Join(ws, "Dockerfile"),
		[]byte("FROM busybox:latest\nRUN echo own > /marker\n"), 0644); err != nil {
		t.Fatal(err)
	}
	yml := fmt.Sprintf(`app:
  build: Dockerfile
  image: %s
  test:
    - 'docker tag %s:latest %s:latest'
`, image, foreign, image)
	if err := ioutil.WriteFile(filepath.Join(ws, "captain.yml"), []byte(yml), 0644); err != nil {
		t.Fatal(err)
	}

	if out, err := runCaptain(t, bin, ws, lockDir, "build"); err != nil {
		t.Fatalf("initial build failed: %v\n%s", err, out)
	}

	out, err := runCaptain(t, bin, ws, lockDir, "test")
	if err == nil {
		t.Fatalf("external retag was not detected; test output:\n%s", out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected process exit error, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != ExternalRetag {
		t.Fatalf("expected exit code %d (ExternalRetag), got %d; output:\n%s",
			ExternalRetag, exitErr.ExitCode(), out)
	}
	if !strings.Contains(out, "external retag detected") {
		t.Fatalf("failure message must explain the external retag boundary, got:\n%s", out)
	}
}
