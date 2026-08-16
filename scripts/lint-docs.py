#!/usr/bin/env python3
"""Check the prose in this project's Markdown against the writing rules.

The rules are in CONTRIBUTING.md under "Writing". They exist because docs drift
into a style that reads as terse and knowing rather than clear: verbless
epigrams, sentences carrying two or three em-dashes, and terms used pages before
anything defines them.

Every check here is mechanical and quotable. Anything that needs judgement is
left to review, because a linter that guesses produces noise, and noise gets
ignored.

    scripts/lint-docs.py            # check
    scripts/lint-docs.py --stats    # and print the per-file measurements
    scripts/lint-docs.py --list     # show which files are checked

Code fences, tables and link definitions are excluded throughout: they are not
prose and none of the rules are about them.

TUNING. Nothing in this file is project-specific. Every knob lives in
lint-docs.config.py beside it: the glossary a reader of this project cannot
infer, the directories to skip, the project's own lower-case command name, and
any verb the shared list does not carry. Tune there until every reported problem
is a real one. A check that cries wolf is worse than no check.

This file is vendored from a shared copy and is meant to stay byte-identical
across projects. Fix a check there, then copy it out again; the config file
beside it is what carries the local differences.
"""

import pathlib
import re
import sys

# The project's knobs, read from the file beside this one. It is exec'd rather
# than imported because the hyphen in the name is not a legal module name, and
# renaming it would hide the pairing the two files have. A project with nothing
# to tune can leave the file out entirely.
CONFIG = dict(SKIP_EXTRA=set(), GLOSSARY=[], LOWERCASE_STARTERS=[],
              PROJECT_VERBS=[])
_config_file = pathlib.Path(__file__).with_name("lint-docs.config.py")
if _config_file.exists():
    exec(compile(_config_file.read_text(), str(_config_file), "exec"), CONFIG)

# PROJECT-SPECIFIC, from the config file. The domain terms a reader of this
# project cannot infer, each of which must be introduced where a document first
# uses it: glossed on the spot, defined in a glossary table, or linked to the
# page that defines it. An empty list disables the check and gives up the most
# valuable of them.
GLOSSARY = CONFIG["GLOSSARY"]

# PROJECT-SPECIFIC, from the config file. Words that legitimately start a
# sentence in lower case, which is nearly always the project's own command name.
# Without them the splitter reads "Nothing prompts. cs-ledger exits 1." as a
# single 6-word sentence.
LOWERCASE_STARTERS = CONFIG["LOWERCASE_STARTERS"]

# Every Markdown file in the repository root, plus docs/ if it exists. Generated
# or vendored trees are skipped: they are not this project's prose. The config
# file adds any directory holding fixtures, corpora or generated Markdown, and
# any root file that is data rather than documentation.
SKIP = {"node_modules", "vendor", "dist", "bin", "target", "build", ".git",
        "third_party", "testdata", "CHANGELOG.md"} | set(CONFIG["SKIP_EXTRA"])


def docs():
    root = pathlib.Path(".")
    found = sorted(p for p in root.glob("*.md") if p.name not in SKIP)
    for sub in ("docs", "doc"):
        d = root / sub
        if d.is_dir():
            found += sorted(p for p in d.rglob("*.md")
                            if not set(p.parts) & SKIP and p.name not in SKIP)
    return [str(p) for p in found]

# One em-dash in a paragraph reads as an aside; three read as a writer who will
# not commit to a sentence.
MAX_EM_DASHES_PER_PARAGRAPH = 1

# Long enough for a qualified statement, short enough to hold one idea.
MAX_SENTENCE_WORDS = 30

# Frames that comment on the writing instead of getting on with it. Each one is
# a phrase you can delete without losing a fact.
THROAT_CLEARING = [
    "it is worth", "worth stating", "worth noting", "worth saying", "put simply",
    "simply put", "the point is", "needless to say", "suffice it to say",
    "stated plainly", "to be clear", "in other words", "that said,",
]

# What counts as introducing a term on the spot.
GLOSS = re.compile(
    r"\b(is|are|means|holds|records|names|covers)\b|[:(]|\bcalled\b|\bthat is\b", re.I)

# An epigram is short by nature: "Two version numbers, one verdict, one remedy."
# Above this length a sentence that trips the verb check is almost always a false
# positive with a verb the list does not carry, so the check gives up rather than
# cry wolf.
MAX_EPIGRAM_WORDS = 12

# A sentence needs one of these to be a sentence. Deliberately generous: the
# check is for epigrams with no verb at all, not for unusual verbs. This is the
# union of what every project using this linter has needed; a project adds only
# what is its own, through PROJECT_VERBS in the config file.
SHARED_VERBS = r"""
is|are|was|were|be|been|being|has|have|had|does|do|did|can|cannot|could|may|
might|must|should|would|will|serves?|holds?|keeps?|makes?|takes?|gives?|
gets?|goes|comes?|runs?|sends?|reads?|writes?|records?|replays?|matches|
means?|needs?|names?|shows?|says?|calls?|called|tells?|lets?|leaves?|puts?|
adds?|drops?|
refuses?|reports?|carries|carry|costs?|works?|fails?|exists?|belongs?|
applies|apply|covers?|happens?|arrives?|appears?|starts?|stops?|waits?|wants?|uses?|
sits?|lives?|turns?|appends?|aligns?|differs?|blanks?|restores?|proposes?|
checks?|answers?|asks?|ships?|gates?|grab|see|verify|compares?|streams?|
beats?|part|updates?|prints?|install|build|produces?|requires?|expects?|
prefers?|avoids?|treats?|points?|binds?|forwards?|spends?|contacts?|
reach(?:es)?|decides?|chooses?|splits?|joins?|stores?|loads?|opens?|closes?|
follows?|describes?|explains?|lists?|pins?|hooks?|collects?|removes?|
deletes?|creates?|allows?|blocks?|passes?|picks?|sorts?|contains?|includes?|
involves?|supports?|offers?|returns?|accepts?|ignores?|skips?|ends?|begins?|
spans?|tracks?|marks?|flags?|counts?|emits?|wraps?|wins?|fights?|helps?|
meets?|breaks?|hides?|lands?|leads?|routes?|builds?|assembles?|behaves?|
settles?|strips?|throws?|hangs?|dials?|exposes?|guards?|changes?|proxies|
proxy|consults?|negotiates?|matched|matching|selects?|introduces?|walks?|
reaches?|stays?|target|resolves?|state|say|note|fold|split|number|cut|
delete|links?|output|discovers?|produced|caught|moves?|repoint|fix|becomes|
measure|generate|capture|paste|feed|check|run|add|show|list|name|bury|
document|confirm|record|report|prove|extend|tune|wire|ship|drop|keep|sets?|
bumps?|catches|catch|edits?|copies|copy|write|read|pass|try|editing|
pointing|running|recording|replaying|calling|adding|search|classify|reorder|
invalidates?|renders?|validates?|iterates?|quote|reproduce|rewrite|matters?|
mint|land|share|scrolls?|handles?|assert|drive|exercise|push|comments?|
refreshes?|scaffolds?|promotes?|named|reuse|render|refresh|validate|mints?|
accretes?|files?|renumbers?|schedules?|scheduled|claims?|claimed|satisfies|
satisfy|self-hosts?|predates?|conforms?|derives?|embeds?|inlines?|surfaces?|
travels?|iterate|pushes?|shares?|handle|teleports?|prompts?|reflows?|occupy|
occupies|said|saw|attribute|downgrade|silencing|clobbered|earns?|accreting|
honest|tunes?|restrict|narrows?|address|reclaim|boots?|reaches|inherits?|
pivots?|disturb|destroys?|recreat(?:e|ing)|mounts?|pulls?|denies?|deny|lends?|
lend|trust|skip|spin|hand|oversee|wall(?:ed)?|specify|specifies|falls?|
addresses|source|chafe|climbs?|adopts?|sweeps?|probes?|volunteers?|merges?|
finds?|hands?|stalls?|head|provide|resolve|exhaust|re-enables?|touch(?:es)?|
anchors?|wedge|trips?|owes?|rules?|shaped?|simplifying|reintroduces?|
rebuild|scrape"""
VERBS = SHARED_VERBS + "".join(f"|{v}" for v in CONFIG["PROJECT_VERBS"])
VERB_RE = re.compile(rf"\b({VERBS})\b", re.I | re.X)

# A sentence boundary: a terminator, then whitespace, then something that starts
# a sentence — a capital, a markdown marker, or one of the project's own
# lower-case names.
#
# Quotation marks sit on both sides of a boundary and belong in both classes.
# A sentence can end inside them, as `he called it "done." It shipped` does, and
# the next can open with one, as `stops on it. "No key MUST be written"` does.
# Without them the splitter joins the two and reports a length that is not real.
_CLOSERS = "*`)\\]\"'”’"
_OPENERS = "A-Z`*\\[\"'“‘"
_STARTERS = "".join(rf"|{re.escape(w)}\b" for w in LOWERCASE_STARTERS)
BOUNDARY = re.compile(rf"(?<=[.!?])[{_CLOSERS}]*\s+(?=[{_OPENERS}]{_STARTERS})")


def prose(text):
    """The document with everything that is not prose removed."""
    text = re.sub(r"```.*?```", "", text, flags=re.S)   # fenced code
    # HTML comments, including the marker pairs a tool injects to own a block of
    # a file. The raw-HTML pattern below cannot catch these: its \b after the
    # tag name never matches the space in "<!-- MARKER -->".
    text = re.sub(r"<!--.*?-->", "", text, flags=re.S)
    text = re.sub(r"^\s*\|.*$", "", text, flags=re.M)   # tables
    text = re.sub(r"^\s*\[[^\]]+\]:.*$", "", text, flags=re.M)  # link defs
    # Raw HTML blocks. The tag list is explicit on purpose: a bare "starts with
    # <word>" pattern also eats a line beginning with a placeholder like <name>,
    # and swallowing its closing backtick corrupts every code span after it.
    text = re.sub(r"^\s*</?(?:p|div|img|a|sub|sup|i|b|em|strong|br|hr|span|table|tr|td|th"
                  r"|details|summary|picture|source|video|h[1-6]|!--)\b[^>]*>.*$",
                  "", text, flags=re.M | re.I)
    text = re.sub(r"^\s*> ?", "", text, flags=re.M)     # blockquote markers: the content is prose
    text = re.sub(r"`[^`]*`", "CODE", text)             # inline code
    return text


def sentences(paragraph):
    # Not a full sentence splitter: abbreviations and version numbers would
    # break one. Splitting on a terminator followed by a capital is enough for
    # a length check, and errs towards longer rather than shorter. The optional
    # markers after the terminator are markdown emphasis closing on the full
    # stop, which would otherwise hide the boundary.
    return [s.strip() for s in re.split(BOUNDARY, paragraph) if s.strip()]


def units(paragraph):
    """A paragraph's prose units: a list is one unit per item, not one blob."""
    out, buf, in_item = [], [], False
    for line in paragraph.split("\n"):
        if re.match(r"\s*([-*+]|\d+\.)\s", line):
            if buf:
                out.append(" ".join(buf))
            buf, in_item = [line.strip()], True
        elif in_item and line.startswith((" ", "\t")):
            # An indented line continues the item above it, not a new sentence.
            buf.append(line.strip())
        else:
            if in_item and buf:
                out.append(" ".join(buf))
                buf, in_item = [], False
            buf.append(line.strip())
    if buf:
        out.append(" ".join(buf))
    return [u for u in out if u.strip()]


def check(path):
    raw = pathlib.Path(path).read_text()
    text = prose(raw)
    problems = []

    for para in text.split("\n\n"):
        flat = " ".join(para.split())
        if not flat or flat.startswith("#"):
            continue

        # Counted per unit, not per paragraph: a list of "**Term** — meaning"
        # entries is a readable pattern, and one em-dash belongs to each item
        # rather than all of them to the list.
        for u in units(para):
            one = " ".join(u.split())
            n = one.count("—")
            if n > MAX_EM_DASHES_PER_PARAGRAPH:
                problems.append((f"{n} em-dashes in one paragraph "
                                 f"(max {MAX_EM_DASHES_PER_PARAGRAPH})", one))

        for s in [x for u in units(para) for x in sentences(" ".join(u.split()))]:
            words = s.split()
            # Bullets and headings are not sentences.
            if s.lstrip().startswith(("-", "*", ">", "#", "|", "[![")):
                continue
            # A line that is only a link, or only a link plus a gloss, is a
            # list entry in prose clothing.
            if re.fullmatch(r"[\[!].*", s) and s.count("](") >= 1 and len(words) < 14:
                continue
            if len(words) > MAX_SENTENCE_WORDS:
                problems.append((f"{len(words)}-word sentence "
                                 f"(max {MAX_SENTENCE_WORDS})", s))
            if 3 <= len(words) <= MAX_EPIGRAM_WORDS and not VERB_RE.search(s):
                problems.append(("no verb — an epigram, not a sentence", s))

    problems += undefined_terms(raw)
    problems += throat_clearing(text)
    problems += echoes(text)
    problems += unshown_scripts(raw)
    return problems


def throat_clearing(text):
    out = []
    for phrase in THROAT_CLEARING:
        for m in re.finditer(re.escape(phrase), text, re.I):
            if quoted(text, m.start()):
                continue    # naming a phrase is not using it
            line = " ".join(text[max(0, m.start() - 60): m.start() + 90].split())
            out.append((f"'{phrase}' comments on the writing; delete the frame", line))
    return out


def quoted(text, pos):
    """Whether the offset sits inside quotation marks on its own line."""
    start = text.rfind("\n", 0, pos) + 1
    return text.count('"', start, pos) % 2 == 1


# Words common enough that repeating them says nothing about the sentence, plus
# CODE, which is what an inline code span was replaced with.
COMMON = set("""the a an and or of to in is are was were be it its this that these those
for with on at by as not no so if but who whom whose one two all any each every some
have has had do does did can could may might must should would will code been being
into onto from over under about through within without after before where while both
against
only also other others same such very more most than then there here when what which""".split())


def echoes(text):
    """One content word three times in a sentence, which reads as circling.

    Three rather than two, and deliberately: repeating a word twice is often the
    clearest thing to do, and a check that argued about it would be noise. This
    catches only "X is the difference, and it is the difference that ..." — the
    sentence that says the same thing twice and lands nowhere.
    """
    out = []
    for para in text.split("\n\n"):
        for u in units(para):
            for sent in sentences(" ".join(u.split())):
                bare = re.sub(r"\[[^\]]*\]\([^)]*\)", "", sent)  # link text and target
                seen = {}
                for w in re.findall(r"[a-zA-Z][a-zA-Z'-]{3,}", bare.lower()):
                    if w not in COMMON:
                        seen[w] = seen.get(w, 0) + 1
                for w, n in sorted(seen.items()):
                    if n >= 3:
                        out.append((f"'{w}' {n} times in one sentence — it circles", sent))
                        break
    return out


def unshown_scripts(raw):
    """A command that runs a script the document never showed the reader."""
    out = []
    shown = set(re.findall(r"#\s*([\w.-]+\.(?:sh|py|js|rb))", raw))
    shown |= set(re.findall(r"(?:cat|tee)\s*>?\s*([\w./-]+\.(?:sh|py))", raw))
    for m in re.finditer(r"\./([\w.-]+\.(?:sh|py|js|rb))", raw):
        name = m.group(1)
        if name in shown or name.startswith("scripts/"):
            continue
        # Inside backticks it is being named, not run.
        line_start = raw.rfind("\n", 0, m.start()) + 1
        if raw.count("`", line_start, m.start()) % 2 == 1:
            continue
        # Shown later still counts as shown; the check is for never.
        out.append((f"./{name} is run but never shown to the reader",
                    " ".join(raw[max(0, m.start() - 70): m.start() + 60].split())))
    return out


def undefined_terms(raw):
    """A glossary term must be introduced where a document first uses it."""
    out = []
    # Headings name a term without introducing it, and "# Cassettes" is not a
    # first use in any sense a reader cares about.
    body = re.sub(r"^#+ .*$", "", prose(raw), flags=re.M)
    # Single-asterisk emphasis is the use/mention convention: a sentence about
    # the word *draft* is not a sentence that uses drafts. Double-asterisk bold
    # is ordinary emphasis and stays.
    body = re.sub(r"(?<!\*)\*([^*\n]+)\*(?!\*)", "MENTION", body)
    # A glossary table introduces every term in it, wherever it sits.
    defined = {t.lower() for t in
               re.findall(r"^\|\s*\*\*([\w -]+)\*\*\s*\|", raw, flags=re.M)}
    # Introducing it elsewhere counts if the document links to that page first.
    for term in GLOSSARY:
        if any(d.startswith(term) for d in defined):
            continue
        m = re.search(rf"\b{term}\w*\b", body, re.I)
        if not m:
            continue
        # The paragraph it lands in, and everything before it.
        before = body[:m.start()]
        para_start = body.rfind("\n\n", 0, m.start()) + 2
        para_end = body.find("\n\n", m.end())
        para = body[para_start: para_end if para_end > 0 else len(body)]
        if GLOSS.search(para) or "](" in before:
            continue
        out.append((f"'{term}' used before anything introduces it",
                    " ".join(para.split())[:110]))
    return out


def stats(path):
    text = prose(pathlib.Path(path).read_text())
    words = text.split()
    sents = [s for p in text.split("\n\n") for u in units(p)
             for s in sentences(" ".join(u.split())) if len(s.split()) > 2]
    avg = sum(len(s.split()) for s in sents) / max(len(sents), 1)
    you = len(re.findall(r"\byou\b|\byour\b", text, re.I))
    return len(words), avg, text.count("—") / max(len(words), 1) * 100, you


def main():
    show_stats = "--stats" in sys.argv
    files = docs()
    if "--list" in sys.argv:
        for f in files:
            print(f)
        return 0
    if not files:
        print("docs: no Markdown found")
        return 0
    total = 0
    for path in files:
        problems = check(path)
        total += len(problems)
        if problems:
            print(f"\n{path}")
            for why, quote in problems:
                print(f"  {why}")
                print(f"    {quote[:150]}")
    if show_stats:
        print(f"\n{'file':28} {'words':>6} {'avg sentence':>13} "
              f"{'em-dash/100w':>13} {'you':>5}")
        for path in files:
            w, avg, em, you = stats(path)
            print(f"{path:28} {w:6} {avg:13.1f} {em:13.2f} {you:5}")
    if total:
        print(f"\n{total} problem(s). The rules are in CONTRIBUTING.md.")
        return 1
    print("docs: prose ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
