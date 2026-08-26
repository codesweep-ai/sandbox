module github.com/codesweep-ai/sandbox

go 1.27.0

require (
	github.com/spf13/cobra v1.10.2
	golang.org/x/mod v0.40.0
	golang.org/x/sys v0.47.0
)

require (
	github.com/codesweep-ai/ledger v0.0.0-20260826154712-f3d4cf8989eb // indirect
	github.com/codesweep-ai/lint v0.0.0-20260826152054-3acef36b8e16 // indirect
	github.com/codesweep-ai/tracer v0.0.0-20260826154852-c266382e4233 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

tool (
	github.com/codesweep-ai/ledger/cmd/cs-ledger
	github.com/codesweep-ai/lint/cmd/cs-lint
	github.com/codesweep-ai/tracer/cmd/cs-tracer
)
