#!/usr/bin/env python3
"""Check that this repository is in a shape it can be published in.

The rules are what a published project owes a reader: a licence, a document set,
a build a stranger's clone can run, a release they can verify, and nothing in
the tree or in any past commit that was never meant to leave the machine it was
written on.

    scripts/lint-oss.py             # check
    scripts/lint-oss.py --explain   # every rule, and what it wants
    scripts/lint-oss.py --list      # what it found to check
    scripts/lint-oss.py --online    # and ask GitHub about the repository itself
    scripts/lint-oss.py --review    # the pack a model reads for the rest
    scripts/lint-oss.py --review --agent    # and hand it to one

Errors fail the run. Warnings print and pass, because they flag a judgement call
rather than broken data.

Every pattern matches a class rather than a name, so nothing private is written
down here: a username is the segment after /home/ that is not a placeholder this
project ships, and the name of whoever runs the check comes from the environment.
A term no pattern can infer goes in .leakterms at the root, which is gitignored.
Anything needing judgement is left to --review, because a linter that guesses
produces noise, and noise gets ignored.

TUNING. Nothing in this file is project-specific. Every knob lives in
lint-oss.config.py beside it: the placeholder home names, the mail domains that
are documentation addresses, the paths a scan skips, and the rules this
repository waives with a reason.

This file is vendored from a shared copy and is meant to stay byte-identical
across projects. Fix a check there, then copy it out again; the config file
beside it is what carries the local differences.

Needs python3 and git. Everything else is optional, and its absence is reported
as a skip rather than a pass.
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
    PROJECT="", GITHUB_REPO="", PUBLISHED=False,
    DOC_SET=["README.md", "INSTALL.md", "MANUAL.md", "SPEC.md",
             "CONTRIBUTING.md", "AGENTS.md"],
    EXTRA_DOCS=[], HOME_ALLOW={"user", "you", "name", "runner"},
    EMAIL_ALLOW=set(), SKIP_PATHS={}, ALLOW={},
    BINARY_OK=(".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".icns",
               ".woff", ".woff2", ".ttf", ".otf", ".pdf", ".zip", ".gz",
               ".tar", ".mp4", ".mov", ".wasm"),
    REQUIRED_TARGETS=["build", "test", "check", "docs", "oss", "clean"],
    EXPECTED_TARGETS=["help", "install", "uninstall", "fmt", "fmt-check",
                      "vet", "lint"],
)
_config_file = pathlib.Path(__file__).with_name("lint-oss.config.py")
if _config_file.exists():
    exec(compile(_config_file.read_text(), str(_config_file), "exec"), CONFIG)

DOC_SET = list(CONFIG["DOC_SET"])
EXTRA_DOCS = list(CONFIG["EXTRA_DOCS"])
ALL_DOCS = DOC_SET + EXTRA_DOCS
ALLOW = dict(CONFIG["ALLOW"])
SKIP_PATHS = dict(CONFIG["SKIP_PATHS"])
BINARY_OK = tuple(CONFIG["BINARY_OK"])

# Domains reserved by RFC 2606 and RFC 6761 for documentation and testing, plus
# the noreply identities a bot commits under. Every project gets these; the
# config adds only what is its own.
RESERVED_DOMAINS = {"example.com", "example.org", "example.net",
                    "noreply.github.com"}
RESERVED_TLDS = {"test", "example", "invalid", "localhost", "local"}

ERROR, WARN, SKIP = "error", "warning", "skipped"

# This file necessarily contains the patterns it searches for.
SELF = "scripts/lint-oss.py"


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


def excerpt(text, index, before=40, after=60):
    """A bounded quote. A generated page puts its whole payload on one line."""
    start = max(0, index - before)
    lead = "…" if start > 0 else ""
    return lead + " ".join(text[start:index + after].split()) + "…"


def line_of(body, index):
    return body.count("\n", 0, index) + 1


class Repo:
    """Everything the checks read, gathered once."""

    def __init__(self):
        listing = git("ls-files", "-z")
        self.tracked = [p for p in (listing or "").split("\0") if p]
        self.text = {}          # path -> contents, for what reads as text
        self.unreadable = []    # tracked, not a known asset, not decodable
        for path in self.tracked:
            if path.lower().endswith(BINARY_OK):
                continue
            full = ROOT / path
            try:
                if not full.is_file() or full.stat().st_size > 40 * 1024 * 1024:
                    continue
                self.text[path] = full.read_text(encoding="utf-8")
            except (OSError, UnicodeDecodeError):
                self.unreadable.append(path)
        self._leaks = None

    def scannable(self):
        """The tracked text files the leak scans read.

        Every tracked file, not a chosen subset. Leaks have turned up in
        fixtures, in goldens derived from them, in a committed manifest, in
        docs, and in a script with a hard-coded path. Narrowing the scope is
        how the second round of a scrub survives the first.
        """
        for path, body in self.text.items():
            if path == SELF:
                continue
            if any(path == k or path.startswith(k) for k in SKIP_PATHS):
                continue
            yield path, body

    def read(self, name):
        return self.text.get(name)

    def has(self, name):
        return name in self.tracked or (ROOT / name).exists()

    def workflows(self):
        return {p: b for p, b in self.text.items()
                if p.startswith(".github/workflows/")
                and p.endswith((".yml", ".yaml"))}

    def ci(self):
        return self.read(".github/workflows/ci.yml") or \
            self.read(".github/workflows/ci.yaml")

    def makefile(self):
        return self.read("Makefile") or self.read("makefile") or ""

    def goreleaser(self):
        return self.read(".goreleaser.yaml") or self.read(".goreleaser.yml")

    def keeps_ledger(self):
        return any(p.startswith("ledger/") for p in self.tracked)

    def project(self):
        if CONFIG["PROJECT"]:
            return CONFIG["PROJECT"]
        m = re.search(r"^BIN\s*:?=\s*\S*?([\w.-]+)\s*$", self.makefile(), re.M)
        if m:
            return m.group(1)
        m = re.search(r"^project_name:\s*(\S+)", self.goreleaser() or "", re.M)
        if m:
            return m.group(1)
        return ROOT.name

    def slug(self):
        """owner/name, from the config or the origin remote."""
        if CONFIG["GITHUB_REPO"]:
            return CONFIG["GITHUB_REPO"]
        url = (git("remote", "get-url", "origin") or "").strip()
        m = re.search(r"[:/]([\w.-]+)/([\w.-]+?)(?:\.git)?$", url)
        return f"{m.group(1)}/{m.group(2)}" if m else ""

    def leaks(self):
        """The leak scan, run once and shared by the rules that read it."""
        if self._leaks is None:
            self._leaks = _scan(self)
        return self._leaks


# ---------------------------------------------------------------------------
# 1xx — Legal
# ---------------------------------------------------------------------------
@rule("OSS-101", ERROR, "A licence file sits at the repository root",
      "Without one the code is All Rights Reserved by default, whatever the "
      "README says, and nobody may legally use it.")
def check_license(repo):
    for name in ("LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE"):
        if repo.has(name):
            return []
    return [err("OSS-101", "no LICENSE at the repository root")]


def _license_text(repo):
    for name in ("LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE"):
        body = repo.read(name)
        if body is not None:
            return body
    return None


@rule("OSS-102", ERROR, "The licence is the full Apache 2.0 text",
      "A summary, a link or an SPDX line is not a grant. GitHub also detects a "
      "licence by matching the full text, and shows nothing for a file it "
      "cannot match.")
def check_license_text(repo):
    body = _license_text(repo)
    if body is None:
        return [skipped("OSS-102", "no LICENSE to read")]
    wanted = ["Apache License", "Version 2.0, January 2004",
              "TERMS AND CONDITIONS FOR USE, REPRODUCTION, AND DISTRIBUTION"]
    missing = [w for w in wanted if w not in body]
    if missing:
        return [err("OSS-102", "LICENSE does not read as the Apache 2.0 text "
                               f"(missing {missing[0]!r})")]
    return []


@rule("OSS-103", ERROR, "The licence appendix names a copyright holder",
      "Apache 2.0 ships with [yyyy] and [name of copyright owner] "
      "placeholders. Leaving them in publishes a licence that grants rights on "
      "behalf of nobody.")
def check_license_copyright(repo):
    body = _license_text(repo)
    if body is None:
        return [skipped("OSS-103", "no LICENSE to read")]
    if re.search(r"\[yyyy\]|\[name of copyright owner\]", body):
        return [err("OSS-103", "LICENSE still carries the Apache placeholders")]
    if not re.search(r"Copyright\s+(?:\(c\)\s*)?\d{4}\s+\S", body):
        return [err("OSS-103", "LICENSE has no filled-in copyright line")]
    return []


@rule("OSS-104", ERROR, "The README says what the licence is",
      "A reader deciding whether they may use this looks for one line. A "
      "repository that makes them open a 200-line legal file to find out loses "
      "them.")
def check_readme_license(repo):
    body = repo.read("README.md")
    if body is None:
        return [skipped("OSS-104", "no README.md")]
    section = re.search(r"^##+\s*Licen[cs]e\b", body, re.M | re.I)
    if not section:
        return [err("OSS-104", "README.md has no License section")]
    if "LICENSE" not in body[section.start():section.start() + 600]:
        return [err("OSS-104", "the README's License section does not link LICENSE")]
    return []


@rule("OSS-105", WARN, "Borrowed code carries the licence it came under",
      "Vendoring someone else's work strips its licence unless you carry it. "
      "The obligation survives the copy.")
def check_vendor_license(repo):
    roots = set()
    for path in repo.tracked:
        parts = path.split("/")
        for i, part in enumerate(parts[:-1]):
            if part in ("vendor", "third_party", "thirdparty"):
                roots.add("/".join(parts[:i + 2]))
    out = []
    for tree in sorted(roots):
        if any(p.startswith(tree) and
               re.search(r"/(LICENSE|LICENCE|NOTICE|COPYING)", "/" + p, re.I)
               for p in repo.tracked):
            continue
        out.append(warn("OSS-105", f"{tree}/ carries no LICENSE or NOTICE"))
    return out


@rule("OSS-106", WARN, "One licence, stated once",
      "Two licence files at the root leave a reader guessing which one "
      "governs, and GitHub picks whichever it matches first.")
def check_one_license(repo):
    found = [p for p in repo.tracked
             if re.fullmatch(r"(LICENSE|LICENCE|COPYING)(\.\w+)?", p, re.I)]
    if len(found) > 1:
        return [warn("OSS-106", "more than one root licence file: " + ", ".join(found))]
    return []


# ---------------------------------------------------------------------------
# 2xx — The document set
# ---------------------------------------------------------------------------
@rule("OSS-201", ERROR, "The document set is complete",
      "Each document answers one question a reader arrives with. A missing one "
      "means that question is answered nowhere, or answered inside a document "
      "whose readers did not ask it.")
def check_doc_set(repo):
    return [err("OSS-201", f"{d} is missing") for d in ALL_DOCS if not repo.has(d)]


@rule("OSS-202", ERROR, "AGENTS.md routes to every document",
      "An agent harness discovers AGENTS.md by filename, so it is read whether "
      "or not anyone points at it. A router that omits a document is why that "
      "document goes unread.")
def check_agents_router(repo):
    body = repo.read("AGENTS.md")
    if body is None:
        return [skipped("OSS-202", "no AGENTS.md")]
    missing = [d for d in ALL_DOCS if d != "AGENTS.md" and d not in body]
    out = []
    if missing:
        out.append(err("OSS-202", "AGENTS.md does not route to " + ", ".join(missing)))
    if repo.keeps_ledger() and "ledger/AGENTS.md" not in body:
        out.append(err("OSS-202", "AGENTS.md does not route to ledger/AGENTS.md"))
    return out


@rule("OSS-203", WARN, "AGENTS.md routes and holds no knowledge of its own",
      "A second copy of a fact goes stale. The file exists to point at the "
      "documents, so anything it explains is something a document should.")
def check_agents_thin(repo):
    body = repo.read("AGENTS.md")
    if body is None:
        return [skipped("OSS-203", "no AGENTS.md")]
    out = []
    lines = len(body.splitlines())
    if lines > 40:
        out.append(warn("OSS-203", f"AGENTS.md is {lines} lines; a router is under 40"))
    if "routes" not in body:
        out.append(warn("OSS-203", "AGENTS.md does not say that it routes, so a "
                                   "reader treats it as a source"))
    return out


@rule("OSS-204", WARN, "The README opens the way the family's do",
      "A stranger reaching the repository from a search result decides in "
      "under a minute. The name, one bold sentence and the badge block are "
      "what they read.")
def check_readme_shape(repo):
    body = repo.read("README.md")
    if body is None:
        return [skipped("OSS-204", "no README.md")]
    head = body.split("\n")[:16]
    out = []
    if not head or not head[0].startswith("# "):
        out.append(warn("OSS-204", "README.md does not open with an H1"))
    else:
        name = head[0][2:].strip()
        slug = repo.slug().split("/")[-1]
        if slug and name.lower() not in (slug.lower(), repo.project().lower()):
            out.append(warn("OSS-204", f"the README's H1 is {name!r}, and the "
                                       f"repository is named {slug!r}"))
    if not any(l.startswith("> **") for l in head):
        out.append(warn("OSS-204", "no bold one-sentence claim in the opening lines"))
    if "img.shields.io/badge/license" not in body.lower():
        out.append(warn("OSS-204", "no licence badge"))
    return out


@rule("OSS-205", ERROR, "The README's CI badge points at this repository",
      "A badge copied from a sibling reports that sibling's build. It stays "
      "green while this one is broken, which is worse than having no badge.")
def check_ci_badge(repo):
    body = repo.read("README.md")
    if body is None:
        return [skipped("OSS-205", "no README.md")]
    badges = re.findall(
        r"https://github\.com/([\w.-]+/[\w.-]+)/actions/workflows/[\w.-]+", body)
    if not badges:
        return [err("OSS-205", "the README carries no CI badge")]
    slug = repo.slug()
    if slug:
        wrong = sorted({b for b in badges if b.lower() != slug.lower()})
        if wrong:
            return [err("OSS-205", f"the CI badge names {wrong[0]}, not {slug}")]
    return []


@rule("OSS-206", ERROR, "Every relative link resolves",
      "A link to a file that is not there is the classic residue of splitting "
      "a document, and it is the first thing a new reader clicks.")
def check_links(repo):
    out = []
    for path, body in repo.scannable():
        if not path.endswith(".md"):
            continue
        base = (ROOT / path).parent
        for m in re.finditer(r"\[[^\]]*\]\(<?([^)\s>]+)>?(?:\s+\"[^\"]*\")?\)", body):
            target = m.group(1)
            if target.startswith(("http://", "https://", "mailto:", "#",
                                  "tel:", "data:")):
                continue
            file_part = target.split("#", 1)[0]
            if not file_part:
                continue
            if not (base / file_part).resolve().exists():
                out.append(err("OSS-206", f"link to {target} does not resolve",
                               f"{path}:{line_of(body, m.start())}"))
    return out


def _slugs(body):
    """The anchors GitHub mints for a document's headings."""
    out = set()
    for line in body.split("\n"):
        m = re.match(r"^(#{1,6})\s+(.*?)\s*#*$", line)
        if not m:
            continue
        text = re.sub(r"`([^`]*)`", r"\1", m.group(2))
        text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", text)
        text = re.sub(r"[*_]", "", text)
        out.add(re.sub(r"[^\w\- ]", "", text.lower()).strip().replace(" ", "-"))
    return out


@rule("OSS-207", ERROR, "Every anchor resolves to a heading",
      "Renumbering a section silently breaks every link into it. Nothing about "
      "the link looks wrong, so only this check finds it.")
def check_anchors(repo):
    out, cache = [], {}
    for path, body in repo.scannable():
        if not path.endswith(".md"):
            continue
        base = (ROOT / path).parent
        for m in re.finditer(r"\[[^\]]*\]\(<?([^)\s>]*#[^)\s>]+)>?\)", body):
            target = m.group(1)
            if target.startswith(("http://", "https://")):
                continue
            file_part, _, anchor = target.partition("#")
            doc = (base / file_part).resolve() if file_part else (ROOT / path).resolve()
            key = str(doc)
            if key not in cache:
                try:
                    cache[key] = _slugs(doc.read_text(encoding="utf-8"))
                except (OSError, UnicodeDecodeError):
                    cache[key] = None
            known = cache[key]
            if known is None or anchor.lower() in known:
                continue
            out.append(err("OSS-207", f"anchor #{anchor} matches no heading in "
                                      f"{file_part or path}",
                           f"{path}:{line_of(body, m.start())}"))
    return out


@rule("OSS-208", ERROR, "CONTRIBUTING states the conventions it expects",
      "A contributor who cannot find the commit rule invents one, and a "
      "reviewer then argues for a convention nobody wrote down.")
def check_contributing(repo):
    body = repo.read("CONTRIBUTING.md")
    if body is None:
        return [skipped("OSS-208", "no CONTRIBUTING.md")]
    out = []
    for heading in ("Commits", "Writing"):
        if not re.search(rf"^##+\s*.*\b{heading}\b", body, re.M | re.I):
            out.append(err("OSS-208", f"CONTRIBUTING.md has no {heading} section"))
    if not re.search(r"\bmake check\b|\bnpm (run )?check\b|scripts/check", body):
        out.append(err("OSS-208", "CONTRIBUTING.md never names the one command "
                                  "to run before pushing"))
    if not re.search(r"trailer", body, re.I):
        out.append(err("OSS-208", "CONTRIBUTING.md states no rule about git "
                                  "trailers, so an agent-written commit keeps "
                                  "whatever its harness appends"))
    return out


@rule("OSS-209", WARN, "The release archive ships the documents",
      "Someone who downloads the release and never visits the repository has "
      "only what the archive carries.")
def check_release_docs(repo):
    body = repo.goreleaser()
    if body is None:
        return [skipped("OSS-209", "no goreleaser manifest")]
    missing = [d for d in ALL_DOCS + ["LICENSE"]
               if d != "AGENTS.md" and d not in body]
    if missing:
        return [warn("OSS-209", "the release archive omits " + ", ".join(missing))]
    return []


@rule("OSS-210", WARN, "No stray document at the root",
      "A root file outside the set is a document with no stated job, and the "
      "reader has no way to know which question it answers.")
def check_stray_docs(repo):
    known = set(ALL_DOCS) | {"CHANGELOG.md", "NOTICE.md", "CODE_OF_CONDUCT.md",
                             "SECURITY.md"}
    stray = [p for p in repo.tracked
             if p.endswith(".md") and "/" not in p and p not in known]
    if stray:
        return [warn("OSS-210", "root documents outside the set: " + ", ".join(stray))]
    return []


# Language that invites a security report, and the shapes an answer takes.
REPORTS_SECURITY = re.compile(
    r"security[- ](?:issue|sensitive|bug|report|vulnerabilit)|vulnerabilit", re.I)
NAMES_A_CHANNEL = re.compile(
    r"https?://|mailto:|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}"
    r"|private vulnerability reporting|security advisor", re.I)


@rule("OSS-212", ERROR, "A security report has somewhere to go",
      "\"Ask for a private contact\" sends a reporter to the public tracker to "
      "request the private channel they were trying to use. A promise with "
      "nothing behind it is worse than saying nothing, because it reads as an "
      "answer.")
def check_security_contact(repo):
    body = repo.read("CONTRIBUTING.md")
    if body is None:
        return [skipped("OSS-212", "no CONTRIBUTING.md")]
    if repo.has("SECURITY.md"):
        return []
    # Flattened first: a wrapped paragraph puts a newline inside the very
    # phrase that names the channel.
    paras = [" ".join(p.split()) for p in body.split("\n\n")]
    invites = [p for p in paras if REPORTS_SECURITY.search(p)]
    if not invites:
        return [warn("OSS-212", "nothing says where a security report goes, so it "
                                "will arrive in the public tracker")]
    named = [p for p in invites if NAMES_A_CHANNEL.search(p)]
    if not named:
        quote = " ".join(invites[0].split())[:110]
        return [err("OSS-212", f"a security report is invited but no channel is "
                               f"named — {quote}")]
    return []


# A repository-relative path with an extension. Anything with a scheme, a
# glob, a variable or a leading slash is not one.
PATHISH = re.compile(r"(?<![\w/@:.-])([\w.-]+/[\w./-]*[\w-]\."
                     r"(?:json|yaml|html|mjs|toml|cast|svg|md|py|sh|js|ts|go|"
                     r"yml|css|txt|gif|png))(?![\w-])")


@rule("OSS-211", WARN, "Every path a manifest names exists",
      "A file copied from a sibling repository passes every other check and "
      "still describes a project that is not this one. The paths it names are "
      "what gives it away. Prose is left to OSS-206, because a document names "
      "illustrative paths on purpose.")
def check_named_paths(repo):
    out = []
    watched = [".gitattributes", ".goreleaser.yaml", ".goreleaser.yml",
               ".github/workflows/release.yml"]
    for path in watched:
        body = repo.read(path)
        if body is None:
            continue
        seen = set()
        for m in PATHISH.finditer(body):
            target = m.group(1)
            if target in seen or "://" in target or "*" in target:
                continue
            seen.add(target)
            if (ROOT / target).exists():
                continue
            out.append(warn("OSS-211", f"names {target}, which is not here",
                            f"{path}:{line_of(body, m.start())}"))
    return out[:12]


# ---------------------------------------------------------------------------
# 3xx — What must not be published
# ---------------------------------------------------------------------------
def _leak_patterns():
    """Patterns that catch the class, never the instance."""
    allow = "|".join(sorted(re.escape(a) for a in CONFIG["HOME_ALLOW"])) or "user"
    domains = RESERVED_DOMAINS | set(CONFIG["EMAIL_ALLOW"])
    mail_allow = "|".join(sorted(re.escape(a) for a in domains))
    tld_allow = "|".join(sorted(RESERVED_TLDS))
    pats = [
        ("OSS-301",
         rf"(?:^|[^A-Za-z0-9_.])/home/(?!(?:{allow})\b)([A-Za-z0-9_-][A-Za-z0-9_.-]*)",
         "a home directory naming a person"),
        ("OSS-301",
         rf"/Users/(?!(?:{allow})\b)([A-Za-z0-9_-][A-Za-z0-9_.-]*)",
         "a macOS home directory naming a person"),
        ("OSS-302",
         rf"/(?:home|Users)/(?!(?:{allow})\b)[A-Za-z0-9_.-]+/"
         rf"\.(?:cache|config|local|ssh|aws|gnupg|kube|docker|npm|claude|codex)\b",
         "an absolute path into a person's own state"),
        ("OSS-303",
         rf"[A-Za-z0-9._%+-]+@(?!(?:{mail_allow})\b)"
         rf"(?!noreply\b)[A-Za-z0-9.-]+\.(?!(?:{tld_allow})\b)[A-Za-z]{{2,}}",
         "a mail address"),
    ]
    # The invoking user, from the environment — never written into this file.
    for name in (os.environ.get("USER"), os.environ.get("LOGNAME")):
        if name and len(name) > 2 and name not in CONFIG["HOME_ALLOW"]:
            pats.append(("OSS-304", rf"\b{re.escape(name)}\b",
                         "the invoking user's own name"))
    # Terms no pattern can infer. Absent by default, gitignored when present, so
    # the list of what you consider private never lands in the repository.
    try:
        for term in (ROOT / ".leakterms").read_text().split("\n"):
            term = term.strip()
            if term and not term.startswith("#"):
                pats.append(("OSS-304", re.escape(term), "a term from .leakterms"))
    except OSError:
        pass
    return [(rid, re.compile(p, re.I if rid == "OSS-304" else 0), what)
            for rid, p, what in pats]


def _scan(repo):
    """One pass over every tracked text file, shared by the 3xx rules."""
    patterns = _leak_patterns()
    out = []
    for path, body in repo.scannable():
        for rid, regex, what in patterns:
            m = regex.search(body)
            if not m:
                continue
            out.append(err(rid, f"{what} — {excerpt(body, m.start())}",
                           f"{path}:{line_of(body, m.start())}"))
            break   # one report per file is enough to act on
    return out


@rule("OSS-301", ERROR, "No tracked file names a person's home directory",
      "A path is the easiest leak to make and the hardest to notice: it "
      "reaches a fixture, then a golden derived from it, then a manifest, and "
      "every copy carries a login name. Placeholder names a shipped image uses "
      "go in HOME_ALLOW, and nothing else does.")
def check_home_paths(repo):
    return [p for p in repo.leaks() if p.rule == "OSS-301"]


@rule("OSS-302", ERROR, "No tracked file points into a person's own state",
      "A cache or config path leaks a login even after the /home prefix has "
      "been rewritten, which is how a browser path once reached a committed "
      "manifest.")
def check_state_paths(repo):
    return [p for p in repo.leaks() if p.rule == "OSS-302"]


@rule("OSS-303", ERROR, "No tracked file carries a mail address",
      "An address in a public repository is scraped within days. The "
      "documentation domains and reserved test TLDs are the exception, and a "
      "fixture that needs an address should use one of them.")
def check_mail(repo):
    return [p for p in repo.leaks() if p.rule == "OSS-303"]


@rule("OSS-304", ERROR, "No tracked file names the person publishing it",
      "The name comes from the environment at run time, so the check works "
      "without the name ever being written down.")
def check_own_name(repo):
    return [p for p in repo.leaks() if p.rule == "OSS-304"]


SECRET_PATTERNS = [
    (r"-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----", "a private key"),
    (r"\bsk-ant-[A-Za-z0-9_-]{16,}", "an Anthropic key"),
    (r"\bsk-(?:proj-)?[A-Za-z0-9]{32,}", "an OpenAI-shaped key"),
    (r"\bgh[pousr]_[A-Za-z0-9]{30,}", "a GitHub token"),
    (r"\bgithub_pat_[A-Za-z0-9_]{50,}", "a GitHub fine-grained token"),
    (r"\bAKIA[0-9A-Z]{16}\b", "an AWS access key id"),
    (r"\bASIA[0-9A-Z]{16}\b", "an AWS session key id"),
    (r"\bAIza[0-9A-Za-z_-]{35}\b", "a Google API key"),
    (r"\bxox[abprs]-[0-9A-Za-z-]{10,}", "a Slack token"),
    (r"\bglpat-[0-9A-Za-z_-]{20,}", "a GitLab token"),
    (r"\bfw_[A-Za-z0-9]{20,}", "a Fireworks key"),
    (r"\bey[A-Za-z0-9_-]{14,}\.ey[A-Za-z0-9_-]{14,}\.[A-Za-z0-9_-]{14,}", "a JWT"),
    (r"(?i)\b(?:api[_-]?key|client[_-]?secret|passwd|password|auth[_-]?token)\b"
     r"\s*[:=]\s*['\"][A-Za-z0-9/+_-]{20,}['\"]", "a literal credential"),
]


# A test needs a credential-shaped string, and the only safe one says so in
# itself. A value that carries one of these is exempt, which makes "spell your
# fixtures obviously fake" the rule the check enforces rather than a rule a
# reviewer has to remember.
FAKE_MARKERS = re.compile(
    r"AAAA|XXXX|0000|example|fixture|sample|dummy|fake|placeholder|redacted|"
    r"not-?a-?real|replace-?me|your-?key|CREDENTIAL|TOKEN|SECRET|test-?only",
    re.I)


@rule("OSS-305", ERROR, "No credential is in a tracked file",
      "A key in a published commit is compromised the moment the repository is "
      "public, and deleting it later does not un-publish it. Rotate it as well "
      "as removing it. A fixture that needs a key-shaped string spells it "
      "obviously fake, and is then exempt.")
def check_secrets(repo):
    out = []
    compiled = [(re.compile(p), what) for p, what in SECRET_PATTERNS]
    for path, body in repo.scannable():
        for regex, what in compiled:
            for m in regex.finditer(body):
                if FAKE_MARKERS.search(m.group(0)):
                    continue
                out.append(err("OSS-305",
                               f"{what} — {excerpt(body, m.start(), 20, 24)}",
                               f"{path}:{line_of(body, m.start())}"))
                break
            else:
                continue
            break
    return out


@rule("OSS-306", ERROR, "Every tracked file can be read",
      "A file nobody can inspect must never be reported as clean. A committed "
      "editor swap file smuggled a username past a scan of this kind exactly "
      "that way.")
def check_readable(repo):
    return [err("OSS-306", "cannot be read as text, so its contents were never "
                           "checked; remove it, or add its extension to "
                           "BINARY_OK if it is a legitimate asset", path)
            for path in repo.unreadable]


CREDENTIAL_FILES = re.compile(
    r"(^|/)(\.env(\..*)?|\.npmrc|\.netrc|id_rsa|id_ed25519|.*\.pem|.*\.p12|"
    r".*\.pfx|.*\.keystore|credentials\.json|service-account.*\.json)$")


@rule("OSS-307", ERROR, "No credential file is tracked, and .env is ignored",
      "A local .env is where a key lands first. Ignoring it is what keeps the "
      "next `git add -A` from publishing it.")
def check_env(repo):
    out = [err("OSS-307", "a credential file is tracked", p)
           for p in repo.tracked if CREDENTIAL_FILES.search(p)]
    ignore = repo.read(".gitignore") or ""
    if not re.search(r"^/?\.env\b", ignore, re.M):
        out.append(err("OSS-307", ".gitignore does not ignore .env"))
    return out


BUILD_OUTPUT = re.compile(r"^(bin|dist|build|out|target|node_modules|"
                          r"__pycache__|\.venv|coverage)/")


@rule("OSS-308", ERROR, "No build output or dependency tree is tracked",
      "A committed artifact is a second copy of what the build makes, and it "
      "goes stale silently. A committed dependency tree also republishes "
      "somebody else's code without their licence.")
def check_build_output(repo):
    out = [err("OSS-308", "build output is tracked", p)
           for p in repo.tracked if BUILD_OUTPUT.match(p)]
    return out[:20]


@rule("OSS-309", WARN, "The ignore file explains itself",
      "An ignore rule with no reason is one the next person deletes, and the "
      "output it was keeping out of the tree comes back.")
def check_gitignore(repo):
    body = repo.read(".gitignore")
    if body is None:
        return [warn("OSS-309", "no .gitignore")]
    if not any(l.strip().startswith("#") for l in body.split("\n")):
        return [warn("OSS-309", ".gitignore carries no comment saying what it "
                                "keeps out, or what it deliberately does not")]
    return []


@rule("OSS-310", WARN, "The checkout is declared LF",
      "Without it a checkout on Windows rewrites every text file to CRLF, and "
      "a shell script then fails with a message naming the interpreter rather "
      "than the script.")
def check_gitattributes(repo):
    body = repo.read(".gitattributes")
    if body is None:
        return [warn("OSS-310", "no .gitattributes declaring the line ending")]
    if not re.search(r"^\*\s+text=auto\s+eol=lf", body, re.M):
        return [warn("OSS-310", ".gitattributes does not declare `* text=auto eol=lf`")]
    return []


# ---------------------------------------------------------------------------
# 4xx — The build, the gate and the release
# ---------------------------------------------------------------------------
@rule("OSS-401", ERROR, "There is a CI workflow",
      "An outside contributor cannot run the host-specific half of the suite. "
      "CI is what tells them their change is sound, and what tells you a merge "
      "is safe.")
def check_ci_exists(repo):
    if repo.workflows():
        return []
    return [err("OSS-401", "no workflow under .github/workflows/")]


@rule("OSS-402", WARN, "CI runs on the default branch, on every pull request, "
                       "and on demand",
      "A workflow that only runs on push never sees a fork's pull request, "
      "which is every outside contribution.")
def check_ci_triggers(repo):
    body = repo.ci()
    if body is None:
        return [skipped("OSS-402", "no ci.yml")]
    return [warn("OSS-402", f"ci.yml has no {trigger} trigger")
            for trigger in ("pull_request", "workflow_dispatch")
            if not re.search(rf"^\s{{2}}{trigger}:", body, re.M)]


@rule("OSS-403", ERROR, "Every workflow declares the permissions it needs",
      "Without a permissions block a workflow gets the repository default, "
      "which on an older repository is write access to everything. A public "
      "repository runs workflow code proposed by strangers.")
def check_ci_permissions(repo):
    return [err("OSS-403", "no top-level permissions block", path)
            for path, body in repo.workflows().items()
            if not re.search(r"^permissions:", body, re.M)]


ACTION_REF = re.compile(r"^\s*(?:-\s*)?uses:\s*([^\s#]+)", re.M)


@rule("OSS-404", ERROR, "Every action is pinned to a version",
      "`@main` means the workflow runs whatever that repository holds today. "
      "The build that passed yesterday is not the build that runs now.")
def check_action_pins(repo):
    out = []
    for path, body in repo.workflows().items():
        for m in ACTION_REF.finditer(body):
            ref = m.group(1)
            if ref.startswith("./") or ref.startswith("docker://"):
                continue
            _, _, version = ref.partition("@")
            if not version:
                out.append(err("OSS-404", f"{ref} has no version", path))
            elif not re.fullmatch(r"v?\d[\w.+-]*|[0-9a-f]{40}", version):
                out.append(err("OSS-404", f"{ref} is not pinned to a version", path))
    return out


@rule("OSS-405", ERROR, "CI runs the same gate a contributor runs",
      "Two lists of checks drift, and the one that drifts is always the one "
      "nobody runs locally.")
def check_ci_runs_check(repo):
    bodies = "\n".join(repo.workflows().values())
    if not bodies:
        return [skipped("OSS-405", "no workflows")]
    if re.search(r"\bmake check\b|\bnpm (run )?check\b|scripts/check", bodies):
        return []
    return [err("OSS-405", "no CI job runs the project's own check target")]


@rule("OSS-406", ERROR, "Each linter has its own CI job",
      "A linter needs no toolchain and answers in seconds, so on its own job it "
      "reports even when the tests are failing. Buried behind a test matrix it "
      "reports nothing. The readiness job also needs the whole history, which "
      "the default shallow checkout does not fetch.")
def check_linter_jobs(repo):
    bodies = "\n".join(repo.workflows().values())
    if not bodies:
        return [skipped("OSS-406", "no workflows")]
    out = []
    if not (re.search(r"^\s{2}docs:", bodies, re.M)
            and re.search(r"make docs|lint-docs", bodies)):
        out.append(err("OSS-406", "no docs job runs the prose linter"))
    if not (re.search(r"^\s{2}oss:", bodies, re.M)
            and re.search(r"make oss|lint-oss", bodies)):
        out.append(err("OSS-406", "no oss job runs the readiness linter"))
    elif "fetch-depth: 0" not in bodies:
        out.append(warn("OSS-406", "the oss job takes a shallow checkout, so the "
                                   "history rules see nothing and pass"))
    return out


@rule("OSS-407", ERROR, "A tagged release builds the artifacts",
      "The first thing a reader does is look for a binary. A releases page "
      "with nothing on it is the install path every new user takes.")
def check_release_workflow(repo):
    body = repo.read(".github/workflows/release.yml") or \
        repo.read(".github/workflows/release.yaml")
    if body is None:
        return [err("OSS-407", "no release workflow under .github/workflows/")]
    if "tags:" not in body:
        return [err("OSS-407", "the release workflow is not triggered by a tag")]
    if not re.search(r"contents:\s*write", body):
        return [err("OSS-407", "the release workflow cannot create a release "
                               "(no contents: write)")]
    return []


@rule("OSS-408", ERROR, "The release manifest validates",
      "A manifest that names a document the rework deleted breaks the release "
      "build outright, and schema validation of the manifest does not catch "
      "it.")
def check_goreleaser(repo):
    if repo.goreleaser() is None:
        return [skipped("OSS-408", "no goreleaser manifest")]
    if not shutil.which("goreleaser"):
        return [skipped("OSS-408", "goreleaser is not installed")]
    run = subprocess.run(["goreleaser", "check"], cwd=ROOT,
                         capture_output=True, text=True)
    if run.returncode != 0:
        tail = (run.stderr or run.stdout).strip().split("\n")[-1][:200]
        return [err("OSS-408", f"goreleaser check failed: {tail}")]
    return []


@rule("OSS-409", WARN, "The release is reproducible and verifiable",
      "A downloaded binary is trusted on the strength of what shipped beside "
      "it: a checksum, a signature over that checksum, and a bill of "
      "materials.")
def check_release_supply_chain(repo):
    body = repo.goreleaser()
    if body is None:
        return [skipped("OSS-409", "no goreleaser manifest")]
    out = []
    for pattern, what in (
            (r"CGO_ENABLED=0", "the binary is not built static (CGO_ENABLED=0)"),
            (r"-trimpath", "the build does not pass -trimpath, so paths from "
                           "the build machine reach the binary"),
            (r"mod_timestamp", "no mod_timestamp, so the archive is not reproducible"),
            (r"^checksum:", "no checksum block"),
            (r"^sboms:", "no SBOM block"),
            (r"^signs:", "no signature block")):
        if not re.search(pattern, body, re.M):
            out.append(warn("OSS-409", what))
    return out


TARGET = re.compile(r"^([a-zA-Z][\w.-]*):", re.M)


@rule("OSS-410", ERROR, "The task runner carries the family's vocabulary",
      "Someone who has worked on one of these repositories knows the targets. "
      "Renaming them costs that knowledge for no gain.")
def check_targets(repo):
    body = repo.makefile()
    if not body:
        return [skipped("OSS-410", "no Makefile")]
    have = set(TARGET.findall(body))
    out = []
    missing = [t for t in CONFIG["REQUIRED_TARGETS"] if t not in have]
    if missing:
        out.append(err("OSS-410", "the Makefile has no " + ", ".join(missing) + " target"))
    absent = [t for t in CONFIG["EXPECTED_TARGETS"] if t not in have]
    if absent:
        out.append(warn("OSS-410", "the Makefile has no " + ", ".join(absent) + " target"))
    if "help" in have and ".DEFAULT_GOAL" not in body:
        out.append(warn("OSS-410", "make with no argument does not print the help menu"))
    return out


def check_target(body):
    """The check target's prerequisites and its recipe, or (None, None)."""
    # [^\S\n] rather than \s: the latter crosses the newline and swallows the
    # tab-indented recipe, so a delegating target reads as its own prerequisite.
    m = re.search(r"^check:[^\S\n]*(.*)$", body, re.M)
    if not m:
        return None, None
    recipe = ""
    for line in body[m.end():].lstrip("\n").split("\n"):
        if line and not line[0].isspace():
            break
        recipe += line + "\n"
    return m.group(1).split(), recipe


def check_reaches(repo, prereqs, recipe, target, needle):
    """Whether `check` runs target, directly or through a delegating script."""
    if target in prereqs or needle in recipe:
        return True
    # A project that routes its gates through one script satisfies this by
    # calling the tool there instead.
    script = re.search(r"([\w./-]*check\.sh)", recipe)
    if script:
        delegated = repo.read(script.group(1).lstrip("./")) or ""
        if needle in delegated or f"make {target}" in delegated:
            return True
    return False


@rule("OSS-411", ERROR, "The check target reaches both linters",
      "The one command a contributor runs before pushing is the contract "
      "between them and CI. A gate it does not reach is one they meet after "
      "the fact, in a red build.")
def check_check_covers_docs(repo):
    body = repo.makefile()
    if not body:
        return [skipped("OSS-411", "no Makefile")]
    prereqs, recipe = check_target(body)
    if prereqs is None:
        return [err("OSS-411", "the Makefile has no check target")]
    return [err("OSS-411", f"check does not reach {linter}")
            for target, linter in (("docs", "lint-docs"), ("oss", "lint-oss"))
            if not check_reaches(repo, prereqs, recipe, target, linter)]


@rule("OSS-412", WARN, "The toolchain floor is declared once",
      "A version pinned in the build file and again in CI drifts, and CI is "
      "the copy that wins while everyone reads the other one.")
def check_toolchain(repo):
    bodies = "\n".join(repo.workflows().values())
    if not bodies or not repo.has("go.mod"):
        return [skipped("OSS-412", "not a Go project with workflows")]
    if "setup-go" not in bodies:
        return [skipped("OSS-412", "CI does not set up Go")]
    out = []
    if "go-version-file" not in bodies:
        out.append(warn("OSS-412", "CI pins a Go version of its own instead of "
                                   "reading go.mod"))
    declared = re.search(r"^go\s+(\d+)\.(\d+)", repo.read("go.mod") or "", re.M)
    install = repo.read("INSTALL.md") or ""
    claimed = re.search(r"Go\s+(\d+)\.(\d+)\+", install)
    if declared and claimed and declared.groups() != claimed.groups():
        out.append(warn("OSS-412", f"go.mod declares Go {declared.group(0)[3:]} "
                                   f"and INSTALL.md claims "
                                   f"{claimed.group(1)}.{claimed.group(2)}+"))
    return out


@rule("OSS-413", WARN, "A release exists to download",
      "INSTALL.md tells the reader to grab an archive. Until a version tag has "
      "been pushed, the first install path every new user takes points at an "
      "empty page.")
def check_tags(repo):
    install = repo.read("INSTALL.md") or ""
    if "release" not in install.lower():
        return [skipped("OSS-413", "INSTALL.md offers no release download")]
    tags = (git("tag", "--list", "v*") or "").split()
    if not tags:
        return [warn("OSS-413", "no v* tag has been pushed, so the releases "
                                "page INSTALL.md points at is empty")]
    return []


@rule("OSS-414", WARN, "No gate needs a secret an outside contributor lacks",
      "A pull request from a fork gets no secrets. A job that needs one fails "
      "for every contributor, and the failure looks like their change.")
def check_secrets_in_ci(repo):
    out = []
    for path, body in repo.workflows().items():
        if "release" in path:
            continue
        for m in re.finditer(r"secrets\.([A-Z_][A-Z0-9_]*)", body):
            if m.group(1) == "GITHUB_TOKEN":
                continue
            out.append(warn("OSS-414", f"a job reads secrets.{m.group(1)}",
                            f"{path}:{line_of(body, m.start())}"))
    return out[:10]


@rule("OSS-415", WARN, "The workflows themselves lint clean",
      "A workflow error surfaces as a run that never starts, and the message "
      "GitHub shows for it names a line rather than a cause.")
def check_actionlint(repo):
    if not repo.workflows():
        return [skipped("OSS-415", "no workflows")]
    if not shutil.which("actionlint"):
        return [skipped("OSS-415", "actionlint is not installed")]
    run = subprocess.run(["actionlint"], cwd=ROOT, capture_output=True, text=True)
    if run.returncode != 0:
        first = (run.stdout or run.stderr).strip().split("\n")[0][:200]
        return [warn("OSS-415", f"actionlint reports problems: {first}")]
    return []


@rule("OSS-416", ERROR, "No tag fires the release workflow by accident",
      "The release workflow triggers on a tag glob. A tag that matches the "
      "glob but is not a version cuts a release named after it, or poisons the "
      "version stamp `git describe` produces.")
def check_tag_shapes(repo):
    body = repo.read(".github/workflows/release.yml") or \
        repo.read(".github/workflows/release.yaml")
    if body is None:
        return [skipped("OSS-416", "no release workflow")]
    if not re.search(r'tags:.*\bv\*', body):
        return [skipped("OSS-416", "the release trigger is not a v* glob")]
    tags = (git("tag", "--list", "v*") or "").split()
    bad = [t for t in tags if not re.fullmatch(r"v\d+\.\d+\.\d+[\w.+-]*", t)]
    if bad:
        return [err("OSS-416", "tags that match the release trigger but are not "
                               "versions: " + ", ".join(bad[:6]))]
    return []


@rule("OSS-417", ERROR, "The language's own static analysis is in the gate",
      "gofmt and go vet catch what will not compile or is plainly wrong. They "
      "say nothing about a stdlib call written out by hand, a parameter "
      "nothing reads, or a helper that already exists under another name — the "
      "residue that accumulates fastest in generated code, and the reason a "
      "reviewer's attention goes on mechanics instead of design. A `lint` "
      "target nobody runs is the same as no target: it has to hang off `check` "
      "and off CI, or it reports only to whoever remembers it exists.")
def check_language_lint(repo):
    if not repo.has("go.mod"):
        return [skipped("OSS-417", "no go.mod; this rule is Go-specific")]
    out = []
    cfgs = [".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json"]
    cfg = next((c for c in cfgs if repo.has(c)), None)
    if not cfg:
        out.append(err("OSS-417", "no .golangci.yml, so golangci-lint runs its "
                                  "default set and every linter that finds this "
                                  "residue is off"))
    body = repo.makefile()
    if body:
        prereqs, recipe = check_target(body)
        if prereqs is None:
            out.append(err("OSS-417", "the Makefile has no check target"))
        elif not check_reaches(repo, prereqs, recipe, "lint", "golangci-lint"):
            out.append(err("OSS-417", "check does not reach golangci-lint"))
        if "deadcode" not in body:
            # `unused` is package-scoped: a function whose only caller lives in
            # another package looks used to it, and an exported one nothing
            # calls is invisible. Whole-program reachability is a separate tool.
            out.append(warn("OSS-417", "no deadcode target; golangci-lint's "
                                       "`unused` cannot see across packages"))
    bodies = "\n".join(repo.workflows().values())
    if bodies and "golangci-lint" not in bodies and "make check" not in bodies:
        out.append(err("OSS-417", "no CI job runs golangci-lint"))
    return out


# ---------------------------------------------------------------------------
# 5xx — What a stranger's clone can do
# ---------------------------------------------------------------------------
LOCAL_DEP = [
    (r'"[^"]*"\s*:\s*"file:\.\.[^"]*"',
     "an npm dependency resolved from a path outside the repository"),
    (r'"[^"]*"\s*:\s*"link:\.\.[^"]*"',
     "an npm dependency linked from outside the repository"),
    (r"^\s*replace\s+\S+\s+=>\s+\.\.",
     "a Go module replaced by a path outside the repository"),
    (r'path\s*=\s*"\.\.',
     "a Cargo dependency resolved from a path outside the repository"),
    (r"-e\s+\.\./",
     "a Python dependency installed from a path outside the repository"),
]


# The tail of a `../`-prefixed path, used the same way OSS-502 uses it: a
# dependency that climbs out of its directory and back into this tree is in the
# tree, whatever the path looks like.
CLIMBS_BACK = re.compile(r"(?:\.\./)+([\w.-]+(?:/[\w.-]+)*)")


@rule("OSS-501", ERROR, "Nothing resolves from outside the repository",
      "A dependency read from a sibling directory builds on the machine it was "
      "written on and nowhere else. A stranger's clone is one directory, and "
      "the failure names a path they have never seen.")
def check_local_deps(repo):
    out = []
    compiled = [(re.compile(p, re.M), what) for p, what in LOCAL_DEP]
    for path, body in repo.scannable():
        if not path.endswith((".json", ".mod", ".toml", ".txt", ".yaml", ".yml")):
            continue
        if "lock" in path:
            continue
        for regex, what in compiled:
            for m in regex.finditer(body):
                tail = CLIMBS_BACK.search(m.group(0))
                if tail and (ROOT / tail.group(1)).exists():
                    continue
                out.append(err("OSS-501", f"{what} — {excerpt(body, m.start(), 10, 70)}",
                               f"{path}:{line_of(body, m.start())}"))
    return out[:20]


SIBLING = re.compile(r"(?<![\w./$({])((?:\.\./)+)([\w.-]+(?:/[\w.-]+)*)/?")


def _escapes_the_repo(climb, tail):
    """Whether a `../`-prefixed path leaves the repository.

    Judged by the tail rather than by the base, because the base a comment or a
    script means is not always the file it sits in. A tail that names something
    in the tree is a path back into it, however many levels it climbs first;
    `../../vendor/thing` from apps/viewer is this repository, and
    `../../thing` is not.
    """
    return not (ROOT / tail).exists()


@rule("OSS-502", ERROR, "No gate needs a checkout beside this one",
      "A build that requires a second repository as a sibling fails for "
      "reasons unrelated to the change, and the message names a directory the "
      "reader does not have.")
def check_sibling_checkout(repo):
    out = []
    for path, body in repo.scannable():
        if not (path.startswith("scripts/") or path in ("Makefile", "makefile")
                or path.startswith(".github/workflows/")):
            continue
        for m in SIBLING.finditer(body):
            if not _escapes_the_repo(m.group(1), m.group(2)):
                continue
            out.append(err("OSS-502", f"a path outside the repository — "
                                      f"{excerpt(body, m.start(), 30, 60)}",
                           f"{path}:{line_of(body, m.start())}"))
            break
    return out


@rule("OSS-503", ERROR, "The module path is the published one",
      "A module whose declared path is not where it lives cannot be installed "
      "with `go install`, and the error names a repository that does not "
      "exist.")
def check_module_path(repo):
    gomod = repo.read("go.mod")
    slug = repo.slug()
    if gomod is None or not slug:
        return [skipped("OSS-503", "no go.mod, or no origin remote")]
    m = re.search(r"^module\s+(\S+)", gomod, re.M)
    if not m:
        return [err("OSS-503", "go.mod declares no module path")]
    if not m.group(1).lower().endswith(slug.lower()):
        return [err("OSS-503", f"go.mod declares {m.group(1)}, and the remote "
                               f"is {slug}")]
    return []


@rule("OSS-504", ERROR, "The prose linter is vendored and wired in",
      "Every other artifact in the repository has a gate. Prose has none "
      "unless this one is here, and documentation is what a published "
      "repository is read through.")
def check_lint_docs(repo):
    if not repo.has("scripts/lint-docs.py"):
        return [err("OSS-504", "scripts/lint-docs.py is not vendored into this repo")]
    out = []
    body = repo.read("scripts/lint-docs.py") or ""
    if "vendored from" not in body:
        out.append(warn("OSS-504", "scripts/lint-docs.py has lost its provenance note"))
    config = repo.read("scripts/lint-docs.config.py")
    if config is None:
        out.append(err("OSS-504", "scripts/lint-docs.config.py is missing"))
    elif re.search(r"^GLOSSARY\s*=\s*\[\s*\]", config, re.M):
        out.append(err("OSS-504", "GLOSSARY is empty, which disables the most "
                                  "valuable prose check"))
    run = subprocess.run([sys.executable, "scripts/lint-docs.py"], cwd=ROOT,
                         capture_output=True, text=True)
    if run.returncode != 0:
        tail = (run.stdout or run.stderr).strip().split("\n")[-1][:160]
        out.append(err("OSS-504", f"the prose linter reports problems: {tail}"))
    return out


# ---------------------------------------------------------------------------
# 6xx — The ledger
# ---------------------------------------------------------------------------
@rule("OSS-601", ERROR, "A tracked ledger validates and its page is current",
      "The rendered page is what a human reads. A record changed without a "
      "re-render publishes a page that disagrees with the records beside it.")
def check_ledger(repo):
    if not repo.keeps_ledger():
        return [skipped("OSS-601", "this repository keeps no ledger")]
    if not shutil.which("cs-ledger"):
        return [skipped("OSS-601", "cs-ledger is not installed")]
    run = subprocess.run(["cs-ledger", "check", "ledger"], cwd=ROOT,
                         capture_output=True, text=True)
    if run.returncode != 0:
        tail = (run.stdout or run.stderr).strip().split("\n")[-1][:200]
        return [err("OSS-601", f"cs-ledger check failed: {tail}")]
    return []


@rule("OSS-602", ERROR, "The ledger carries its own router and guide",
      "An agent that finds records with no doctrine beside them invents a "
      "practice, and the ledger fills with records nobody can close.")
def check_ledger_docs(repo):
    if not repo.keeps_ledger():
        return [skipped("OSS-602", "this repository keeps no ledger")]
    out = [err("OSS-602", f"ledger/{name} is missing")
           for name in ("AGENTS.md", "GUIDE.md") if not repo.has(f"ledger/{name}")]
    agents = repo.read("ledger/AGENTS.md") or ""
    if agents and len(agents.splitlines()) > 30:
        out.append(warn("OSS-602", "ledger/AGENTS.md is not the short router the "
                                   "current cs-ledger writes; re-render it"))
    return out


@rule("OSS-603", ERROR, "The repository points at its own ledger",
      "A ledger nobody is routed to is a ledger nobody files into.")
def check_ledger_routing(repo):
    if not repo.keeps_ledger():
        return [skipped("OSS-603", "this repository keeps no ledger")]
    return [err("OSS-603", f"{doc} never mentions the ledger")
            for doc in ("AGENTS.md", "CONTRIBUTING.md")
            if "ledger" not in (repo.read(doc) or "").lower()]


@rule("OSS-604", WARN, "CI gates the ledger",
      "CONTRIBUTING asks for a render and a check before every commit that "
      "touches the ledger. Nothing enforces it until CI does.")
def check_ledger_ci(repo):
    if not repo.keeps_ledger():
        return [skipped("OSS-604", "this repository keeps no ledger")]
    bodies = "\n".join(repo.workflows().values())
    if "cs-ledger" in bodies or "ledger check" in bodies:
        return []
    return [warn("OSS-604", "no CI job runs cs-ledger check")]


# ---------------------------------------------------------------------------
# 7xx — The history, which ships with the code
# ---------------------------------------------------------------------------
def _history_severity():
    """Published history cannot be rewritten, so the rule becomes advice."""
    return WARN if CONFIG["PUBLISHED"] else ERROR


SESSION_TRAILER = re.compile(
    r"^(?:Claude-Session|Session|Codex-Session|Transcript|Agent-Session):|"
    r"claude\.ai/code/session|chatgpt\.com/codex/|Generated with \[",
    re.M | re.I)


@rule("OSS-701", ERROR, "No commit message links an agent session",
      "A session link is private to whoever ran it and dead to everyone else. "
      "It is also the one part of a commit message that cannot be fixed after "
      "the repository is public.")
def check_session_trailers(repo):
    log = git("log", "--format=%H%x00%B%x00%x00")
    if log is None:
        return [skipped("OSS-701", "no git history")]
    hits = []
    for entry in log.split("\0\0"):
        if "\0" not in entry:
            continue
        sha, _, message = entry.partition("\0")
        if SESSION_TRAILER.search(message):
            hits.append(sha.strip()[:9])
    if not hits:
        return []
    out = [Problem("OSS-701", _history_severity(),
                   f"{len(hits)} commit message(s) link an agent session; the "
                   f"newest is {hits[0]}", hits[0])]
    return out


@rule("OSS-702", WARN, "Commit subjects read the way CONTRIBUTING says",
      "The history is published too, and it is the first thing a reader who "
      "wants to trust the project scrolls through.")
def check_subjects(repo):
    log = git("log", "--format=%h %s")
    if log is None:
        return [skipped("OSS-702", "no git history")]
    lines = [l for l in log.split("\n") if l.strip()]
    bad_case, too_long, trailing = [], [], []
    for line in lines:
        sha, _, subject = line.partition(" ")
        if not subject:
            continue
        if subject[0].islower():
            bad_case.append(sha)
        if len(subject) > 60:
            too_long.append(sha)
        if subject.endswith("."):
            trailing.append(sha)
    out = []
    for label, hits in (("open in lower case", bad_case),
                        ("are over 60 characters", too_long),
                        ("end with a full stop", trailing)):
        if hits:
            out.append(warn("OSS-702", f"{len(hits)} of {len(lines)} commit "
                                       f"subjects {label}", hits[0]))
    return out


# A name that says "this is the history I removed". Not anchored: the branch
# that held one project's de-shipped design document was called
# go-port-backup-blueprint, and a leading-anchor pattern walked past it.
PRIVATE_BRANCH = re.compile(
    r"(?:^|[-_/])(?:backup|bak|wip|old|tmp|temp|scratch|orig|snapshot)"
    r"(?:[-_/]|\d|$)|^pre-", re.I)


@rule("OSS-703", WARN, "No branch was kept as a private backup",
      "A branch named for a rewrite that has already happened carries the "
      "history the rewrite removed. Publishing the repository publishes it.")
def check_branches(repo):
    listing = git("branch", "--format=%(refname:short)")
    if listing is None:
        return [skipped("OSS-703", "no git history")]
    suspect = [b.strip() for b in listing.split("\n")
               if b.strip() and PRIVATE_BRANCH.search(b.strip())]
    if suspect:
        return [warn("OSS-703", "local branches that must not be pushed: "
                     + ", ".join(suspect[:8]))]
    return []


@rule("OSS-704", WARN, "Every address the history publishes is meant to be public",
      "A Co-Authored-By trailer publishes an address. A machine identity is "
      "fine; a person's is theirs to publish, not yours.")
def check_history_mail(repo):
    log = git("log", "--format=%B")
    if log is None:
        return [skipped("OSS-704", "no git history")]
    domains = RESERVED_DOMAINS | set(CONFIG["EMAIL_ALLOW"])
    allow = "|".join(sorted(re.escape(a) for a in domains))
    pat = re.compile(rf"[A-Za-z0-9._%+-]+@(?!(?:{allow})\b)"
                     rf"(?!noreply\b)[A-Za-z0-9.-]+\.[A-Za-z]{{2,}}")
    found = sorted({a for a in pat.findall(log) if not a.startswith("noreply@")})
    if found:
        return [warn("OSS-704", "the history publishes these addresses; confirm "
                                "each is meant to be public: " + ", ".join(found[:6]))]
    return []


@rule("OSS-705", WARN, "The working tree is clean",
      "A check run against a dirty tree reports on something that is not what "
      "would be published.")
def check_clean_tree(repo):
    status = git("status", "--porcelain")
    if status is None:
        return [skipped("OSS-705", "no git repository")]
    dirty = [l for l in status.split("\n") if l.strip()]
    if dirty:
        return [warn("OSS-705", f"{len(dirty)} uncommitted change(s); the first "
                                f"is {dirty[0].strip()}")]
    return []


@rule("OSS-706", ERROR, "No rewrite left its backup behind",
      "`git filter-branch` saves the history it rewrote under refs/original. "
      "That ref still reaches every commit the rewrite was meant to remove, "
      "and `git push --mirror` publishes it.")
def check_original_refs(repo):
    listing = git("for-each-ref", "--format=%(refname)", "refs/original/")
    if listing is None:
        return [skipped("OSS-706", "no git repository")]
    refs = [r.strip() for r in listing.split("\n") if r.strip()]
    if refs:
        return [err("OSS-706", "a filter-branch backup survives: "
                    + ", ".join(refs[:4])
                    + " — delete it with `git update-ref -d`, then expire the "
                      "reflog and gc")]
    return []


# The high-signal half of the leak patterns, in the syntax `git log -G` speaks.
# A candidate commit is then re-read with the full pattern, because the ERE
# here cannot say "any name except the placeholder".
HISTORY_PROBES = [
    # Any home directory, not only one leading into a dot-directory. Requiring
    # the dot was a real hole: a test constant naming a developer's own home,
    # with an ordinary path after it, sat in eighty commits and every history
    # probe passed it. The name is checked against HOME_ALLOW afterwards,
    # because `git log -G` speaks POSIX regular expressions and cannot say
    # "any name except these".
    (r"home/[A-Za-z0-9_.-]+/",
     re.compile(r"(?:^|[^A-Za-z0-9_.])/?(?:home|Users)/"
                r"([A-Za-z0-9_-][A-Za-z0-9_.-]*)/", re.M),
     "a home directory naming a person"),
    (r"home/[A-Za-z0-9_.-]+/\.(cache|config|local|ssh|aws|gnupg)",
     re.compile(r"(?:home|Users)/([A-Za-z0-9_.-]+)/\."
                r"(?:cache|config|local|ssh|aws|gnupg)"),
     "a path into a person's own state"),
    (r"-----BEGIN [A-Z ]*PRIVATE KEY-----",
     re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"), "a private key"),
    (r"sk-ant-[A-Za-z0-9_-]{16,}",
     re.compile(r"sk-ant-[A-Za-z0-9_-]{16,}"), "an Anthropic key"),
    (r"gh[pousr]_[A-Za-z0-9]{30,}",
     re.compile(r"gh[pousr]_[A-Za-z0-9]{30,}"), "a GitHub token"),
    (r"AKIA[0-9A-Z]{16}",
     re.compile(r"AKIA[0-9A-Z]{16}"), "an AWS access key id"),
]


def _history_hit(match):
    """Whether a match found in a past diff is a real leak.

    A capturing group means the pattern needs the name it caught checked
    against the placeholders this project ships. Everything else is judged by
    whether the value says of itself that it is a fixture.
    """
    if match.groups():
        return match.group(1) not in CONFIG["HOME_ALLOW"]
    return not FAKE_MARKERS.search(match.group(0))


@rule("OSS-708", ERROR, "No commit in the history carries a leak",
      "Publishing a repository publishes every commit in it. A path or a key "
      "that was removed later is still in the blob the earlier commit points "
      "at, and a clone taken on the first day keeps it. This reads the text of "
      "each diff, so a leak inside a binary blob that was later deleted — a "
      "compiled bytecode file, an editor swap file — is past it: for those, "
      "check what `git log --diff-filter=A --name-only --all` ever added.")
def check_history_content(repo):
    if git("rev-parse", "HEAD") is None:
        return [skipped("OSS-708", "no git history")]
    out = []
    probes = list(HISTORY_PROBES)
    names = {n for n in (os.environ.get("USER"), os.environ.get("LOGNAME"))
             if n and len(n) > 2 and n not in CONFIG["HOME_ALLOW"]}
    for name in sorted(names):
        probes.append((re.escape(name), None, "the invoking user's own name"))
    for probe, confirm, what in probes:
        found = git("log", "--all", "--format=%h", "-G" + probe)
        if not found:
            continue
        shas = [s for s in found.split() if s][:80]
        hits = []
        for sha in shas:
            if confirm is None:
                hits.append(sha)
                continue
            diff = git("show", "--format=", sha) or ""
            if any(_history_hit(m) for m in confirm.finditer(diff)):
                hits.append(sha)
        if hits:
            out.append(Problem("OSS-708", _history_severity(),
                               f"{what} is in {len(hits)} commit(s), the newest "
                               f"{hits[0]}; rewriting is possible now and not "
                               f"after publication", hits[0]))
    return out


# ---------------------------------------------------------------------------
# 8xx — The repository as GitHub shows it (--online)
# ---------------------------------------------------------------------------
def _gh_view(repo):
    if not shutil.which("gh"):
        return None, "gh is not installed"
    slug = repo.slug()
    if not slug:
        return None, "no origin remote to ask about"
    run = subprocess.run(
        ["gh", "repo", "view", slug, "--json",
         "description,licenseInfo,visibility,defaultBranchRef,repositoryTopics,"
         "hasIssuesEnabled"],
        capture_output=True, text=True)
    if run.returncode != 0:
        return None, (run.stderr or "gh repo view failed").strip().split("\n")[-1][:120]
    try:
        return json.loads(run.stdout), None
    except ValueError:
        return None, "gh returned something that is not JSON"


@rule("OSS-801", ERROR, "The repository has a description and a detected licence",
      "The description is what a search result and every list of repositories "
      "shows. An empty one makes the project look abandoned before it is read.")
def check_repo_metadata(repo, online=False):
    if not online:
        return [skipped("OSS-801", "run with --online to ask GitHub")]
    data, why = _gh_view(repo)
    if data is None:
        return [skipped("OSS-801", why)]
    out = []
    if not (data.get("description") or "").strip():
        out.append(err("OSS-801", "the repository has no description"))
    if not data.get("licenseInfo"):
        out.append(err("OSS-801", "GitHub does not detect a licence"))
    if not data.get("hasIssuesEnabled"):
        out.append(warn("OSS-801", "issues are disabled, so nobody can report a bug"))
    branch = (data.get("defaultBranchRef") or {}).get("name")
    if branch and branch != "main":
        out.append(warn("OSS-801", f"the default branch is {branch}"))
    if not (data.get("repositoryTopics") or []):
        out.append(warn("OSS-801", "no topics, so the repository appears in no "
                                   "topic listing"))
    # Offered on a public repository only, so this is a publication step rather
    # than something that could have been done earlier.
    if data.get("visibility") == "PUBLIC" and shutil.which("gh"):
        run = subprocess.run(
            ["gh", "api", f"repos/{repo.slug()}/private-vulnerability-reporting"],
            capture_output=True, text=True)
        if run.returncode == 0 and '"enabled":false' in run.stdout.replace(" ", ""):
            out.append(err("OSS-801", "private vulnerability reporting is off, and "
                                      "CONTRIBUTING.md points a reporter at it — "
                                      "`gh api -X PUT repos/<owner>/<repo>/"
                                      "private-vulnerability-reporting`"))
    return out


@rule("OSS-802", ERROR, "No private ref was pushed",
      "What a clone can reach is the set of refs on the remote, and every ref "
      "drags its whole history with it. A backup branch publishes exactly what "
      "a rewrite removed, and a leftover tag pins the old history whatever "
      "`main` is squashed to. A force-push touches neither.")
def check_remote_refs(repo, online=False):
    if not online:
        return [skipped("OSS-802", "run with --online to ask the remote")]
    listing = git("ls-remote", "--heads", "--tags", "origin")
    if listing is None:
        return [skipped("OSS-802", "cannot reach the remote")]
    out = []
    branches = [l.split("refs/heads/")[-1] for l in listing.split("\n")
                if "refs/heads/" in l]
    suspect = [b for b in branches if PRIVATE_BRANCH.search(b)]
    others = [b for b in branches if b != "main" and b not in suspect]
    if others:
        out.append(warn("OSS-802", f"{len(others)} branch(es) on the remote "
                        f"besides main, each published with the repository: "
                        + ", ".join(others[:8])))
    if suspect:
        out.append(err("OSS-802", "branches on the remote that must be deleted: "
                       + ", ".join(suspect[:8])
                       + " — `git push origin --delete <branch>`"))
    tags = {l.split("refs/tags/")[-1].removesuffix("^{}")
            for l in listing.split("\n") if "refs/tags/" in l}
    stale = sorted(t for t in tags
                   if not re.fullmatch(r"v?\d+\.\d+\.\d+[\w.+-]*", t))
    if stale:
        out.append(err("OSS-802", "tags on the remote that are not versions, "
                       "each pinning the history behind it: "
                       + ", ".join(stale[:6])
                       + " — `git push origin :refs/tags/<tag>`"))
    return out


@rule("OSS-803", WARN, "Only the publishing remote is configured",
      "A second remote pointing at a personal machine is how a push meant for "
      "GitHub lands somewhere else, and its name is a fact about your network.")
def check_remotes(repo, online=False):
    listing = git("remote")
    if listing is None:
        return [skipped("OSS-803", "no git repository")]
    remotes = [r.strip() for r in listing.split("\n") if r.strip()]
    extra = [r for r in remotes if r != "origin"]
    if extra:
        return [warn("OSS-803", "remotes other than origin: " + ", ".join(extra))]
    return []


# ---------------------------------------------------------------------------
# The review pack — what a regex cannot decide
# ---------------------------------------------------------------------------
REVIEWS = [
    {
        "id": "REV-01",
        "title": "Material that should not become public",
        "evidence": ["git ls-files", "git log --format='%h %s%n%b'"],
        "ask": """Read every tracked file and the whole commit history of this
repository. You are looking for material that is fine internally and wrong in
public: a customer or employer named without their agreement, an unreleased
plan or roadmap, an internal system, dashboard or ticket URL, a machine on a
private network, a colleague's name or handle, revenue or headcount, a
screenshot showing a private screen, a document written for an audience inside
one company.

The mechanical scans have already covered home directories, mail addresses and
credentials. Do not repeat them. Report only what needs a person to recognise
it.

For each finding give the file and line, quote it, and say what it reveals.""",
    },
    {
        "id": "REV-02",
        "title": "A stranger's first clone",
        "evidence": ["cat README.md INSTALL.md", "cat Makefile",
                     "ls .github/workflows"],
        "ask": """Take the position of somebody who has just cloned this
repository and has none of the author's machine. Work through INSTALL.md and
the build instructions literally, and report every step that would fail or that
assumes something undeclared: a tool that is never named, a sibling checkout,
an environment variable with no default, a service that must already be
running, an operating system the docs never mention, a version floor stated
nowhere.

Say for each one whether it is a documentation fix or a code fix.""",
    },
    {
        "id": "REV-03",
        "title": "The first minute",
        "evidence": ["head -60 README.md"],
        "ask": """Read only the first screen of README.md, as a stranger who
arrived from a search result would. Answer four questions and quote the line
that answered each: what is this, who is it for, what problem does it solve,
and what is the shortest path to seeing it work. Where the README does not
answer one, say so and propose the sentence that would.

Then judge the claim in the tagline. Is it true of what this repository
actually does, and would somebody who used the software agree with it?""",
    },
    {
        "id": "REV-04",
        "title": "What the software does to the person running it",
        "evidence": ["cat SPEC.md", "cat MANUAL.md"],
        "ask": """Threat-model the published artifact, not the repository. What
does this software touch on the machine that runs it: credentials, network, the
filesystem outside its own directory, other processes, a container engine, a
kernel feature. For each one, say whether the documentation tells the user
before they run it.

Report anything that would surprise a careful user, anything that runs with
more privilege than it explains, and anything that sends data anywhere. A
public repository is read by people looking for exactly this.""",
    },
    {
        "id": "REV-05",
        "title": "Attribution and borrowed work",
        "evidence": ["git ls-files", "cat go.mod package.json 2>/dev/null"],
        "ask": """Find everything in this repository that came from somewhere
else: vendored source, a file copied from another project, an algorithm
transcribed from a paper or a blog post, generated code, a fixture captured
from a third-party service, an image or a font.

For each, say where it came from, what licence it carries, and whether this
repository's licence and its attribution satisfy that licence. Report anything
whose provenance you cannot establish, because that is the case that has to be
resolved before publication rather than after.""",
    },
    {
        "id": "REV-06",
        "title": "Consistency with the sibling projects",
        "evidence": ["cat README.md CONTRIBUTING.md AGENTS.md"],
        "ask": """Compare this repository against the conventions its siblings
follow: an H1 that is the repository name, one bold sentence of claim, a badge
block, an ASCII diagram where the shape is not obvious, a Quickstart, a Docs
link list, and a License line. CONTRIBUTING carries the commit rules, the
writing rules and the doc map. AGENTS.md routes and holds no knowledge of its
own.

Report every place this repository reads as a different project rather than a
member of the same family, and propose the specific edit.""",
    },
    {
        "id": "REV-07",
        "title": "Claims the code does not support",
        "evidence": ["cat README.md MANUAL.md"],
        "ask": """Every sentence in the documentation that states a fact about
the software is a claim. Check the load-bearing ones against the source: the
performance numbers, the platform support, the guarantees, the "never" and
"always" statements, and the list of what is supported.

Report each claim you could not ground in the code, and say what the code
actually does. A claim that survives publication and turns out to be false is
the expensive kind.""",
    },
    {
        "id": "REV-08",
        "title": "The history a reader scrolls",
        "evidence": ["git log --format='%h %s' | head -80",
                     "git log --format='%B' | head -200"],
        "ask": """Read the commit history as a stranger evaluating whether to
depend on this project. Report subjects that say nothing ("fixes", "wip",
"updates"), bodies that narrate a debugging session rather than describe a
design, messages that reference an internal ticket or a conversation the reader
cannot see, and any message that would embarrass someone.

Say for each whether it is worth rewriting before publication, given that
rewriting is possible now and impossible afterwards.""",
    },
]


def render_reviews(repo, apply_fixes):
    out = [f"# Review pack for {repo.project()}", "",
           "Each section below is one review. Run them one at a time, gather the",
           "evidence named, and report findings with a file and a line.", ""]
    if apply_fixes:
        out += ["**Apply the fixes.** For each finding, make the change in the",
                "repository, then re-run `python3 scripts/lint-oss.py` and the",
                "project's own `make check`. Leave anything that is the owner's",
                "decision unapplied, and list it instead.", ""]
    else:
        out += ["**Report only.** Do not change anything; this pass produces a",
                "list. Re-run with `--fix` to have the changes applied.", ""]
    for review in REVIEWS:
        out += [f"## {review['id']} — {review['title']}", "",
                "Evidence to gather first:", "", "```bash",
                *review["evidence"], "```", "", review["ask"].strip(), ""]
    return "\n".join(out)


def run_reviews(repo, apply_fixes):
    """Hand each review to `claude -p`, one at a time."""
    if not shutil.which("claude"):
        print("claude is not on PATH; printing the pack instead\n")
        print(render_reviews(repo, apply_fixes))
        return 0
    failures = 0
    for review in REVIEWS:
        print(f"\n=== {review['id']} — {review['title']} ===\n", flush=True)
        prompt = (f"You are reviewing the repository at {ROOT} before it is "
                  f"published as open source.\n\n"
                  f"{review['ask'].strip()}\n\n"
                  + ("Apply every fix that is not the owner's decision to make, "
                     "then re-run `python3 scripts/lint-oss.py` and `make check`.\n"
                     if apply_fixes else
                     "Report findings only. Change nothing.\n"))
        run = subprocess.run(["claude", "-p", prompt], cwd=ROOT, text=True)
        if run.returncode != 0:
            failures += 1
            print(f"{review['id']}: the review did not complete", file=sys.stderr)
    return 1 if failures else 0


# ---------------------------------------------------------------------------
# Running them
# ---------------------------------------------------------------------------
def _wants_online(fn):
    return "online" in fn.__code__.co_varnames[:fn.__code__.co_argcount]


def run_checks(repo, online):
    results = []
    for rid, severity, title, why, fn in RULES:
        try:
            found = fn(repo, online=online) if _wants_online(fn) else fn(repo)
        except Exception as exc:                    # a broken check must not
            found = [warn(rid, f"the check itself failed: {exc!r}")]  # hide the rest
        for problem in found:
            if problem.rule in ALLOW and problem.severity == ERROR:
                problem.severity = SKIP
                problem.message = (f"waived: {ALLOW[problem.rule]} "
                                   f"(would have failed: {problem.message})")
            results.append(problem)
    return results


def main():
    argv = sys.argv[1:]
    online = "--online" in argv
    apply_fixes = "--fix" in argv

    if "--explain" in argv:
        for rid, severity, title, why, _ in RULES:
            print(f"{rid}  [{severity}]  {title}")
            print("    " + " ".join(why.split()))
            print()
        return 0

    repo = Repo()
    if not repo.tracked:
        print("oss: this is not a git repository, or it tracks nothing")
        return 1

    if "--list" in argv:
        print(f"project:   {repo.project()}")
        print(f"remote:    {repo.slug() or '(none)'}")
        print(f"published: {CONFIG['PUBLISHED']}")
        print(f"tracked:   {len(repo.tracked)} files, {len(repo.text)} read as text")
        print(f"docs:      {', '.join(ALL_DOCS)}")
        print(f"skipped:   {', '.join(sorted(SKIP_PATHS)) or '(none)'}")
        print(f"waived:    {', '.join(sorted(ALLOW)) or '(none)'}")
        return 0

    if "--review" in argv:
        if "--agent" in argv:
            return run_reviews(repo, apply_fixes)
        print(render_reviews(repo, apply_fixes))
        return 0

    problems = run_checks(repo, online)
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
    print(f"oss: {len(errors)} error(s), {len(warnings)} warning(s), "
          f"{len(skips)} skipped. `--explain` says what each rule wants.")
    if not online:
        print("     Run with --online to check the repository's own settings.")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
