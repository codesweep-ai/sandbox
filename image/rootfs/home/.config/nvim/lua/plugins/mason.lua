-- Mason: installs the language servers and the formatters/linters.
--
-- The plugins moved from `williamboman/*` to `mason-org/*` (v2), which also dropped the
-- old lspconfig-framework bridge: mason-lspconfig now only installs servers and, for
-- each one it has installed, calls `vim.lsp.enable()` (see `automatic_enable`). The
-- per-server settings live in lsp.lua as `vim.lsp.config()` calls.
--
-- Installation is owned by mason-tool-installer for BOTH lists below, so there is a
-- single blocking command — `:MasonInstallSync` (defined at the bottom of this file) —
-- that the image build runs to bake every package in (see image/Containerfile). A
-- sandbox therefore starts with the servers already on disk and downloads nothing on
-- first launch.

-- language servers, by nvim-lspconfig name
--
-- The image ships servers for the mainstream languages only: Python, Java, Go and
-- JavaScript/TypeScript. Each one here is pre-installed into every image, so a server
-- for anything else belongs in a private MASON_ROOT (see below), not in this list.
local servers = {
  "basedpyright",
  "jdtls",
  "gopls",
  -- typescript-language-server. `vtsls` is the other common choice (it wraps VS Code's
  -- own TS extension and exposes more code actions) — swap the name if you want it.
  "ts_ls",
}

-- formatters / linters, by mason package name
local tools = {
  "prettier", -- prettier formatter
  "isort", -- python formatter
  "black", -- python formatter
  "pylint",
  "eslint_d",
}

-- Packages installed from a different source than their upstream registry entry names.
-- Each spec is upstream's with only the source changed, served to Mason as a Lua registry
-- listed ahead of the upstream one; the first registry that has a package wins.
--
-- basedpyright: upstream installs it from PyPI, where it depends on nodejs-wheel-binaries,
-- a ~200 MB Node private to its venv. The same release is on npm, and installed from there
-- it runs on the Node already on PATH. The version is pinned here and does not follow the
-- registry: bump it to upgrade.
--
-- A read-only root never refreshes its registries, so this one is not loaded there, and
-- `:Mason` describes basedpyright from the upstream entry. What is installed is this one.
local overrides = {
  basedpyright = {
    name = "basedpyright",
    description = "Fork of the Pyright static type checker for Python, with extra Pylance features.",
    homepage = "https://detachhead.github.io/basedpyright",
    licenses = { "MIT" },
    languages = { "Python" },
    categories = { "LSP" },
    source = { id = "pkg:npm/basedpyright@1.40.1" },
    schemas = {
      lsp = "vscode:https://raw.githubusercontent.com/DetachHead/basedpyright/v{{version}}/packages/vscode-pyright/package.json",
    },
    bin = {
      basedpyright = "npm:basedpyright",
      ["basedpyright-langserver"] = "npm:basedpyright-langserver",
    },
    neovim = { lspconfig = "basedpyright" },
  },
}

-- A Lua registry is a module that names one module per package spec; preload serves both.
local override_registry = "mason-overrides"
package.preload[override_registry] = function()
  local index = {}
  for name, spec in pairs(overrides) do
    index[name] = override_registry .. "." .. name
    package.preload[index[name]] = function()
      return spec
    end
  end
  return index
end

-- Where the packages live.
--
-- Mason bakes the absolute install path into the `bin/` wrappers (and into the venvs it
-- builds for the Python tools), so these ~250 MB of servers cannot be pre-installed into the
-- home skeleton and then copied to a developer's home — the wrappers would still point
-- at the build-time path. They are therefore installed into /opt like the sandbox's
-- other heavy toolchains: one shared, root-owned, path-stable copy that is never
-- duplicated into an instance's home. Read-only at runtime, same as /opt/pyenv & co.
--
-- Set MASON_ROOT (e.g. to ~/.local/share/nvim/mason) for a private, writable Mason if
-- you want to add packages from inside Neovim; everything in the lists above is then
-- installed on demand.
local shared_root = "/opt/nvim/mason"
local mason_root = vim.env.MASON_ROOT
if not mason_root or mason_root == "" then
  mason_root = (vim.fn.isdirectory(shared_root) == 1) and shared_root or (vim.fn.stdpath("data") .. "/mason")
end
-- Create the root if it is missing, so a new private root counts as writable. fs_access()
-- is false for a missing directory, and a read-only root never downloads the registry.
pcall(vim.fn.mkdir, mason_root, "p")
local writable = vim.uv.fs_access(mason_root, "W") or false

return {
  "mason-org/mason.nvim",
  version = "^2",
  lazy = false,
  dependencies = {
    { "mason-org/mason-lspconfig.nvim", version = "^2" },
    "WhoIsSethDaniel/mason-tool-installer.nvim",
    -- mason-lspconfig resolves server names against nvim-lspconfig's `lsp/` configs
    -- and enables them, so lspconfig must be loaded (and configured) first.
    "neovim/nvim-lspconfig",
  },
  config = function()
    -- import mason
    local mason = require("mason")

    -- import mason-lspconfig
    local mason_lspconfig = require("mason-lspconfig")

    local mason_tool_installer = require("mason-tool-installer")

    -- enable mason and configure icons
    mason.setup({
      install_root_dir = mason_root,
      registries = { "lua:" .. override_registry, "github:mason-org/mason-registry" },
      -- a read-only (shared) root cannot cache a registry refresh
      registry_cache = { refresh = writable },
      ui = {
        icons = {
          package_installed = "✓",
          package_pending = "➜",
          package_uninstalled = "✗",
        },
      },
    })

    mason_lspconfig.setup({
      -- installation is driven by mason-tool-installer below (one list, one sync
      -- command); this plugin only has to turn the servers on
      ensure_installed = {},
      -- vim.lsp.enable() the servers above, but only those actually installed — a
      -- missing one stays disabled instead of failing to spawn on every buffer you
      -- open. Passing the list rather than `true` also keeps mason-lspconfig from
      -- enabling a *formatter* as a language server, when a package a user adds to a
      -- private MASON_ROOT shares its name with an nvim-lspconfig config.
      automatic_enable = servers,
    })

    mason_tool_installer.setup({
      -- server names are translated to mason package names via mason-lspconfig
      ensure_installed = vim.list_extend(vim.list_extend({}, servers), tools),
      -- Everything above is already installed in the shared read-only root, so don't
      -- spend startup (and network) re-checking it — and don't try to write there.
      -- With a private MASON_ROOT this behaves like stock mason-tool-installer.
      -- `:MasonToolsInstall` runs the check on demand either way.
      run_on_start = writable,
    })

    -- What the image build runs (`nvim --headless +MasonInstallSync! +qa`):
    -- mason-tool-installer's blocking install, then a check that every server and
    -- tool above really landed. With `!` a missing package exits Neovim non-zero, so
    -- a partial pre-install fails the image build instead of shipping a sandbox where
    -- Neovim quietly downloads servers the first time you open a file.
    vim.api.nvim_create_user_command("MasonInstallSync", function(cmd)
      -- pcall so that an install that throws (no network, a bad registry entry) still
      -- reaches the check below and is reported as the missing package it left behind,
      -- rather than aborting this command with a zero exit status.
      local ok, err = pcall(vim.cmd, "MasonToolsInstallSync")
      if not ok then
        vim.api.nvim_echo({ { "MasonInstallSync: " .. tostring(err), "ErrorMsg" } }, true, {})
      end

      local installed = {}
      for _, name in ipairs(require("mason-registry").get_installed_package_names()) do
        installed[name] = true
      end
      for _, name in ipairs(mason_lspconfig.get_installed_servers()) do
        installed[name] = true -- also by nvim-lspconfig name
      end

      local missing = {}
      for _, name in ipairs(vim.list_extend(vim.list_extend({}, servers), tools)) do
        if not installed[name] then
          table.insert(missing, name)
        end
      end
      if #missing > 0 then
        vim.api.nvim_echo(
          { { "MasonInstallSync: packages failed to install: " .. table.concat(missing, " "), "ErrorMsg" } },
          true,
          {}
        )
        if cmd.bang then
          vim.cmd("cquit 1")
        end
      end
    end, { bang = true, desc = "Install this config's LSP servers and tools, blocking (! = fail hard)" })
  end,
}
