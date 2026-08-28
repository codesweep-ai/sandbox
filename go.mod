module github.com/codesweep-ai/sandbox

go 1.27.0

require (
	github.com/spf13/cobra v1.10.2
	golang.org/x/mod v0.40.0
	golang.org/x/sys v0.47.0
)

require (
	github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/codesweep-ai/ledger v0.0.0-20260828041611-7ebdac7cf8e5 // indirect
	github.com/codesweep-ai/lint v0.0.0-20260827203949-760d8d08d2a1 // indirect
	github.com/codesweep-ai/tracer v0.0.0-20260828043736-fa4d6eedf7f6 // indirect
	github.com/codesweep-ai/vcr v0.0.0-20260828045312-de9143b6a09a // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.21 // indirect
	github.com/mattn/go-shellwords v1.0.12 // indirect
	github.com/rhysd/actionlint v1.7.12 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.3 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/telemetry v0.0.0-20260811182544-a038080d80e5 // indirect
	golang.org/x/tools v0.49.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

tool (
	github.com/codesweep-ai/ledger/cmd/cs-ledger
	github.com/codesweep-ai/lint/cmd/cs-lint
	github.com/codesweep-ai/tracer/cmd/cs-tracer
	github.com/codesweep-ai/vcr/cmd/cs-vcr
	github.com/rhysd/actionlint/cmd/actionlint
	golang.org/x/tools/cmd/deadcode
)
