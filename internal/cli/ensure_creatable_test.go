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

// swapsOnPull is a host whose store changes when an image is pulled, which a
// Fake's fixed answers cannot express: before the pull it answers from one
// Fake, and from the pull on from the other.
type swapsOnPull struct {
	before, after *run.Fake
	pulled        bool
}

func (s *swapsOnPull) Run(ctx context.Context, opts run.Opts, argv ...string) (run.Result, error) {
	if s.pulled {
		return s.after.Run(ctx, opts, argv...)
	}
	if len(argv) >= 2 && argv[0] == "podman" && argv[1] == "pull" {
		s.pulled = true
	}
	return s.before.Run(ctx, opts, argv...)
}

// onArm64 is a host whose engine runs linux/arm64 and whose store holds the
// image for the given platform.
func onArm64(image string) *run.Fake {
	return run.NewFake().
		OnStdout("{{.Os}}/{{.Architecture}}", image).
		OnStdout("{{.Server.OsArch}}", "linux/arm64")
}

// TestEnsureImageReplacesAnImageForAnotherPlatform: an amd64 image on an arm64
// engine is not the image this host can run. A registry that publishes both
// hands this host its own, so create fetches it rather than starting a sandbox
// that runs emulated.
func TestEnsureImageReplacesAnImageForAnotherPlatform(t *testing.T) {
	host := &swapsOnPull{before: onArm64("linux/amd64"), after: onArm64("linux/arm64")}
	app := &App{Image: "ghcr.io/codesweep-ai/sandbox:v1", Runner: host, errW: &bytes.Buffer{}}

	if err := app.ensureImage(context.Background()); err != nil {
		t.Fatalf("ensureImage = %v, want nil once the pull brought this platform's image", err)
	}
	if !host.pulled {
		t.Errorf("kept an image for another platform instead of fetching this one: %s", host.before)
	}
}

// TestEnsureImageRefusesAPullForAnotherPlatform: podman keeps a
// single-architecture image for any platform, so a pull can succeed and still
// bring the wrong one. What arrived is checked, and create names `build`, which
// makes this platform's image from tiers published for both.
func TestEnsureImageRefusesAPullForAnotherPlatform(t *testing.T) {
	f := onArm64("linux/amd64").
		On("podman image exists", run.Result{ExitCode: 1}, &run.ExitError{ExitCode: 1})
	app := creatableApp(f)

	err := app.ensureImage(context.Background())
	if err == nil {
		t.Fatal("ensureImage after pulling an amd64 image onto an arm64 engine = nil, want an error")
	}
	for _, want := range []string{"is linux/amd64", "runs linux/arm64", "has no linux/arm64 image", "cs-sandbox build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

// TestEnsureImageSaysAnUnfetchableImageIsTheWrongPlatform: with an image here
// and nothing in the registry, "not on this host" would be untrue. The error
// says what is here and what to run about it, and nothing is pulled.
func TestEnsureImageSaysAnUnfetchableImageIsTheWrongPlatform(t *testing.T) {
	said := `Error: reading image "docker://ghcr.io/x:v1": manifest unknown`
	f := onArm64("linux/amd64").
		On("podman manifest inspect", run.Result{ExitCode: 125, Stderr: said}, &run.ExitError{ExitCode: 125, Stderr: said})
	app := creatableApp(f)

	err := app.ensureImage(context.Background())
	if err == nil {
		t.Fatal("ensureImage = nil, want an error")
	}
	if strings.Contains(err.Error(), "not on this host") || !strings.Contains(err.Error(), "is linux/amd64") {
		t.Errorf("error = %v, want it to name the platform of the image that is here", err)
	}
	if f.Contains("podman pull") {
		t.Errorf("pulled an image the registry does not have: %s", f)
	}
}

// localBuildApp is creatableApp on a host that also holds, or lacks, this
// machine's own build of the version under its localhost/ name.
func localBuildApp(f *run.Fake) *App {
	app := creatableApp(f)
	app.LocalImage = "localhost/codesweep-ai/sandbox:v1"
	return app
}

// TestEnsureImageFallsBackToTheLocalBuild: no registry serves an unpushed
// version, and the build this machine made of it is the image create boots
// (R166). It is asked for only after the registry, so nothing is pulled.
func TestEnsureImageFallsBackToTheLocalBuild(t *testing.T) {
	said := `Error: reading image "docker://ghcr.io/x:v1": manifest unknown`
	f := run.NewFake().
		On("podman image exists ghcr.io/", run.Result{ExitCode: 1}, &run.ExitError{ExitCode: 1}).
		On("podman manifest inspect", run.Result{ExitCode: 125, Stderr: said}, &run.ExitError{ExitCode: 125, Stderr: said})
	app := localBuildApp(f)

	if err := app.ensureImage(context.Background()); err != nil {
		t.Fatalf("ensureImage with a local build here = %v, want nil", err)
	}
	if app.Image != app.LocalImage {
		t.Errorf("Image = %q, want the local build %q", app.Image, app.LocalImage)
	}
	if f.Contains("podman pull") {
		t.Errorf("pulled an image the registry does not have: %s", f)
	}
}

// TestEnsureImagePrefersThePublishedImage: once CI publishes the version, a
// host that holds its own build of it moves to CI's, without being told.
func TestEnsureImagePrefersThePublishedImage(t *testing.T) {
	f := run.NewFake().
		On("podman image exists ghcr.io/", run.Result{ExitCode: 1}, &run.ExitError{ExitCode: 1})
	app := localBuildApp(f)

	if err := app.ensureImage(context.Background()); err != nil {
		t.Fatalf("ensureImage = %v, want nil", err)
	}
	if !f.Contains("podman pull ghcr.io/codesweep-ai/sandbox:v1") {
		t.Errorf("did not fetch the published image: %s", f)
	}
	if app.Image != "ghcr.io/codesweep-ai/sandbox:v1" {
		t.Errorf("Image = %q, want the published one", app.Image)
	}
}

// TestEnsureImageWithNeitherNamesBuild: with no published image and no local
// build, the error is the one it always was.
func TestEnsureImageWithNeitherNamesBuild(t *testing.T) {
	said := `Error: reading image "docker://ghcr.io/x:v1": manifest unknown`
	f := run.NewFake().
		On("podman image exists", run.Result{ExitCode: 1}, &run.ExitError{ExitCode: 1}).
		On("podman manifest inspect", run.Result{ExitCode: 125, Stderr: said}, &run.ExitError{ExitCode: 125, Stderr: said})
	app := localBuildApp(f)

	err := app.ensureImage(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not be fetched") || !strings.Contains(err.Error(), "cs-sandbox build") {
		t.Fatalf("ensureImage = %v, want the error naming build", err)
	}
	if app.Image != "ghcr.io/codesweep-ai/sandbox:v1" {
		t.Errorf("Image = %q, want it left on the published name", app.Image)
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
