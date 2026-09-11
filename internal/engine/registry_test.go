package engine

import (
	"context"
	"testing"

	"github.com/codesweep-ai/sandbox/internal/run"
)

// TestCheckRegistryReadsTheManifestNotTheStatus: the subtlety this rests on.
// `podman manifest inspect` fetches the manifest and then refuses to treat a
// single-architecture image as a manifest list, so a registry that HAS the image
// answers with a failure — and only the manifest in the output says so.
func TestCheckRegistryReadsTheManifestNotTheStatus(t *testing.T) {
	const img = "ghcr.io/codesweep-ai/sandbox:v1"
	ctx := context.Background()

	if got := CheckRegistry(ctx, run.NewFake(), img); !got.Fetchable {
		t.Errorf("a manifest list = %+v, want fetchable", got)
	}

	oneArch := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	failed := `Error: parsing manifest blob "` + oneArch + `": Treating single images as manifest lists is not implemented`
	f := run.NewFake().On("podman manifest inspect", run.Result{ExitCode: 125, Stderr: failed},
		&run.ExitError{ExitCode: 125, Stderr: failed})
	if got := CheckRegistry(ctx, f, img); !got.Fetchable {
		t.Errorf("a single-architecture image = %+v, want fetchable — the manifest came back", got)
	}

	// Nothing came back: the registry does not have it, and its words say why.
	said := `Error: reading image "docker://ghcr.io/x:v1": reading manifest v1 in ghcr.io/x: manifest unknown`
	no := run.NewFake().On("podman manifest inspect", run.Result{ExitCode: 125, Stderr: said},
		&run.ExitError{ExitCode: 125, Stderr: said})
	got := CheckRegistry(ctx, no, img)
	if got.Fetchable {
		t.Errorf("a registry that does not have it = %+v, want not fetchable", got)
	}
	if got.Detail != "reading manifest v1 in ghcr.io/x: manifest unknown" {
		t.Errorf("detail = %q, want podman's reason without the reference it repeats", got.Detail)
	}
}

// TestCheckRegistryNeverAsksAboutALocalImage: localhost/ names an image that
// exists only in a local store. There is no registry to ask, and asking one
// would spend three retries on a host nobody meant to contact.
func TestCheckRegistryNeverAsksAboutALocalImage(t *testing.T) {
	f := run.NewFake()
	if got := CheckRegistry(context.Background(), f, "localhost/sandbox-slim:ci"); got.Fetchable {
		t.Errorf("a localhost image = %+v, want a no", got)
	}
	if len(f.Calls) != 0 {
		t.Errorf("asked the registry about a local-only image: %s", f)
	}
	if got := CheckRegistry(context.Background(), f, ""); got.Fetchable {
		t.Errorf("no image at all = %+v, want a no", got)
	}
}

// TestRegistryDetailCarriesTheMessage: skopeo fails with a structured log line,
// and only the sentence inside it belongs in something a person reads.
func TestRegistryDetailCarriesTheMessage(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`Error: reading image "docker://x:v1": pinging container registry ghcr.io: no such host`,
			"pinging container registry ghcr.io: no such host"},
		{"plain trouble\nand a second line", "plain trouble"},
		{"", ""},
	} {
		if got := registryDetail(tc.in); got != tc.want {
			t.Errorf("registryDetail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// The reference podman repeats back is dropped: the caller has just printed
	// it, and what is left is the part that says what went wrong.
	full := `Error: reading image "docker://ghcr.io/x:v1": reading manifest v1 in ghcr.io/x: manifest unknown`
	if got := registryDetail(full); got != "reading manifest v1 in ghcr.io/x: manifest unknown" {
		t.Errorf("registryDetail = %q, want just the reason", got)
	}
}
