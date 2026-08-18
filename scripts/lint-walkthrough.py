#!/usr/bin/env python3
"""Check that the documentation still describes the software it ships with.

The prose linter checks how the documents are written. The readiness linter
checks what the repository owes a reader it is published to. This one checks
the claims: that every command a document names exists, that every command the
tool carries is named, that the settings the code reads are the settings the
documents list, that a sample output is still what the command prints, and that
a tool the build needs is named somewhere a reader will find it.

    scripts/lint-walkthrough.py             # check
    scripts/lint-walkthrough.py --explain   # every rule, and what it wants
    scripts/lint-walkthrough.py --list      # what it found to check
    scripts/lint-walkthrough.py --run       # the ordered inventory of documented commands
    scripts/lint-walkthrough.py --review    # the pack a model reads for the rest

Errors fail the run. Warnings print and pass, because they flag a judgement
call rather than broken data.

Every check compares a document against something that cannot lie: the tool's
own help tree, the source that reads an environment variable, the build file
that shells out to a binary, or the command re-run right now. Nothing here
guesses what a document ought to say, because a linter that guesses produces
noise, and noise gets ignored. What needs judgement is left to --review.

TUNING. Nothing in this file is project-specific. Every knob lives in
lint-walkthrough.config.py beside it: the tool's name, the verbs a check may
run, the samples that cannot be reproduced here, and the waivers.

This file is vendored from a shared copy and is meant to stay byte-identical
across projects. Fix a check there, then copy it out again; the config file
beside it is what carries the local differences.

Needs python3 and git. A check that cannot run reports a skip rather than a
pass, so a run that verified nothing never reads as a run that verified
everything.
"""

import json
import os
import pathlib
import re
import shutil
import subprocess
import sys

ROOT = pathlib.Path(".").resolve()

# ---------------------------------------------------------------------------
# The project's knobs, read from the file beside this one. It is exec'd rather
# than imported because the hyphen in the name is not a legal module name, and
# renaming it would hide the pairing the two files have.
# ---------------------------------------------------------------------------
CONFIG = dict(
    TOOL="",                # the command name; guessed from the build file
    TOOL_PATH="",           # a built binary to prefer over one on PATH
    DOCS=["README.md", "INSTALL.md", "MANUAL.md", "SPEC.md", "CONTRIBUTING.md"],
    EXTRA_DOCS=[],
    ENV_PREFIX="",          # e.g. CS_VCR_; guessed from TOOL
    ENV_INTERNAL={},        # variable -> why it is deliberately undocumented
    SAFE_VERBS=[],          # verbs a sample check may re-run
    SAMPLE_SKIP={},         # a sample's first command -> why it cannot re-run here
    PLACEHOLDER_OK=[],      # placeholder paths a block may name on purpose
    PREREQ_OK=[],           # tools the build needs that no document has to name
    SOURCE_SKIP={},         # path prefix -> why its settings are not the tool's
    AGENT_SECTION="Notes for agents",
    ALLOW={},               # rule id -> why it is waived
)
_config_file = pathlib.Path(__file__).with_name("lint-walkthrough.config.py")
if _config_file.exists():
    exec(compile(_config_file.read_text(), str(_config_file), "exec"), CONFIG)

ALLOW = dict(CONFIG["ALLOW"])
ERROR, WARN, SKIP = "error", "warning", "skipped"

# This file necessarily contains the patterns it searches for.
SELF = "scripts/lint-walkthrough.py"

# Words that read as a command in prose but are not this tool's.
SHELL_NOISE = {
    "cd", "ls", "cat", "echo", "export", "sudo", "source", "curl", "grep",
    "sed", "awk", "mkdir", "rm", "cp", "mv", "tar", "install", "chmod",
    "chcon", "git", "make", "go", "npm", "node", "python3", "sh", "bash",
    "open", "less", "head", "tail", "diff", "find", "xargs", "jq", "ssh",
    "podman", "docker", "kubectl", "brew", "apt", "dnf", "systemctl",
}


# ---------------------------------------------------------------------------
# Plumbing
# ---------------------------------------------------------------------------
class Problem:
    def __init__(self, rule, severity, message, where=""):
        self.rule, self.severity = rule, severity
        self.message, self.where = message, where


RULES = []          # (id, severity, title, why, fn)


def rule(rid, severity, title, why):
    def wrap(fn):
        RULES.append((rid, severity, title, why, fn))
        return fn
    return wrap


def err(rid, message, where=""):
    return Problem(rid, ERROR, message, where)


def warn(rid, message, where=""):
    return Problem(rid, WARN, message, where)


def skipped(rid, message):
    return Problem(rid, SKIP, message)


def git(*args):
    """Run git and return stdout, or None when the call fails."""
    try:
        out = subprocess.run(["git", *args], cwd=ROOT,
                             capture_output=True, text=True, timeout=180)
    except (OSError, subprocess.SubprocessError):
        return None
    return out.stdout if out.returncode == 0 else None


def line_of(body, index):
    return body.count("\n", 0, index) + 1


class Block:
    """One fenced block, with where it came from and what it holds.

    `commands` are the lines a reader would type. In a ``console`` block those
    are the lines after a `$` prompt, and everything else is the output the
    document claims they print.
    """

    def __init__(self, doc, line, lang, body):
        self.doc, self.line, self.lang = doc, line, lang
        self.body = body
        self.commands = []
        self.output = []
        if lang == "console":
            current = None
            for raw in body.splitlines():
                if raw.startswith("$ "):
                    current = raw[2:]
                    self.commands.append(current)
                    self.output.append([])
                elif self.output:
                    self.output[-1].append(raw)
        else:
            joined = []
            continued = ""
            for raw in body.splitlines():
                line_text = raw.strip()
                if not line_text or line_text.startswith("#"):
                    continue
                if continued:
                    line_text = continued + " " + line_text
                    continued = ""
                if line_text.endswith("\\"):
                    continued = line_text[:-1].strip()
                    continue
                joined.append(line_text)
            self.commands = joined

    def where(self):
        return f"{self.doc}:{self.line}"


class Repo:
    """Everything the checks read, gathered once."""

    def __init__(self):
        listing = git("ls-files", "-z")
        self.tracked = [p for p in (listing or "").split("\0") if p]
        self.text = {}
        for path in self.tracked:
            full = ROOT / path
            try:
                if not full.is_file() or full.stat().st_size > 8 * 1024 * 1024:
                    continue
                self.text[path] = full.read_text(encoding="utf-8")
            except (OSError, UnicodeDecodeError):
                continue
        self._help = None
        self._blocks = None

    # -- the documents ------------------------------------------------------
    def docs(self):
        """The document set that is present, in the order a reader meets it."""
        names = list(CONFIG["DOCS"]) + list(CONFIG["EXTRA_DOCS"])
        return {n: self.text[n] for n in names if n in self.text}

    def prose(self):
        return "\n".join(self.docs().values())

    def blocks(self):
        if self._blocks is None:
            found = []
            for name, body in self.docs().items():
                for m in re.finditer(r"^```([a-zA-Z]*)\n(.*?)^```", body,
                                     re.S | re.M):
                    found.append(Block(name, line_of(body, m.start()),
                                       m.group(1).lower(), m.group(2)))
            self._blocks = found
        return self._blocks

    # -- the tool -----------------------------------------------------------
    def tool(self):
        if CONFIG["TOOL"]:
            return CONFIG["TOOL"]
        mk = self.text.get("Makefile", "") or self.text.get("makefile", "")
        m = re.search(r"^BIN\s*:?=\s*\S*?([\w.-]+)\s*$", mk, re.M)
        if m:
            return m.group(1)
        gor = self.text.get(".goreleaser.yaml", "") or \
            self.text.get(".goreleaser.yml", "")
        m = re.search(r"^project_name:\s*(\S+)", gor, re.M)
        if m:
            return m.group(1)
        return ROOT.name

    def binary(self):
        """A binary to ask for help, preferring the one this checkout built."""
        candidates = []
        if CONFIG["TOOL_PATH"]:
            candidates.append(ROOT / CONFIG["TOOL_PATH"])
        candidates.append(ROOT / "bin" / self.tool())
        for path in candidates:
            if path.is_file() and os.access(path, os.X_OK):
                return str(path)
        found = shutil.which(self.tool())
        return found

    def run_tool(self, *args, timeout=30):
        """Run the tool and return (exit status, stdout + stderr)."""
        binary = self.binary()
        if not binary:
            return None, ""
        try:
            out = subprocess.run([binary, *args], cwd=ROOT, timeout=timeout,
                                 capture_output=True, text=True)
        except (OSError, subprocess.SubprocessError):
            return None, ""
        return out.returncode, out.stdout + out.stderr

    def help_tree(self):
        """{verb path -> help text} for every verb the tool carries.

        Walked rather than assumed: a subcommand's own subcommands are where a
        surface goes undocumented, because nothing at the top level names them.
        """
        if self._help is not None:
            return self._help
        tree = {}
        if not self.binary():
            self._help = tree
            return tree
        pending = [()]
        seen = set()
        while pending:
            path = pending.pop(0)
            if path in seen or len(path) > 3:
                continue
            seen.add(path)
            status, text = self.run_tool(*path, "--help")
            if status is None:
                continue
            tree[path] = text
            # A tool with no per-verb help answers `<tool> <verb> --help` with
            # the page its parent gave, listing the same verbs again. Reading
            # those as children multiplies the tree by itself at every level, so
            # an identical page means this verb has no subcommands.
            if path and tree.get(path[:-1]) == text:
                continue
            for child in _verbs_in(text):
                pending.append(path + (child,))
        self._help = tree
        return tree

    def verbs(self):
        """Every verb path the tool carries, as a space-joined string."""
        return {" ".join(p) for p in self.help_tree() if p}

    def flags(self):
        """Every long flag the tool's help tree mentions.

        The completion subtree is left out: its flags come from the shell
        completion framework rather than from this project, and a document that
        named them would be documenting somebody else's surface.
        """
        found = set()
        for path, text in self.help_tree().items():
            if path[:1] == ("completion",):
                continue
            found |= set(re.findall(r"(--[a-z][a-z0-9-]+)", text))
        return found

    # -- the source ---------------------------------------------------------
    def source(self):
        """Tracked source files, which is where a setting is really read.

        Tests are left out. A variable only the suite reads is instrumentation
        rather than a setting, and documenting it would tell a user to reach for
        something built for the harness.
        """
        exts = (".go", ".rs", ".py", ".ts", ".js", ".java", ".rb", ".c", ".h",
                ".cpp", ".sh")
        for path, body in self.text.items():
            if path == SELF or path.startswith("vendor/"):
                continue
            if _is_test(path):
                continue
            if any(path.startswith(k) for k in CONFIG["SOURCE_SKIP"]):
                continue
            if path.endswith(exts):
                yield path, body

    def env_prefix(self):
        if CONFIG["ENV_PREFIX"]:
            return CONFIG["ENV_PREFIX"]
        return re.sub(r"[^A-Z0-9]", "_", self.tool().upper()) + "_"

    def build_files(self):
        for path, body in self.text.items():
            if path in ("Makefile", "makefile") or \
                    path.startswith("scripts/") or path.startswith("Taskfile"):
                yield path, body


READS = ("getenv", "lookupenv", "envor", "environ", "process.env", "env[",
         "env::var", "env.var", "env_var", "env(")


def _env_reads(prefix, body):
    """The prefixed variable names this file reads, with the write sites left out.

    A tool that hands a child process `-e NAME=value` is not reading NAME, and
    a list of those is the largest class of false positive an environment scan
    has. A read is the name inside a getenv-shaped call, or spelled $NAME.
    """
    names = set()
    for m in re.finditer(r"(\$\{?)?\b(" + re.escape(prefix) + r"[A-Z0-9_]+)\b", body):
        if m.group(1):
            names.add(m.group(2))
            continue
        window = body[max(0, m.start() - 60):m.start()].lower()
        if any(token in window for token in READS):
            names.add(m.group(2))
    return names


def _is_test(path):
    """Whether a path is test code, by the conventions the languages use."""
    name = os.path.basename(path)
    parts = path.split("/")
    return (
        "test" in parts or "tests" in parts or "spec" in parts
        or name.endswith(("_test.go", "_test.py", "_test.rb", "_spec.rb"))
        or name.startswith("test_")
        or ".test." in name or ".spec." in name
    )


def _matches(recorded, actual):
    """Whether a recorded line still matches what the command printed.

    An elision stands for whatever the command prints there, which is how a
    document keeps a sample true across a number that moves: a byte count, a
    duration, a path under a temporary directory. Everything either side of it
    still has to match, so the elision buys drift in one place rather than
    turning the whole line off.
    """
    if "…" not in recorded and "..." not in recorded:
        return recorded == actual
    parts = re.split(r"…|\.\.\.", recorded)
    pattern = ".*".join(re.escape(p) for p in parts)
    return re.fullmatch(pattern, actual, re.S) is not None


def _verb_of(words):
    """The verb path in an argv, with flags and the values they take removed.

    A flag's value is a word like any other, so `--cassette build` would read as
    a verb called build. Anything after a flag that carries no `=` is that
    flag's value unless it is another flag.
    """
    path, skip_next = [], False
    for word in words:
        if skip_next:
            skip_next = False
            if not word.startswith("-"):
                continue
        if word.startswith("-"):
            skip_next = "=" not in word
            continue
        path.append(word)
    return " ".join(path)


def _verbs_in(help_text):
    """The subcommand names a help page lists, from its commands section."""
    verbs = []
    listing = False
    for raw in help_text.splitlines():
        # "Commands:", "Available Commands:" and "verbs:" all head the same
        # list. A hand-rolled help page picks its own word, and a parser that
        # knows only cobra's finds nothing and reports nothing.
        if re.match(r"^\s*(available )?(commands|verbs):\s*$", raw, re.I):
            listing = True
            continue
        if listing:
            if not raw.strip():
                break
            # A row is a name, optionally the argument it takes, then the
            # description: `normalize <path>    write the JSON tree`. Without
            # the argument the row falls through and the verb goes undiscovered.
            m = re.match(r"^\s{1,6}([a-z][a-z0-9-]*)"
                         r"(?:\s+<[^>]+>|\s+\[[^\]]+\])?\s{2,}\S", raw)
            if m:
                verbs.append(m.group(1))
            elif raw.strip().startswith("-"):
                break
    return [v for v in verbs if v not in ("help", "completion")] + \
        [v for v in verbs if v in ("help", "completion")]


# ---------------------------------------------------------------------------
# 1xx — The command surface
# ---------------------------------------------------------------------------
@rule("WALK-101", ERROR, "Every command a document names exists",
      "A reader who types what the page says and gets 'unknown command' stops "
      "trusting the page, and cannot tell which of the rest is also stale.")
def check_documented_verbs_exist(repo):
    if not repo.binary():
        return [skipped("WALK-101", f"no {repo.tool()} binary to ask")]
    tool = repo.tool()
    carried = repo.verbs()
    named = {}
    for block in repo.blocks():
        for command in block.commands:
            words = command.split()
            if not words or os.path.basename(words[0]) != tool:
                continue
            path = []
            for word in words[1:]:
                if not re.match(r"^[a-z][a-z0-9-]*$", word):
                    break
                path.append(word)
                if " ".join(path) in carried:
                    continue
                break
            if path:
                named.setdefault(" ".join(path), block.where())
    problems = []
    for verb, where in sorted(named.items()):
        if verb in carried:
            continue
        # A leading word may be an argument rather than a verb, so only the
        # first word is a claim about the surface.
        head = verb.split()[0]
        if head in carried:
            continue
        status, _ = repo.run_tool(*verb.split(), "--help")
        if status not in (0, None):
            problems.append(err("WALK-101",
                                f"`{tool} {verb}` is documented and the binary "
                                f"does not have it", where))
    return problems


@rule("WALK-102", ERROR, "Every command the tool carries is documented",
      "A verb no document names is a verb nobody finds. The manual is the "
      "reference by the project's own doc map, so a gap there is a gap "
      "everywhere.")
def check_carried_verbs_documented(repo):
    if not repo.binary():
        return [skipped("WALK-102", f"no {repo.tool()} binary to ask")]
    prose = repo.prose()
    missing = []
    for verb in sorted(repo.verbs()):
        if verb == "help":
            continue
        last = verb.split()[-1]
        if re.search(r"\b" + re.escape(last) + r"\b", prose):
            continue
        missing.append(verb)
    if not missing:
        return []
    return [err("WALK-102",
                f"the binary carries {len(missing)} command(s) no document "
                f"names: {', '.join(missing)}")]


@rule("WALK-103", WARN, "Every flag the tool carries is documented",
      "An option nobody documents is an option nobody uses, and the manual "
      "claims to list them all.")
def check_carried_flags_documented(repo):
    if not repo.binary():
        return [skipped("WALK-103", f"no {repo.tool()} binary to ask")]
    prose = repo.prose()
    missing = sorted(f for f in repo.flags()
                     if f not in ("--help",) and f not in prose)
    if not missing:
        return []
    return [warn("WALK-103",
                 f"the binary carries {len(missing)} flag(s) no document "
                 f"names: {', '.join(missing)}")]


@rule("WALK-104", ERROR, "Every flag a document attributes to the tool exists",
      "A flag that was renamed leaves a document telling readers to pass one "
      "the parser rejects, and the error names the flag rather than the page.")
def check_documented_flags_exist(repo):
    if not repo.binary():
        return [skipped("WALK-104", f"no {repo.tool()} binary to ask")]
    tool = repo.tool()
    carried = repo.flags()
    problems = []
    seen = set()
    for block in repo.blocks():
        for command in block.commands:
            words = command.split()
            if not words or os.path.basename(words[0]) != tool:
                continue
            for word in words[1:]:
                flag = word.split("=")[0]
                if not re.match(r"^--[a-z][a-z0-9-]+$", flag):
                    continue
                if flag in carried or flag in seen or flag == "--help":
                    continue
                seen.add(flag)
                problems.append(err("WALK-104",
                                    f"`{flag}` is passed to {tool} in a "
                                    f"document and the binary has no such flag",
                                    block.where()))
    return problems


# ---------------------------------------------------------------------------
# 2xx — The settings surface
# ---------------------------------------------------------------------------
@rule("WALK-201", ERROR, "Every environment variable the code reads is documented",
      "A setting only the source names is a setting only its author knows, and "
      "one of them usually moves a boundary the spec states as a requirement.")
def check_env_documented(repo):
    prefix = repo.env_prefix()
    read = {}
    for path, body in repo.source():
        for name in _env_reads(prefix, body):
            read.setdefault(name, path)
    if not read:
        return [skipped("WALK-201", f"no {prefix}* variable is read in the source")]
    prose = repo.prose()
    internal = dict(CONFIG["ENV_INTERNAL"])
    missing = [(v, p) for v, p in sorted(read.items())
               if v not in prose and v not in internal]
    return [err("WALK-201", f"{v} is read by the code and named in no document",
                p) for v, p in missing]


@rule("WALK-202", WARN, "Every environment variable a document names is read",
      "A variable the code stopped reading still reads as a setting, and a "
      "reader who sets it is debugging a document rather than the software.")
def check_documented_env_read(repo):
    prefix = repo.env_prefix()
    read = set()
    for _, body in repo.source():
        read |= _env_reads(prefix, body)
    named = set()
    for name, body in repo.docs().items():
        named |= set(re.findall(r"\b(" + re.escape(prefix) + r"[A-Z0-9_]+)\b", body))
    if not named:
        return [skipped("WALK-202", f"no {prefix}* variable is named in the documents")]
    if not read:
        return [skipped("WALK-202", f"no {prefix}* variable is read in the source")]
    stale = sorted(named - read)
    return [warn("WALK-202", f"{v} is documented and the code does not read it")
            for v in stale]


# ---------------------------------------------------------------------------
# 3xx — The blocks a reader copies
# ---------------------------------------------------------------------------
PLACEHOLDERS = (
    r"~/projects/\S+", r"/path/to/\S+", r"<your[-\w]*>", r"~/my-\S+",
    r"/my-\S+", r"<PATH>", r"<name-of-\S+>",
)


@rule("WALK-301", WARN, "A block a reader copies names nothing they lack",
      "A walkthrough introduced as runnable end to end, opening on a repo the "
      "reader was never given, fails on its first line. Say the path is theirs "
      "to supply, or give them one.")
def check_placeholder_paths(repo):
    allowed = list(CONFIG["PLACEHOLDER_OK"])
    problems = []
    for block in repo.blocks():
        if block.lang not in ("bash", "sh", "shell", "console"):
            continue
        for command in block.commands:
            hits = []
            for pattern in PLACEHOLDERS:
                hits.extend(re.findall(pattern, command))
            for hit in hits:
                if any(ok in hit for ok in allowed):
                    continue
                # A shorter pattern matching inside a longer hit is the same
                # placeholder seen twice: /my-service inside ~/my-service.
                if any(other != hit and hit in other for other in hits):
                    continue
                problems.append(warn(
                    "WALK-301",
                    f"a command names {hit}, which the reader has to supply",
                    block.where()))
    # One report per document is enough to send someone to the section.
    seen, unique = set(), []
    for p in problems:
        key = (p.where, p.message)
        if key in seen:
            continue
        seen.add(key)
        unique.append(p)
    return unique


@rule("WALK-302", ERROR, "Every repository path a document names exists",
      "A file that moves leaves the documents pointing at where it was, and "
      "nothing fails. The reference is then wrong until somebody happens to "
      "read that line.")
def check_paths_exist(repo):
    roots = {p.split("/")[0] for p in repo.tracked}
    problems, seen = [], set()
    for name, body in repo.docs().items():
        for token in re.findall(r"[`\[(]([\w.@-]+/[\w./@-]+)[`\])]", body):
            token = token.rstrip(".,;:")
            head = token.split("/")[0]
            if head not in roots or token.startswith("http"):
                continue
            if (ROOT / token).exists() or token in repo.tracked:
                continue
            # A path under a generated or ignored tree is not a claim about a
            # tracked file, and a trailing wildcard names a family.
            if "*" in token or token.endswith("/"):
                continue
            # package/file.Symbol is a citation of code, not of a path. An
            # extension is short and lower case; a camelCase tail is a symbol.
            tail = token.split("/")[-1]
            if "." in tail:
                ext = tail.rsplit(".", 1)[1]
                if not re.fullmatch(r"[a-z0-9]{1,6}", ext):
                    continue
            key = (name, token)
            if key in seen:
                continue
            seen.add(key)
            problems.append(err("WALK-302",
                                f"{token} is named here and does not exist",
                                name))
    return problems


@rule("WALK-303", ERROR, "Every section citation resolves",
      "A citation like SPEC.md §7.2 is useful only while §7.2 says what it "
      "said. Renumbering breaks every one at once, and a stale citation sends "
      "a reader to a rule that now means something else.")
def check_citations_resolve(repo):
    problems, seen = [], set()
    for name, body in repo.docs().items():
        for m in re.finditer(r"(\w+\.md)\s*(?:§|section\s+)([\d.]+)", body):
            target, section = m.group(1), m.group(2).rstrip(".")
            text = repo.text.get(target)
            if text is None:
                continue
            if re.search(r"^#{1,6}\s+" + re.escape(section) + r"[.\s]", text, re.M):
                continue
            key = (name, target, section)
            if key in seen:
                continue
            seen.add(key)
            problems.append(err(
                "WALK-303", f"{target} has no section {section}",
                f"{name}:{line_of(body, m.start())}"))
    return problems


# ---------------------------------------------------------------------------
# 4xx — The samples
# ---------------------------------------------------------------------------
@rule("WALK-401", ERROR, "A sample output is what the command prints today",
      "Sample output is the half of a document a reader compares their own "
      "screen against. Wrong in small ways, it destroys trust in the rest.")
def check_samples_reproduce(repo):
    if not repo.binary():
        return [skipped("WALK-401", f"no {repo.tool()} binary to run")]
    safe = set(CONFIG["SAFE_VERBS"])
    if not safe:
        return [skipped("WALK-401", "no SAFE_VERBS configured, so no sample is re-run")]
    tool = repo.tool()
    skips = dict(CONFIG["SAMPLE_SKIP"])
    problems = []
    ran = 0
    for block in repo.blocks():
        if block.lang != "console":
            continue
        for command, output in zip(block.commands, block.output):
            if command in skips:
                continue
            # A sample is often two commands joined by &&, and the recorded
            # output covers both. Every part has to be safe before any runs.
            parts = [p.strip().split() for p in command.split("&&")]
            if not all(p and os.path.basename(p[0]) == tool for p in parts):
                continue
            verbs = [_verb_of(p[1:]) for p in parts]
            if any(v not in safe for v in verbs):
                continue
            text, status = "", 0
            for part in parts:
                status, chunk = repo.run_tool(*part[1:])
                text += chunk
                if status is None or status != 0:
                    break
            if status is None:
                continue
            ran += 1
            recorded = [l.rstrip() for l in output if l.strip()]
            actual = [l.rstrip() for l in text.splitlines() if l.strip()]
            for i, line in enumerate(recorded):
                if _matches(line, actual[i] if i < len(actual) else ""):
                    continue
                if any(_matches(line, a) for a in actual):
                    continue
                got = next((a for a in actual if a.strip()), "(nothing)")
                problems.append(err(
                    "WALK-401",
                    f"`{command}` no longer prints {line.strip()!r}; it "
                    f"prints {got.strip()!r}. Fix the sample, or name the "
                    f"command in SAMPLE_SKIP with the reason it cannot "
                    f"reproduce here",
                    block.where()))
                break
    if not ran:
        return [skipped("WALK-401", "no sample named a verb in SAFE_VERBS")]
    return problems


@rule("WALK-402", WARN, "A version a document quotes is the version it ships",
      "A sample naming a version that has moved is the cheapest kind of wrong, "
      "and a reader comparing their own output cannot tell which of you is "
      "stale.")
def check_versions_quoted(repo):
    if not repo.binary():
        return [skipped("WALK-402", f"no {repo.tool()} binary to ask")]
    status, text = repo.run_tool("version")
    if status is None or status != 0:
        return [skipped("WALK-402", f"`{repo.tool()} version` did not run")]
    current = set(re.findall(r"\b\d+\.\d+\.\d+\b", text))
    if not current:
        return [skipped("WALK-402", "the version output carries no x.y.z number")]
    problems = []
    for name, body in repo.docs().items():
        for m in re.finditer(r"^\s*(?:\$\s*)?(?:\S*/)?" + re.escape(repo.tool()) +
                             r"\s+version\s*$", body, re.M):
            tail = body[m.end():m.end() + 400]
            quoted = set(re.findall(r"\b\d+\.\d+\.\d+\b", tail.split("```")[0]))
            stale = sorted(q for q in quoted if q not in current)
            if stale:
                problems.append(warn(
                    "WALK-402",
                    f"a `{repo.tool()} version` sample quotes "
                    f"{', '.join(stale)}; the binary prints "
                    f"{', '.join(sorted(current))}",
                    f"{name}:{line_of(body, m.start())}"))
    return problems


# ---------------------------------------------------------------------------
# 5xx — The prerequisites
# ---------------------------------------------------------------------------
@rule("WALK-501", ERROR, "Every tool the build needs is named in a document",
      "A contributor told to run one command, on a machine missing a tool "
      "nothing named, meets the gate as a failure rather than as a setup step. "
      "The answer lives in an error message only a failed run prints.")
def check_prereqs_documented(repo):
    prose = repo.prose()
    ok = set(CONFIG["PREREQ_OK"]) | {repo.tool()}
    needed = {}
    for path, body in repo.build_files():
        for m in re.finditer(r"command -v ([\w.-]+)", body):
            name = m.group(1)
            if name in ok or name.startswith("$"):
                continue
            needed.setdefault(name, path)
    missing = [(t, p) for t, p in sorted(needed.items())
               if not re.search(r"[`\s/]" + re.escape(t) + r"[`\s.,@]", prose)]
    if not needed:
        return [skipped("WALK-501", "the build shells out to nothing it checks for")]
    return [err("WALK-501",
                f"{t} has to be installed for the build and no document names it",
                p) for t, p in missing]


# ---------------------------------------------------------------------------
# 6xx — The agent's path
# ---------------------------------------------------------------------------
@rule("WALK-601", WARN, "The manual answers the automated caller",
      "An agent driving the tool needs what a human infers: which commands are "
      "non-interactive, which output is machine-readable, and what touches the "
      "network or the filesystem.")
def check_agent_section(repo):
    manual = repo.docs().get("MANUAL.md")
    if manual is None:
        return [skipped("WALK-601", "no MANUAL.md in the document set")]
    if CONFIG["AGENT_SECTION"].lower() in manual.lower():
        return []
    return [warn("WALK-601",
                 f"MANUAL.md has no \"{CONFIG['AGENT_SECTION']}\" section",
                 "MANUAL.md")]


@rule("WALK-602", ERROR, "The router names every document in the set",
      "An agent reads the AGENTS.md nearest the file it is editing and goes no "
      "further. A document the router omits is a document it never opens.")
def check_router_complete(repo):
    router = repo.text.get("AGENTS.md")
    if router is None:
        return [skipped("WALK-602", "no AGENTS.md at the root")]
    missing = [n for n in repo.docs() if n != "AGENTS.md" and n not in router]
    return [err("WALK-602", f"AGENTS.md does not name {n}", "AGENTS.md")
            for n in missing]


# ---------------------------------------------------------------------------
# The inventory --run prints
# ---------------------------------------------------------------------------
def inventory(repo):
    """Every command the documents tell a reader to run, in reading order.

    This is the walkthrough's checklist. A reader works down it, and so does an
    agent: each line names the document, the line, and the command, so a run
    can be recorded against a stable address rather than a paraphrase.
    """
    tool = repo.tool()
    safe = set(CONFIG["SAFE_VERBS"])
    rows = []
    for block in repo.blocks():
        if block.lang not in ("bash", "sh", "shell", "console"):
            continue
        for command in block.commands:
            words = command.split()
            if not words:
                continue
            head = os.path.basename(words[0])
            if head == tool:
                kind = "safe" if _verb_of(words[1:]) in safe else "tool"
            elif head in SHELL_NOISE:
                kind = "host"
            else:
                kind = "other"
            note = ""
            for pattern in PLACEHOLDERS:
                hit = re.search(pattern, command)
                if hit:
                    note = f"needs a path you supply: {hit.group(0)}"
                    break
            rows.append((block.where(), kind, command, note))
    return rows


# ---------------------------------------------------------------------------
# The review pack
# ---------------------------------------------------------------------------
REVIEWS = [
    {
        "id": "REV-W1",
        "title": "The first ten minutes",
        "evidence": ["cat README.md", "cat INSTALL.md"],
        "ask": """Take the position of somebody who has just found this project
and has decided to try it. Read README.md and then INSTALL.md in that order,
and stop at the first sentence you cannot act on.

Report, in reading order, every place where the page asks you to know something
it never told you: a term used before it is introduced, a file you were never
given, a command whose output you cannot predict, a decision you are asked to
make with no basis, a step whose success you cannot tell from its failure.

For each one quote the line and give the sentence that would fix it.""",
    },
    {
        "id": "REV-W2",
        "title": "The concepts the software actually has",
        "evidence": ["cat README.md", "cat SPEC.md"],
        "ask": """List the concepts a user has to hold to use this software:
the nouns its commands act on, the relationships between them, and the rules a
user cannot infer. Then check each one against the documents.

Report the concepts the documents name but never explain, the ones they explain
twice in different words, and the ones the software has that they never name at
all. Say which document each belongs in.""",
    },
    {
        "id": "REV-W3",
        "title": "The agent's first minutes",
        "evidence": ["cat AGENTS.md", "cat CONTRIBUTING.md",
                     "sed -n '/Notes for agents/,/^## /p' MANUAL.md"],
        "ask": """Take the position of a coding agent that has just been given
this repository and a task. You enter through AGENTS.md, not the README.

Answer four questions using only what the repository says, and quote the line
that answered each: what am I allowed to change, what do I run before
committing, where do I put something I found but am not fixing, and which
commands will block waiting for a human. Where the answer is not in the
repository, say so and name the document it belongs in.

Then test the claims addressed to you. For every command the manual calls
non-interactive, find its implementation and confirm it prompts for nothing and
waits for nothing. Report each one that does.""",
    },
    {
        "id": "REV-W4",
        "title": "The claims the code does not support",
        "evidence": ["cat MANUAL.md", "cat SPEC.md"],
        "ask": """Read every factual claim the documents make about behaviour:
defaults, precedence, exit codes, what a flag does, what is written where, what
is never done. Confirm each against the source.

Report every claim the code contradicts, and every claim that is true only
under a condition the document does not state. A default that depends on the
host is the common case, and it is usually written as though it were fixed.""",
    },
    {
        "id": "REV-W5",
        "title": "What a run leaves behind",
        "evidence": ["cat MANUAL.md"],
        "ask": """For each command that writes anything, list what it creates,
where, and what removes it. Then check the documents say so.

Pay attention to the commands that promise not to write: a dry run, a check, a
report, a verify. Read their implementation and confirm the promise holds all
the way down, including the state the tool keeps for itself. A flag honoured by
one layer and ignored by another is the failure this review exists to find.""",
    },
    {
        "id": "REV-W6",
        "title": "The route nobody takes twice",
        "evidence": ["cat INSTALL.md"],
        "ask": """Work through every installation route the page offers, in the
order it offers them, and say for each whether it works today. Run what can be
run here. For a route that cannot be run, name what it depends on and check
that dependency exists: a releases page with an archive on it, a package in a
registry, an image in a repository.

A route that cannot work today and does not say so is the finding. It is the
first thing a new reader tries.""",
    },
]


def render_reviews(repo):
    out = [f"# Walkthrough review pack — {repo.tool()}", ""]
    out.append("The mechanical checks have already compared the documents "
               "against the tool's help tree, the settings the code reads, the "
               "samples it prints and the tools the build needs. Do not repeat "
               "them. Each review below needs a reader rather than a pattern.")
    out.append("")
    for review in REVIEWS:
        out.append(f"## {review['id']} — {review['title']}")
        out.append("")
        out.append("Evidence to gather first:")
        for command in review["evidence"]:
            out.append(f"    {command}")
        out.append("")
        out.append(" ".join(review["ask"].split()).replace(". ", ".\n"))
        out.append("")
    return "\n".join(out)


# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------
def run_checks(repo):
    problems = []
    for rid, _severity, _title, _why, fn in RULES:
        if rid in ALLOW:
            problems.append(skipped(rid, f"waived: {ALLOW[rid]}"))
            continue
        try:
            problems.extend(fn(repo))
        except Exception as exc:                       # noqa: BLE001
            problems.append(skipped(rid, f"the check itself failed: {exc}"))
    return problems


def main():
    argv = sys.argv[1:]

    if "--explain" in argv:
        for rid, severity, title, why, _ in RULES:
            print(f"{rid}  [{severity}]  {title}")
            print("    " + " ".join(why.split()))
            print()
        return 0

    repo = Repo()
    if not repo.tracked:
        print("walkthrough: this is not a git repository, or it tracks nothing")
        return 1

    if "--list" in argv:
        print(f"tool:      {repo.tool()}")
        print(f"binary:    {repo.binary() or '(not built and not on PATH)'}")
        print(f"docs:      {', '.join(repo.docs()) or '(none found)'}")
        print(f"blocks:    {len(repo.blocks())} fenced")
        print(f"verbs:     {len(repo.verbs())} in the help tree")
        print(f"env:       {repo.env_prefix()}*")
        print(f"safe:      {', '.join(CONFIG['SAFE_VERBS']) or '(none)'}")
        print(f"waived:    {', '.join(sorted(ALLOW)) or '(none)'}")
        return 0

    if "--run" in argv:
        rows = inventory(repo)
        width = max((len(w) for w, _, _, _ in rows), default=0)
        for where, kind, command, note in rows:
            print(f"{where:<{width}}  {kind:<5}  {command}")
            if note:
                print(f"{'':<{width}}         ^ {note}")
        print()
        print(f"walkthrough: {len(rows)} documented command(s). "
              f"`safe` re-runs under the sample check; run the rest yourself, "
              f"in order, and record what each one did.")
        return 0

    if "--review" in argv:
        print(render_reviews(repo))
        return 0

    problems = run_checks(repo)
    errors = [p for p in problems if p.severity == ERROR]
    warnings = [p for p in problems if p.severity == WARN]
    skips = [p for p in problems if p.severity == SKIP]

    if "--json" in argv:
        print(json.dumps([{"rule": p.rule, "severity": p.severity,
                           "message": p.message, "where": p.where}
                          for p in problems], indent=2))
        return 1 if errors else 0

    for group, label in ((errors, "ERROR"), (warnings, "warning")):
        for p in group:
            where = f"  [{p.where}]" if p.where else ""
            print(f"{label:>7}  {p.rule}  {p.message}{where}")
    if "--verbose" in argv:
        for p in skips:
            print(f"   skip  {p.rule}  {p.message}")

    print()
    print(f"walkthrough: {len(errors)} error(s), {len(warnings)} warning(s), "
          f"{len(skips)} skipped. `--explain` says what each rule wants.")
    if not errors and not warnings:
        print("     `--run` lists the commands the documents tell a reader to "
              "run, and `--review` is the half that needs a reader.")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
