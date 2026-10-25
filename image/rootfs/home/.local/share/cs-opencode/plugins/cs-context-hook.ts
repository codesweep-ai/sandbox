// cs-context-hook for OpenCode: ask the agent to hand off when its context crosses a line.
//
// The same behaviour as the cs-context-hook script Claude Code and Codex run, as an OpenCode
// plugin, because OpenCode has no command hooks. It lives here, outside the profile, and the
// cs-opencode wrapper links it into the profile's plugins directory only when CS_HANDOFF_TOKENS
// is set: OpenCode installs its plugin SDK from npm on the first start that finds any plugin,
// and a member with no line set must start exactly as before. Linked in, it registers nothing
// unless the line is set.
//
// Environment: CS_HANDOFF_TOKENS (the line; unset or 0 is off), CS_HANDOFF_FILE (default
// ~/.cs-turns/handoff.md), CS_HANDOFF_ASK (the sentence, with {tokens} and {h}).
//
// On every tool result of the session the turn driver drives (~/.cs-turns/driven names it)
// it reads the context from the driver (`cs-opencode-turn --context`), adds the result that
// is about to be appended, and over the line appends the ask until the handoff file is newer
// than the first ask; then it says once more to end the turn, and after two more tool calls
// aborts the session. A later turn of the same session that is still over the line is asked
// again, because the driver counts only an ask made in its own turn. The ask is recorded in
// ~/.cs-turns/handoff-ask for the driver, which reports such a turn end as class `handoff`. A
// session whose first reading is already over the line is never asked, and
// ~/.cs-turns/handoff-floor records why. A child session (a sub-agent the task tool runs) is
// never asked: OpenCode gives it a parentID, which the plugin reads once per session. When
// OpenCode compacts, the summary is told to end by pointing at the handoff file.
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

type Session = { first?: number; floor?: number; askedAt?: number; afterFresh?: number }

function loadState(): Record<string, Session> {
  try {
    return JSON.parse(fs.readFileSync(STATE, "utf8"))
  } catch {
    return {}
  }
}

function saveJSON(file: string, data: unknown) {
  fs.mkdirSync(TURNS, { recursive: true })
  fs.writeFileSync(file + ".tmp", JSON.stringify(data))
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

export const ContextHook = async ({ client }: any) => {
  if (!(H > 0)) return {}
  // A child session runs in the parent's process, so it shares the parent's tmux session and
  // drivenHere cannot tell them apart. OpenCode records the parent on the child, as parentID.
  // The answer is kept per session id, so a session costs one call, not one per tool result.
  // A call that fails is not kept, and the session is treated as the main one.
  const child = new Map<string, boolean>()
  async function isChild(id: string): Promise<boolean> {
    const known = child.get(id)
    if (known !== undefined) return known
    try {
      const info = (await client.session.get({ path: { id } }))?.data
      if (info && typeof info === "object") {
        child.set(id, Boolean(info.parentID))
        return Boolean(info.parentID)
      }
    } catch {}
    return false
  }
  return {
    "tool.execute.after": async (input: any, output: any) => {
      try {
        if (await isChild(input.sessionID)) return
        if (!drivenHere()) return
        const base = reading(input.sessionID)
        if (base === null) return
        const tokens = base + Math.floor(String(output.output ?? "").length / 4)
        const state = loadState()
        const st: Session = state[input.sessionID] ?? {}
        state[input.sessionID] = st
        const turn = turnStarted()
        if (st.askedAt !== undefined && turn !== null && turn > st.askedAt) {
          delete st.askedAt
          delete st.afterFresh
        }
        if (st.first === undefined) {
          st.first = tokens
          if (tokens >= H) {
            st.floor = tokens
            saveJSON(FLOOR_PATH, { session: input.sessionID, tokens, h: H, at: Date.now() / 1000 })
          }
        }
        let stop = false
        if (st.floor === undefined && tokens >= H) {
          let fresh = false
          try {
            fresh = st.askedAt !== undefined && fs.statSync(HANDOFF).mtimeMs / 1000 >= st.askedAt
          } catch {}
          if (!fresh) {
            if (st.askedAt === undefined) {
              st.askedAt = Date.now() / 1000
              let session = ""
              try {
                session = fs.readFileSync(DRIVEN, "utf8").trim()
              } catch {}
              saveJSON(ASK_PATH, { session, sid: input.sessionID, tokens, h: H, at: st.askedAt })
            }
            output.output = `${output.output ?? ""}\n\n${ASK.replaceAll("{tokens}", String(tokens)).replaceAll("{h}", String(H))}`
          } else {
            st.afterFresh = (st.afterFresh ?? 0) + 1
            if (st.afterFresh > HARDSTOP_AFTER) stop = true
            else output.output = `${output.output ?? ""}\n\n${NUDGE}`
          }
        }
        const keys = Object.keys(state)
        for (const old of keys.slice(0, Math.max(0, keys.length - 50))) delete state[old]
        saveJSON(STATE, state)
        if (stop) {
          try {
            await client.session.abort({ path: { id: input.sessionID } })
          } catch {}
        }
      } catch (e) {
        // A hook that fails must not fail the tool call.
        console.error(`cs-context-hook: ${String(e).slice(0, 200)}`)
      }
    },
    "experimental.session.compacting": async (_input: any, output: any) => {
      output.context.push(
        `End the summary with this sentence: "After this summary, read ${HANDOFF} if it exists before continuing."`,
      )
    },
  }
}
