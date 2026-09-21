module github.com/codesweep-ai/sandbox

go 1.27.0

require (
	github.com/spf13/cobra v1.10.2
	golang.org/x/mod v0.41.0
	golang.org/x/sys v0.48.0
)

require (
	github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/codesweep-ai/ledger v0.0.0-20260921044937-63578cb472f8 // indirect
	github.com/codesweep-ai/lint v0.0.0-20260921044852-bfacc99eb226 // indirect
	github.com/codesweep-ai/npmrevs v0.0.0-20260921040735-e92f1c0bd295 // indirect
	github.com/codesweep-ai/tracer v0.0.0-20260921045317-699bfbdabc70 // indirect
	github.com/codesweep-ai/vcr v0.0.0-20260921050009-be024ba6f866 // indirect
	github.com/docker/cli v29.7.2+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/google/go-containerregistry v0.22.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.21 // indirect
	github.com/mattn/go-shellwords v1.0.12 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/rhysd/actionlint v1.7.12 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.3 // indirect
	golang.org/x/crypto/x509roots/fallback v0.0.0-20260916154351-b853e4b620cd // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/telemetry v0.0.0-20260811182544-a038080d80e5 // indirect
	golang.org/x/tools v0.49.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

tool (
	github.com/codesweep-ai/ledger/cmd/cs-ledger
	github.com/codesweep-ai/lint/cmd/cs-lint
	github.com/codesweep-ai/npmrevs/cmd/cs-npmrevs
	github.com/codesweep-ai/tracer/cmd/cs-tracer
	github.com/codesweep-ai/vcr/cmd/cs-vcr
	github.com/rhysd/actionlint/cmd/actionlint
	golang.org/x/tools/cmd/deadcode
)
