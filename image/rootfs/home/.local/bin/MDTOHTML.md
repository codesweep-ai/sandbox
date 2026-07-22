# Rendering Markdown to HTML with mdtohtml

`mdtohtml` converts a Markdown file into a **single standalone HTML file** styled like GitHub, with
Mermaid diagrams rendered in the browser. Use it whenever a user wants a Markdown document in a
shareable/printable form — a report, a design doc, notes — rather than raw `.md` text.

Its companion `mdview` does the same conversion but writes to a temp file and opens it in the
browser immediately, so no stray `.html` lands next to the source.

## How to use

```bash
mdtohtml README.md                    # -> README.html (prints the output path)
mdtohtml -o /tmp/out.html notes.md    # choose the output file
mdtohtml -t "Design notes" design.md  # set the browser/tab <title>
cat notes.md | mdtohtml -             # stdin -> stdout
mdtohtml report.md -o -               # stdout explicitly (pipe it somewhere)

mdview design.md                      # convert to a temp file and open it in the browser
```

Arguments and options:

- `<input.md>` — exactly one input file; `-` reads stdin.
- `-o, --output FILE` — output path; `-` means stdout. Default: the input with its extension
  replaced by `.html` (stdout when the input is `-`).
- `-t, --title TITLE` — the document `<title>`. Default: the input's base name (`document` for stdin).
- `-h, --help`, `-V, --version`.

On success it prints the output path (nothing when streaming to stdout), so you can capture it:
`out=$(mdtohtml notes.md)`.

## Interpreting user intent

| User says | What to do |
|---|---|
| "turn this into HTML" / "render this markdown" / "make it shareable" | `mdtohtml <file>.md` and report the output path |
| "let me look at it" / "open it in the browser" / "preview this" | `mdview <file>.md` (temp file, opens straight away) |
| "put it somewhere specific" / "write it to X" | `mdtohtml -o <path> <file>.md` |
| "give it a proper title" | `mdtohtml -t "<title>" <file>.md` |
| "render this diagram" (a ```mermaid block) | `mdtohtml` / `mdview` — Mermaid fences become diagrams |

## When to use proactively

- You produced a longer Markdown deliverable (a report, an audit, a design write-up) and the user
  will read rather than diff it — offer to render it.
- The document contains Mermaid fences: as raw Markdown they are unreadable, rendered they are the
  point.
- Do **not** convert files that are meant to stay Markdown in the repo (`README.md`, docs) unless
  the user asks — write the HTML to a temp path instead of beside the source.

## Notes

- **Requires `pandoc`** on PATH; it exits with an install hint if missing. `mdview` additionally
  needs a browser (`google-chrome`, else `xdg-open`).
- "Standalone" means one self-contained HTML file, but the GitHub stylesheet and the Mermaid script
  are pulled from a CDN — the page needs network access to look right and to draw diagrams. Keep
  that in mind before sending it somewhere offline or air-gapped.
- The default output **overwrites** `<input>.html` beside the source without asking; pass `-o` (or
  use `mdview`) when that is not wanted.
- Exactly one input file per run — it does not concatenate multiple Markdown files.
- Exit status: `0` on success, `2` when no input is given (prints usage), `1` on other errors.
