#!/usr/bin/env python3
"""Render docs/demo.cast (an asciinema v2 cast) to docs/demo.gif.

The cast is hand-authored; this renderer turns it into the small, dark-themed GIF shown at the top
of the README. It models a 78x20 terminal grid with xterm-256 color, types out the command lines
character by character, holds the output lines, and quantizes to a small shared palette so the GIF
stays ~80 KB.

Usage (from anywhere):
    python3 docs/render-demo.py [OUTPUT.gif]      # default output: docs/demo.gif

Requires: Pillow, and a monospace TrueType font (Noto Sans Mono by default).
"""
import json
import os
import re
import sys

from PIL import Image, ImageDraw, ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))
CAST = os.path.join(HERE, "demo.cast")
OUT = sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "demo.gif")

COLS, ROWS = 78, 20          # terminal grid (matches the cast header)
W, H = 1026, 638             # canvas pixels (matches the original GIF)
BG = (24, 24, 27)            # background (#18181b)
DEF = (176, 176, 182)        # default foreground (SGR reset)
PADX, PADY = 21, 25
CW = (W - 2 * PADX) / COLS    # cell width / height; glyphs are placed on this grid
LH = (H - 2 * PADY) / ROWS
FONT_SIZE = 19
SPEED = 0.7                   # <1 = faster playback (scales every frame's duration)
FONT_CANDIDATES = [
    "/usr/share/fonts/google-noto-vf/NotoSansMono[wght].ttf",
    "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
    "/usr/share/fonts/liberation/LiberationMono-Regular.ttf",
]


def load_font():
    for p in FONT_CANDIDATES:
        if os.path.exists(p):
            return ImageFont.truetype(p, FONT_SIZE)
    raise SystemExit("no monospace TTF found; edit FONT_CANDIDATES")


font = load_font()


def xterm(n):
    """xterm 256-color index -> RGB."""
    base = [(0, 0, 0), (205, 0, 0), (0, 205, 0), (205, 205, 0), (0, 0, 238), (205, 0, 205),
            (0, 205, 205), (229, 229, 229), (127, 127, 127), (255, 0, 0), (0, 255, 0),
            (255, 255, 0), (92, 92, 255), (255, 0, 255), (0, 255, 255), (255, 255, 255)]
    if n < 16:
        return base[n]
    if n >= 232:
        v = 8 + 10 * (n - 232)
        return (v, v, v)
    n -= 16
    cv = [0, 95, 135, 175, 215, 255]
    return (cv[n // 36], cv[(n % 36) // 6], cv[n % 6])


# --- virtual terminal: a grid of (char, fg) cells, a cursor, and the current fg ---
grid = [[(" ", DEF) for _ in range(COLS)] for _ in range(ROWS)]
cur = [0, 0]
fg = [DEF]


def newline():
    cur[0] += 1
    cur[1] = 0
    if cur[0] >= ROWS:                       # scroll
        grid.pop(0)
        grid.append([(" ", DEF) for _ in range(COLS)])
        cur[0] = ROWS - 1


def putch(ch):
    if ch == "\r":
        cur[1] = 0
        return
    if ch == "\n":
        newline()
        return
    if cur[1] >= COLS:
        newline()
    grid[cur[0]][cur[1]] = (ch, fg[0])
    cur[1] += 1


def render():
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    for r in range(ROWS):
        for c in range(COLS):
            ch, col = grid[r][c]
            if ch != " ":
                d.text((PADX + c * CW, PADY + r * LH - 2), ch, font=font, fill=col)
    return img


CSI = re.compile(r"\x1b\[([0-9;]*)([mK])")    # SGR colors (m) + erase-line (K)


def apply_sgr(code):
    parts = [p for p in code.split(";") if p != ""] or ["0"]
    i = 0
    while i < len(parts):
        p = int(parts[i])
        if p == 0:
            fg[0] = DEF
        elif p == 97:                        # bright white = a typed command
            fg[0] = (255, 255, 255)
        elif p == 38 and i + 2 < len(parts) and parts[i + 1] == "5":
            fg[0] = xterm(int(parts[i + 2]))
            i += 2
        i += 1


def erase_line(param):                        # CSI K: clear part of the current row
    p = param or "0"
    row = grid[cur[0]]
    rng = range(cur[1], COLS) if p in ("0", "") else \
          range(0, min(cur[1] + 1, COLS)) if p == "1" else range(COLS)
    for c in rng:
        row[c] = (" ", DEF)


events = []
for line in open(CAST):
    line = line.strip()
    if line.startswith("["):
        events.append(json.loads(line))

frames, durs = [], []
for idx, (t, _typ, text) in enumerate(events):
    nt = events[idx + 1][0] if idx + 1 < len(events) else t + 1.2
    typed = text.startswith("\x1b[97m")      # white text = the user typing a command
    # tokenize into CSI codes ('m'/'K') and characters, in order
    toks, pos = [], 0
    for m in CSI.finditer(text):
        toks += [("c", ch) for ch in text[pos:m.start()]]
        toks.append((m.group(2), m.group(1)))    # kind = 'm' or 'K', val = params
        pos = m.end()
    toks += [("c", ch) for ch in text[pos:]]

    def step(kind, v):
        if kind == "c":
            putch(v)
        elif kind == "m":
            apply_sgr(v)
        elif kind == "K":
            erase_line(v)

    if typed:                                # animate it appearing, one char per frame
        for kind, v in toks:
            step(kind, v)
            if kind == "c" and v not in "\r\n":
                frames.append(render())
                durs.append(round(80 * SPEED))
    else:                                    # output: render once, hold until the next event
        for kind, v in toks:
            step(kind, v)
        frames.append(render())
        durs.append(max(round(120 * SPEED), min(round(1400 * SPEED),
                                                round((nt - t) * 1000 * SPEED))))

frames.append(render())                      # final frame (the GIF plays once and holds here)
durs.append(1500)

# quantize to one shared 48-color palette (no dither) -> small file, ~80 KB.
# No `loop=` -> the GIF plays through once and holds on the last frame (disposal=1 keeps it).
pal = frames[-1].quantize(colors=48, method=Image.Quantize.MEDIANCUT)
q = [f.quantize(palette=pal, dither=Image.Dither.NONE) for f in frames]
q[0].save(OUT, save_all=True, append_images=q[1:], duration=durs,
          optimize=True, disposal=1)
print(f"wrote {OUT}  frames={len(frames)}  size={frames[0].size}")
