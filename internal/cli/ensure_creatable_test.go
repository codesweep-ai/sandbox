package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/engine"
	"github.com/codesweep-ai/sandbox/internal/run"
)

// creatableApp is an App with a fake host, for the three states ensureCreatable
// has to tell apart.
func creatableApp(f *run.Fake) *App {
	return &App{Image: "ghcr.io/codesweep-ai/sandbox:v1", Runner: f, errW: &bytes.Buffer{}}
}

// TestEnsureImageRefusesAnImageItCannotGet: the gate. Everything create
// builds is built FROM the image, so a host that cannot obtain one must be told
// before any of it starts — not after a pull it was never going to finish, and
// not after a minute of building a filesystem from an image it does not have.
func TestEnsureImageRefusesAnImageItCannotGet(t *testing.T) {
	said := `Error: reading image "docker://ghcr.io/x:v1": manifest unknown`
	f := run.NewFake().
		On("podman image exists", run.Result{ExitCode: 1}, &run.ExitError{ExitCode: 1}).
		On("podman manifest inspect", run.Result{ExitCode: 125, Stderr: said}, &run.ExitError{ExitCode: 125, Stderr: said})
	app := creatableApp(f)

	err := app.ensureImage(context.Background())
	if err == nil {
		t.Fatal("ensureImage with an unobtainable image = nil, want the build error")
	}
	for _, want := range []string{"could not be fetched", "manifest unknown", "cs-sandbox build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if f.Contains("podman pull") {
		t.Errorf("pulled an image the registry does not have: %s", f)
	}
}

// TestEnsureImageFetchesAnImageItCanGet: the convenience. A host the
// registry can supply is supplied, rather than being sent to `build` for it.
func TestEnsureImageFetchesAnImageItCanGet(t *testing.T) {
	f := run.NewFake().
		On("podman image exists", run.Result{ExitCode: 1}, &run.ExitError{ExitCode: 1})
	app := creatableApp(f)

	_ = app.ensureImage(context.Background())
	if !f.Contains("podman pull") {
		t.Errorf("did not fetch an image the registry has: %s", f)
	}
}

// TestEnsureCreatableAlwaysPrepares: the bug this exists to stop. A base rootfs
// is kept per image repository, so an upgrade that moves the tag leaves one that
// exists and mounts and is the PREVIOUS image. Verify passes on it. Only the
// same Prepare `build` runs notices, so create has to run it whether or not
// anything looks missing.
func TestEnsureCreatableAlwaysPrepares(t *testing.T) {
	prepared := false
	eng := &fakeEngine{prepare: func() error { prepared = true; return nil }}
	app := creatableApp(run.NewFake())

	if err := app.ensureCreatable(context.Background(), eng); err != nil {
		t.Fatalf("ensureCreatable = %v, want nil", err)
	}
	if !prepared {
		t.Error("create skipped Prepare because Verify was happy; a stale rootfs boots that way")
	}
	if eng.verifies != 1 {
		t.Errorf("Verify ran %d times, want once — after preparing, to confirm it worked", eng.verifies)
	}
}

// fakeEngine records what the create path asks of an engine.
type fakeEngine struct {
	engine.Engine
	prepare  func() error
	verifies int
}

func (f *fakeEngine) Prepare(context.Context) error {
	if f.prepare != nil {
		return f.prepare()
	}
	return nil
}

func (f *fakeEngine) Verify(context.Context) error {
	f.verifies++
	return nil
}

// TestEnsureNothingHappensOnAPreparedHost: create is the command people
// run all day, and the checks that decide all this are local and must stay the
// only cost on a host with nothing missing.
func TestEnsureNothingHappensOnAPreparedHost(t *testing.T) {
	f := run.NewFake() // every probe succeeds: image present, engine verified
	app := creatableApp(f)

	if err := app.ensureImage(context.Background()); err != nil {
		t.Fatalf("ensureImage on a prepared host = %v, want nil", err)
	}
	if err := app.ensureCreatable(context.Background(), engine.NewPodman(app.engineDeps())); err != nil {
		t.Fatalf("ensureCreatable on a prepared host = %v, want nil", err)
	}
	for _, forbidden := range []string{"podman pull", "podman manifest inspect"} {
		if f.Contains(forbidden) {
			t.Errorf("%s ran on a host that needed nothing: %s", forbidden, f)
		}
	}
}
