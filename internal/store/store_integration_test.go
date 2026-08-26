//go:build integration

package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// image is the sandbox image the live tests run against. There is no default
// any more: the name carries the version of the cs-sandbox that built it, and a
// test binary carries no version to derive it from. `make test-integration` and
// `make test-smoke` both set the variable from the binary; a bare `go test`
// against these tags has to say which image it means.
func image(t *testing.T) string {
	t.Helper()
	v := os.Getenv("CS_SANDBOX_IMAGE")
	if v == "" {
		t.Skip("set CS_SANDBOX_IMAGE to the image to run against, or use `make test-integration`")
	}
	return v
}

func requirePodman(t *testing.T, requiredImage string) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r := &run.Exec{}
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "info"); err != nil {
		t.Skipf("podman unavailable: %v", err)
	}
	if _, err := r.Run(ctx, run.Opts{ReadOnly: true}, "podman", "image", "exists", requiredImage); err != nil {
		t.Skipf("image %s not built (run: cs-sandbox build): %v", requiredImage, err)
	}
}

// TestStoreLifecycleLive: create -> seed (nested pull) -> list -> remove, on real
// podman. Proves the shared-image-store path (--image-store) end to end.
func TestStoreLifecycleLive(t *testing.T) {
	requirePodman(t, image(t))
	ctx := context.Background()
	m := Manager{Runner: &run.Exec{}, Image: image(t)}
	name := fmt.Sprintf("csgostoretest%d", os.Getpid())
	t.Cleanup(func() { _ = m.Remove(context.Background(), name, true) })

	if err := m.Create(ctx, name); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !m.Exists(ctx, name) {
		t.Fatal("store should exist after Create")
	}
	if err := m.Seed(ctx, name, []string{"docker.io/library/busybox"}, false); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	imgs, err := m.Images(ctx, name)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if !strings.Contains(imgs, "busybox") {
		t.Fatalf("seeded store should contain busybox:\n%s", imgs)
	}
	found := false
	for _, n := range m.List(ctx) {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Errorf("List should include %q", name)
	}
	if err := m.Remove(ctx, name, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if m.Exists(ctx, name) {
		t.Error("store should be gone after Remove")
	}
}

// TestStoreSeedFromHostLive: seed-store --from-host copies an image already in
// the host store into the shared store (no registry pull), and rejects an image
// that isn't in the host store.
func TestStoreSeedFromHostLive(t *testing.T) {
	requirePodman(t, image(t))
	ctx := context.Background()
	r := &run.Exec{}
	m := Manager{Runner: r, Image: image(t)}
	name := fmt.Sprintf("csgostorefh%d", os.Getpid())
	t.Cleanup(func() { _ = m.Remove(context.Background(), name, true) })

	// Ensure busybox is present in the host store so --from-host can copy it.
	if _, err := r.Run(ctx, run.Opts{}, "podman", "pull", "docker.io/library/busybox"); err != nil {
		t.Fatalf("host pull busybox: %v", err)
	}

	if err := m.Create(ctx, name); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Seed(ctx, name, []string{"docker.io/library/busybox"}, true); err != nil {
		t.Fatalf("Seed(fromHost): %v", err)
	}
	imgs, err := m.Images(ctx, name)
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if !strings.Contains(imgs, "busybox") {
		t.Fatalf("from-host seeded store should contain busybox:\n%s", imgs)
	}

	// An image not in the host store is rejected before any copy.
	err = m.Seed(ctx, name, []string{"docker.io/library/nonexistent-cs-sandbox-test"}, true)
	if err == nil || !strings.Contains(err.Error(), "not in the host store") {
		t.Errorf("Seed(fromHost) of a missing image = %v, want a 'not in the host store' error", err)
	}
}
