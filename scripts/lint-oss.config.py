# Project-specific knobs for scripts/lint-oss.py.
#
# The linter beside this file is vendored and stays byte-identical across
# projects. Everything that differs between them lives here, so a fix to a check
# can be copied out again without carrying one project's exceptions into the
# next. Tune until every reported problem is a real one: a check that cries wolf
# is worse than no check.
#
# Run `python3 scripts/lint-oss.py --explain` to read what each rule wants.

# The command this repository ships, as a reader types it: "cs-sandbox".
# Left empty, the linter infers it from the module path or the Makefile, and
# says so. Set it when the guess is wrong.
PROJECT = "cs-sandbox"

# The owner/name this repository is published as, used to check that the README
# badges point at the real repository. Empty means "take it from the origin
# remote".
GITHUB_REPO = "codesweep-ai/sandbox"

# True once the repository is public. Published history cannot be rewritten, so
# the history rules (OSS-7xx) report as warnings rather than errors from that
# point on. Flip this in the same commit that makes the repository public.
PUBLISHED = True

# The documents every reader-facing repository in this family carries. Drop
# INSTALL.md only when the repository installs nothing — a prompt library or a
# template collection — and say so in ALLOW below.
DOC_SET = [
    "README.md",
    "INSTALL.md",
    "MANUAL.md",
    "SPEC.md",
    "CONTRIBUTING.md",
    "AGENTS.md",
]

# Documents this project adds to the set, which AGENTS.md must also route to.
# ledger's GUIDE.md is the example: a genuine standalone how-to that folding
# would bury.
EXTRA_DOCS = []

# Home-directory names that are a placeholder or a shipped account rather than a
# person. The linter already knows the four in wide use — user, you, name and
# runner. Add the account an image of your own ships under, and nothing else:
# the check exists to catch a real login, and every name added here is one it
# stops catching. Never add a name that is somebody's.
# dev is the account inside every sandbox image; seeder is the helper account
# the store builder runs as. tester is the placeholder a paths test sets HOME to.
HOME_ALLOW = {"user", "you", "name", "runner", "dev", "seeder", "tester"}

# Mail domains that are documentation addresses or machine identities rather
# than a person's. example.com and example.org are reserved for documentation;
# noreply addresses identify a bot.
# Microsoft.com appears inside a verbatim kernel banner — "Linux version
# 4.4.0-19041-Microsoft (Microsoft@Microsoft.com)" — which the WSL detection
# test matches character for character. It is a string Microsoft ships, not an
# address anybody reads.
EMAIL_ALLOW = {"Microsoft.com"}

# Tracked paths the text scans skip, each with the reason it is safe. A path
# with no reason is a waiver nobody can review, so the value is required.
# Prefixes match a whole directory.
SKIP_PATHS = {
    # "fixtures/recorded/": "captured upstream payloads, scrubbed by `make scrub`",
}

# Rules waived for this repository, each with the reason. The reason is printed
# with the waiver, so a reviewer sees what was traded away and why.
ALLOW = {
    # "OSS-204": "the repository installs nothing; it is the deliverable",
}

# Extensions a scan may skip because they are known binary assets. Anything else
# that cannot be read as text is reported: a file nobody can inspect must never
# be reported as clean, which is how a committed editor swap file once carried a
# username past a scan of this kind.
BINARY_OK = (".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".icns",
             ".woff", ".woff2", ".ttf", ".otf", ".pdf", ".zip", ".gz", ".tar",
             ".mp4", ".mov", ".wasm")

# The targets the project's task runner must carry. `check` is the one command a
# contributor runs before pushing, and it must reach the prose linter.
REQUIRED_TARGETS = ["build", "test", "check", "docs", "oss", "clean"]

# The rest of the family's vocabulary. A missing one is a warning: somebody who
# has worked on a sibling repository will reach for it.
EXPECTED_TARGETS = ["help", "install", "uninstall", "fmt", "fmt-check", "vet",
                    "lint"]
