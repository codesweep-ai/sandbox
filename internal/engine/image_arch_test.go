package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestCheckImageArchReportsAnImageForAnotherPlatform: the case podman lets
// through. It keeps a single-architecture image on any host with only a
// warning, so the store can hold an amd64 image on an arm64 engine, and a
// sandbox started from it runs emulated.
func TestCheckImageArchReportsAnImageForAnotherPlatform(t *testing.T) {
	const img = "ghcr.io/codesweep-ai/sandbox:v1"
	f := run.NewFake().
		OnStdout("podman image inspect", "linux/amd64\n").
		OnStdout("podman version", "linux/arm64\n")

	mm := CheckImageArch(context.Background(), f, img)
	if mm == nil {
		t.Fatalf("an amd64 image on an arm64 engine = nil, want a mismatch; calls=%s", f)
	}
	if mm.Image != img || mm.Have != "linux/amd64" || mm.Want != "linux/arm64" {
		t.Errorf("mismatch = %+v, want the image, linux/amd64 and linux/arm64", *mm)
	}
	// The engine is asked, never this binary: on macOS the podman machine runs
	// the container, and a cs-sandbox built for the other architecture would
	// otherwise compare against the wrong platform.
	if !f.Contains("{{.Server.OsArch}}") {
		t.Errorf("did not ask podman's server for its platform: %s", f)
	}
}

// TestCheckImageArchAcceptsWhatItCannotRead: a false no refuses a sandbox that
// would have worked, so a matching pair, an unreadable image and an engine that
// does not answer all pass.
func TestCheckImageArchAcceptsWhatItCannotRead(t *testing.T) {
	for _, c := range []struct {
		name, image, engine string
	}{
		{"same platform", "linux/arm64", "linux/arm64"},
		{"image unreadable", "", "linux/arm64"},
		{"engine silent", "linux/amd64", ""},
		{"no os recorded", "/amd64", "linux/arm64"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := run.NewFake().
				OnStdout("podman image inspect", c.image).
				OnStdout("podman version", c.engine)
			if mm := CheckImageArch(context.Background(), f, "ghcr.io/codesweep-ai/sandbox:v1"); mm != nil {
				t.Errorf("CheckImageArch = %+v, want nil", *mm)
			}
		})
	}
}

// TestCheckImageArchLeavesALocalImageAlone: a localhost/ image was built or
// loaded on this host by somebody who chose it, and running one under emulation
// on purpose is theirs to do. It is not even asked about.
func TestCheckImageArchLeavesALocalImageAlone(t *testing.T) {
	f := run.NewFake().
		OnStdout("podman image inspect", "linux/amd64").
		OnStdout("podman version", "linux/arm64")
	if mm := CheckImageArch(context.Background(), f, "localhost/sandbox-slim:ci"); mm != nil {
		t.Errorf("a localhost image = %+v, want nil", *mm)
	}
	if len(f.Calls) != 0 {
		t.Errorf("asked podman about a local image: %s", f)
	}
}

// TestVerifyImageRefusesAnImageForAnotherPlatform: every caller that asks
// whether the image is here gets the architecture with the answer, in a
// sentence naming the platform to build and the command that builds it. The
// mismatch stays reachable underneath, which is how create tells it from a
// missing image.
func TestVerifyImageRefusesAnImageForAnotherPlatform(t *testing.T) {
	f := run.NewFake().
		OnStdout("podman image inspect", "linux/amd64").
		OnStdout("podman version", "linux/arm64")

	err := VerifyImage(context.Background(), f, "ghcr.io/codesweep-ai/sandbox:v1")
	if err == nil {
		t.Fatal("VerifyImage with an amd64 image on an arm64 engine = nil, want an error")
	}
	for _, want := range []string{"is linux/amd64", "runs linux/arm64", "cs-sandbox build", "the linux/arm64 image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if _, ok := errors.AsType[*ArchMismatch](err); !ok {
		t.Errorf("error = %v, want an ArchMismatch underneath", err)
	}
}
