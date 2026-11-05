// cs-context-hook for OpenCode: ask the agent to hand off when its context crosses a line, or
// when the sandbox's token budget runs out.
//
// The same behaviour as the cs-context-hook script Claude Code and Codex run, as an OpenCode
// plugin, because OpenCode has no command hooks. It lives here, outside the profile, and the
// cs-opencode wrapper links it into the profile's plugins directory only when CS_HANDOFF_TOKENS
// or CS_METER_URL is set, so a member with neither starts exactly as it would without the
// feature. Linked in, it registers nothing unless one of them is set.
//
// Environment: CS_HANDOFF_TOKENS (the line; unset or 0 is off), CS_HANDOFF_FILE (default
// ~/.cs-turns/handoff.md), CS_HANDOFF_ASK (the sentence, with {tokens} and {h}); CS_METER_URL
// and CS_METER_TOKEN (where the lender answers with this sandbox's token count and budget, and
// the loan the reading is made with; unset is off), CS_BUDGET_ASK (the sentence, with {types}
// and {h}, the handoff file). The plugin runs in the server process, so these are the server's:
// cs-opencode-turn starts that server in the tmux session it drives.
//
// On every tool result of the session the turn driver drives (~/.cs-turns/driven names it)
// it reads the context from the driver (`cs-opencode-turn --context`), adds the result that
// is about to be appended, and over the line appends the ask until the handoff file is newer
// than the first ask; then it says once more to end the turn, and after two more tool calls
// interrupts the session. A later turn of the same session that is still over the line is asked
// again, because the driver counts only an ask made in its own turn. The ask is recorded in
// ~/.cs-turns/handoff-ask for the driver, which reports such a turn end as class `handoff`. A
// session whose first reading is already over the line is never asked, and
// ~/.cs-turns/handoff-floor records why. A child session (a sub-agent the subagent tool runs) is
// never asked: OpenCode gives it a parentID, which the plugin reads once per session. When
// OpenCode compacts, the summary is told to end by pointing at the handoff file.
//
// The budget line runs on the same tool results, with the same skips. It asks the lender for
// this sandbox's count (GET CS_METER_URL, about 3 s at most), and once the reading's `ask` names
// a token type it runs the same ask, nudge and interrupt, in its own words, with its own state.
// The ask is recorded in ~/.cs-turns/budget-ask, and the driver reports such a turn end as class
// `spent`. Where both lines would speak on one result, the budget line does: a fresh session on a
// context hand-off would not be sent its prompt either. It has no floor, because the driver sends
// no turn past the ask line. A reading that says the sandbox has no budget is kept for 30 s in
// ~/.cs-turns/budget-none, as the epoch it was written at, and no reading is taken while it is
// fresh. A reading that fails says nothing.
//
// opencode turns an exception thrown by a hook into a failed tool call, so every hook here
// catches its own.
import * as fs from "node:fs"
import * as path from "node:path"

const HOME = process.env.HOME ?? "/root"
const TURNS = path.join(HOME, ".cs-turns")
const STATE = path.join(TURNS, "handoff-state.json")
const ASK_PATH = path.join(TURNS, "handoff-ask")
const FLOOR_PATH = path.join(TURNS, "handoff-floor")
const DRIVEN = path.join(TURNS, "driven")
const HANDOFF = (process.env.CS_HANDOFF_FILE || path.join(TURNS, "handoff.md")).replace(/^~/, HOME)
const H = Number(process.env.CS_HANDOFF_TOKENS ?? "0")
const HARDSTOP_AFTER = 2
const ASK =
  process.env.CS_HANDOFF_ASK ||
  "[context] Your context is at {tokens} tokens, over the hand-off line of {h}. " +
    "Hand off now, as your instructions describe, then end your turn."
const NUDGE = "[context] Your handoff is written. End your turn now."
const METER_URL = process.env.CS_METER_URL ?? ""
const METER_TOKEN = process.env.CS_METER_TOKEN ?? ""
const BUDGET_ASK_PATH = path.join(TURNS, "budget-ask")
const NONE_PATH = path.join(TURNS, "budget-none")
const BUDGET_ASK =
  process.env.CS_BUDGET_ASK ||
  "[budget] This sandbox has used its {types} token budget. " +
    "Write your handoff to {h}, as your instructions describe, then end your turn."
const BUDGET_NUDGE = "[budget] Your handoff is written. End your turn now."
// The reading waits this long for the lender, and a no-budget answer is kept this long.
const METER_MS = 3000
const NONE_SECS = 30
// The lender's names for the token types, as its own messages spell them.
const NAMES: Record<string, string> = { input: "new-input", cached: "cached-input", output: "output" }

// One line's progress through its ask, nudge and stop.
type Line = { askedAt?: number; afterFresh?: number }
type Session = Line & { first?: number; floor?: number; budget?: Line }
type Reading = { limits: Record<string, unknown>; used?: Record<string, unknown>; ask: unknown[] }
type Act = { append?: string; stop?: boolean } | null

function loadState(): Record<string, Session> {
  try {
    return JSON.parse(fs.readFileSync(STATE, "utf8"))
  } catch {
    return {}
  }
}

function saveJSON(file: string, data: unknown) {
  saveText(file, JSON.stringify(data))
}

function saveText(file: string, text: string) {
  fs.mkdirSync(TURNS, { recursive: true })
  fs.writeFileSync(file + ".tmp", text)
  fs.renameSync(file + ".tmp", file)
}

// When the driven turn began: every driver rewrites the driven file as it starts one.
function turnStarted(): number | null {
  try {
    return fs.statSync(DRIVEN).mtimeMs / 1000
  } catch {
    return null
  }
}

function drivenHere(): boolean {
  const pane = process.env.TMUX_PANE
  if (!fs.existsSync(DRIVEN) || !pane) return true
  const want = fs.readFileSync(DRIVEN, "utf8").trim()
  if (!want) return true
  try {
    const got = Bun.spawnSync(["tmux", "display-message", "-p", "-t", pane, "#S"]).stdout.toString().trim()
    return got === want
  } catch {
    return true
  }
}

// The context the latest model call carried, from this family's own driver, or null.
function reading(sessionID: string): number | null {
  const local = path.join(HOME, ".local", "bin", "cs-opencode-turn")
  const driver = fs.existsSync(local) ? local : "cs-opencode-turn"
  try {
    const out = Bun.spawnSync([driver, "--context", "--uuid", sessionID], {
      env: { ...process.env, CS_TURN_LOG: "" },
    }).stdout.toString()
    for (const word of out.split(/\s+/)) {
      const [k, v] = word.split("=")
      if (k === "tokens") return /^\d+$/.test(v ?? "") ? Number(v) : null
    }
  } catch {}
  return null
}

// The text of a tool's result as the model will read it: its content, or its output when the
// content is empty, which is what opencode sends the model then.
function resultText(result: any): string {
  const c = result?.content
  if (typeof c === "string" && c !== "") return c
  if (Array.isArray(c) && c.length > 0)
    return c
      .filter((p: any) => p?.type === "text")
      .map((p: any) => String(p.text ?? ""))
      .join("\n")
  const o = result?.output
  if (o === undefined) return ""
  return typeof o === "string" ? o : JSON.stringify(o)
}

// Append a sentence to what the model reads back from the tool.
function append(result: any, sentence: string) {
  const c = result.content
  if (Array.isArray(c) && c.length > 0) c.push({ type: "text", text: `\n\n${sentence}` })
  else result.content = `${resultText(result)}\n\n${sentence}`
}

// The lender's reading of this sandbox's budget, or null. A reading is a 200 within METER_MS
// whose body is a JSON object with `limits` (an object, empty for no budget) and `ask` (a list).
async function meterReading(): Promise<Reading | null> {
  try {
    const res = await fetch(METER_URL, {
      headers: { Authorization: `Bearer ${METER_TOKEN}` },
      signal: AbortSignal.timeout(METER_MS),
    })
    if (res.status !== 200) return null
    const doc: any = await res.json()
    const isObject = (v: unknown) => typeof v === "object" && v !== null && !Array.isArray(v)
    if (!isObject(doc) || !isObject(doc.limits) || !Array.isArray(doc.ask)) return null
    return doc
  } catch {
    return null
  }
}

// True while the lender's last answer, under NONE_SECS old, was that there is no budget.
function noBudgetKnown(): boolean {
  try {
    const at = Number(fs.readFileSync(NONE_PATH, "utf8").trim())
    const age = Date.now() / 1000 - at
    return Number.isInteger(at) && age >= 0 && age < NONE_SECS
  } catch {
    return false
  }
}

function typeNames(types: string[]): string {
  const names = types.map((t) => NAMES[t] ?? t)
  return names.length === 1 ? names[0] : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`
}

function drivenSession(): string {
  try {
    return fs.readFileSync(DRIVEN, "utf8").trim()
  } catch {
    return ""
  }
}

// A line's ask belongs to the turn it was made in. In a later turn the line asks again, because
// the driver counts only an ask made in its own turn.
function newTurn(s: Line, turn: number | null) {
  if (s.askedAt !== undefined && turn !== null && turn > s.askedAt) {
    delete s.askedAt
    delete s.afterFresh
  }
}

// One result of a line that is over: the ask until the handoff file is newer than the first ask,
// then the nudge, then the interrupt after HARDSTOP_AFTER more tool calls. s is that line's own
// state, and record files its first ask for the driver.
function step(s: Line, record: (at: number) => void, ask: string, nudge: string): Act {
  let fresh = false
  try {
    fresh = s.askedAt !== undefined && fs.statSync(HANDOFF).mtimeMs / 1000 >= s.askedAt
  } catch {}
  if (!fresh) {
    if (s.askedAt === undefined) {
      s.askedAt = Date.now() / 1000
      record(s.askedAt)
    }
    return { append: ask }
  }
  s.afterFresh = (s.afterFresh ?? 0) + 1
  return s.afterFresh > HARDSTOP_AFTER ? { stop: true } : { append: nudge }
}

export default {
  id: "cs.context-hook",
  async setup(ctx: any) {
    if (!(H > 0) && !METER_URL) return
    // A child session runs in the parent's process, so it shares the parent's tmux session and
    // drivenHere cannot tell them apart. OpenCode records the parent on the child, as parentID.
    // The answer is kept per session id, so a session costs one call, not one per tool result.
    // A call that fails is not kept, and the session is treated as the main one.
    const child = new Map<string, boolean>()
    async function isChild(id: string): Promise<boolean> {
      const known = child.get(id)
      if (known !== undefined) return known
      try {
        const info = await ctx.session.get({ sessionID: id })
        if (info && typeof info === "object") {
          child.set(id, Boolean(info.parentID))
          return Boolean(info.parentID)
        }
      } catch {}
      return false
    }
    await ctx.tool.hook("execute.after", async (event: any) => {
      try {
        if (event.status !== "completed" || !event.result) return
        if (await isChild(event.sessionID)) return
        if (!drivenHere()) return
        const sid = event.sessionID
        const budget = METER_URL && !noBudgetKnown() ? await meterReading() : null
        if (budget !== null && Object.keys(budget.limits).length === 0) saveText(NONE_PATH, `${Math.floor(Date.now() / 1000)}\n`)
        const asks = budget === null ? [] : budget.ask.filter((t): t is string => typeof t === "string")
        const base = H > 0 ? reading(sid) : null
        if (base === null && asks.length === 0) return
        const state = loadState()
        const st: Session = state[sid] ?? {}
        state[sid] = st
        const turn = turnStarted()
        let act: Act = null
        if (base !== null) {
          const tokens = base + Math.floor(resultText(event.result).length / 4)
          newTurn(st, turn)
          if (st.first === undefined) {
            st.first = tokens
            if (tokens >= H) {
              st.floor = tokens
              saveJSON(FLOOR_PATH, { session: sid, tokens, h: H, at: Date.now() / 1000 })
            }
          }
          if (st.floor === undefined && tokens >= H)
            act = step(
              st,
              (at) => saveJSON(ASK_PATH, { session: drivenSession(), sid, tokens, h: H, at }),
              ASK.replaceAll("{tokens}", String(tokens)).replaceAll("{h}", String(H)),
              NUDGE,
            )
        }
        // The budget line goes second, so its word wins when both lines have one.
        if (budget !== null && asks.length > 0) {
          const bt: Line = (st.budget ??= {})
          newTurn(bt, turn)
          const used: Record<string, unknown> = budget.used && typeof budget.used === "object" ? budget.used : {}
          const pick = (m: Record<string, unknown>) => Object.fromEntries(asks.map((t) => [t, m[t] ?? null]))
          act = step(
            bt,
            (at) =>
              saveJSON(BUDGET_ASK_PATH, {
                session: drivenSession(),
                sid,
                types: asks,
                used: pick(used),
                limits: pick(budget.limits),
                at,
              }),
            BUDGET_ASK.replaceAll("{types}", typeNames(asks)).replaceAll("{h}", HANDOFF),
            BUDGET_NUDGE,
          )
        }
        const keys = Object.keys(state)
        for (const old of keys.slice(0, Math.max(0, keys.length - 50))) delete state[old]
        saveJSON(STATE, state)
        if (act?.append) append(event.result, act.append)
        if (act?.stop) {
          try {
            await ctx.session.interrupt({ sessionID: event.sessionID })
          } catch {}
        }
      } catch (e) {
        // A hook that fails must not fail the tool call.
        console.error(`cs-context-hook: ${String(e).slice(0, 200)}`)
      }
    })
    // The pointer at the handoff after a compaction belongs to the context line.
    if (!(H > 0)) return
    await ctx.session.hook("compaction", async (event: any) => {
      try {
        event.system.push({
          type: "text",
          text: `End the summary with this sentence: "After this summary, read ${HANDOFF} if it exists before continuing."`,
        })
      } catch (e) {
        console.error(`cs-context-hook: ${String(e).slice(0, 200)}`)
      }
    })
  },
}
