-- nvim-treesitter, `main` branch (the post-rewrite plugin; requires Nvim >= 0.12,
-- tree-sitter-cli, curl/tar and a C compiler — all baked into the image).
--
-- The `main` rewrite is a DIFFERENT plugin from the old `master` one: `setup()` only
-- takes `install_dir`, parsers are installed with `install()`, and NONE of the
-- features are enabled automatically — highlight/indent/folds are turned on per
-- buffer below. `main` also cannot be lazy-loaded, hence `lazy = false`.
local parsers = {
  "bash",
  "c",
  "cpp",
  "css",
  "go",
  "gomod",
  "html",
  "java",
  "javascript",
  "json",
  "lua",
  "markdown",
  "markdown_inline",
  "nix",
  "python",
  "rust",
  "scala",
  "typescript",
  -- the parser Neovim uses for BOTH typescriptreact (.tsx) and javascriptreact (.jsx);
  -- the "typescript" parser above only covers plain .ts
  "tsx",
  "yaml",
}

-- treesitter indentation is upstream-flagged "experimental"; flip this off to fall
-- back to Nvim's built-in 'smartindent'/'autoindent' (see init.lua).
local enable_indent = true

return {
  "nvim-treesitter/nvim-treesitter",
  branch = "main",
  lazy = false,
  build = ":TSUpdate",
  config = function()
    local ts = require("nvim-treesitter")

    -- Parsers + queries land in stdpath("data")/site — relocatable content, so the
    -- image can pre-build them into the home seed.
    ts.setup({})

    local function missing_parsers()
      local installed = {}
      for _, lang in ipairs(ts.get_installed("parsers")) do
        installed[lang] = true
      end
      return vim.tbl_filter(function(lang)
        return not installed[lang]
      end, parsers)
    end

    -- The image pre-installs every parser above, so this is a no-op in a sandbox.
    -- It only does work — and only for what is missing — if you edit the list or
    -- run this config somewhere unprovisioned. A normal start touches no network.
    local missing = missing_parsers()
    if #missing > 0 then
      ts.install(missing)
    end

    -- Blocking variant, used by the image build to bake the parsers into the home
    -- seed (`nvim --headless +TSInstallSync! +qa`) — `install()` is async, and
    -- `build = ":TSUpdate"` only refreshes parsers that are already installed.
    -- With `!` a parser that did not install exits Neovim non-zero, so a half-done
    -- pre-build fails the image build instead of shipping.
    vim.api.nvim_create_user_command("TSInstallSync", function(cmd)
      ts.install(parsers, { summary = true }):wait(600000)
      local still_missing = missing_parsers()
      if #still_missing > 0 then
        vim.api.nvim_echo(
          { { "TSInstallSync: parsers failed to install: " .. table.concat(still_missing, " "), "ErrorMsg" } },
          true,
          {}
        )
        if cmd.bang then
          vim.cmd("cquit 1")
        end
      end
    end, { bang = true, desc = "Install this config's treesitter parsers, blocking (! = fail hard)" })

    -- Enable the treesitter features for any buffer whose parser is available.
    -- pcall because vim.treesitter.start() errors when there is no parser; such a
    -- buffer then simply keeps Vim's regex syntax highlighting.
    vim.api.nvim_create_autocmd("FileType", {
      group = vim.api.nvim_create_augroup("UserTreesitter", { clear = true }),
      callback = function(ev)
        if vim.b[ev.buf].large_file then
          return
        end
        if not pcall(vim.treesitter.start, ev.buf) then
          return
        end
        if enable_indent then
          vim.bo[ev.buf].indentexpr = "v:lua.require'nvim-treesitter'.indentexpr()"
        end
      end,
    })
  end,
}
