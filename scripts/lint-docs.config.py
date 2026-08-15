# Project-specific knobs for scripts/lint-docs.py.
#
# The linter beside this file is vendored and stays byte-identical across
# projects. Everything that differs between them lives here, so a fix to a check
# can be copied out again without carrying one project's vocabulary into the
# next. Tune until every reported problem is a real one: a check that cries wolf
# is worse than no check.

# Directories holding fixtures, corpora or generated Markdown, and root-level
# .md files that are data rather than documentation. Added to the linter's own
# list, which already covers node_modules, vendor, dist, bin, target, build,
# third_party, testdata and CHANGELOG.md.
#
# image/ is embedded in the binary (assets.go) and shipped into the sandbox.
# bin/ci-assets is a generated copy of it, and bin/ is already on the list
# above. Both are payload rather than this project's prose.
SKIP_EXTRA = {"image"}

# The domain terms a reader of this project cannot infer. Each must be
# introduced where a document first uses it: glossed on the spot, defined in a
# glossary table, or linked to the page that defines it.
#
# An empty list disables the most valuable check. Leave out any term that
# collides with a common verb: the check cannot tell the noun "pin" from "pins"
# and will fire on every ordinary use of the latter.
GLOSSARY = []

# Words that legitimately start a sentence in lower case, which is nearly always
# the project's own command name. Without them the splitter reads "Nothing
# prompts. cs-sandbox exits 1." as one 6-word sentence and reports a length that
# is not real.
LOWERCASE_STARTERS = ["cs-sandbox"]

# Verbs the shared list does not carry, added when a real verb trips the epigram
# check. Regex fragments rather than literals, so "mints?" covers both numbers.
# Only what is this project's own belongs here: an ordinary English verb should
# go into SHARED_VERBS in the linter and be vendored back out to everyone.
#
# runuser is the command, used as a verb in INSTALL.md; not English.
PROJECT_VERBS = ["runuser"]
