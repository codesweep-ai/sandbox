# Project-specific knobs for scripts/lint-walkthrough.py.
#
# The linter beside this file carries no project knowledge and is meant to stay
# byte-identical everywhere it lands. This file is the half you edit.

TOOL = "cs-sandbox"
TOOL_PATH = "bin/cs-sandbox"

DOCS = ["README.md", "INSTALL.md", "MANUAL.md", "SPEC.md", "CONTRIBUTING.md"]
EXTRA_DOCS = []

ENV_PREFIX = "CS_SANDBOX_"

ENV_INTERNAL = {
    "CS_SANDBOX_COVERAGE_LOG": "test instrumentation: the coverage emitter"
                               " writes a record only when the suite sets it",
    "CS_SANDBOX_IT_HOSTROUTE": "test instrumentation: opts the live host-route"
                               " test in",
    "CS_SANDBOX_SSH_BIND": "SBX-005 holds it open. Documenting the override"
                           " before validating it would publish a way around"
                           " R142 rather than close it",
}

# The guest's own scripts read the settings the seed hands them. They are an
# internal protocol between the CLI and the image, not something a user sets.
SOURCE_SKIP = {
    "image/": "the sandbox image's own scripts, read inside a guest",
}

# No document shows a captured sample, so there is nothing to reproduce yet.
# Adding one means adding the verb here.
SAFE_VERBS = []
SAMPLE_SKIP = {}

# The walkthroughs say plainly that these paths are the reader's to supply.
PLACEHOLDER_OK = ["~/projects/"]

PREREQ_OK = []

AGENT_SECTION = "Notes for agents"

ALLOW = {}
