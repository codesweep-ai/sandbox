-- LSP.
--
-- nvim-lspconfig is now only a *collection of server configs* (its `lsp/` directory);
-- the old `require("lspconfig").<server>.setup{}` framework is deprecated upstream and
-- is being removed. Servers are therefore configured with Nvim's own `vim.lsp.config()`
-- and turned on with `vim.lsp.enable()` — the latter done by mason-lspconfig for every
-- server it actually has installed (see mason.lua), so a server that is missing is
-- simply not enabled instead of erroring on every buffer you open.
--
-- Loaded eagerly (`lazy = false`): the plugin only puts `lsp/*.lua` on 'runtimepath'
-- (nothing is executed), and mason-lspconfig must not enable a server before the
-- `vim.lsp.config()` calls below have registered its settings.
return {
  "neovim/nvim-lspconfig",
  version = "^2",
  lazy = false,
  dependencies = {
    "hrsh7th/cmp-nvim-lsp",
    { "antosha417/nvim-lsp-file-operations", config = true },
  },
  config = function()
    local keymap = vim.keymap -- for conciseness

    vim.api.nvim_create_autocmd("LspAttach", {
      group = vim.api.nvim_create_augroup("UserLspConfig", {}),
      callback = function(ev)
        -- Buffer local mappings.
        -- See `:help vim.lsp.*` for documentation on any of the below functions
        local opts = { buffer = ev.buf, silent = true }

        -- set keybinds
        opts.desc = "Show LSP references"
        keymap.set("n", "gR", "<cmd>Telescope lsp_references<CR>", opts) -- show definition, references

        opts.desc = "Go to declaration"
        keymap.set("n", "gD", vim.lsp.buf.declaration, opts) -- go to declaration

        opts.desc = "Show LSP definitions"
        keymap.set("n", "gd", "<cmd>Telescope lsp_definitions<CR>", opts) -- show lsp definitions

        opts.desc = "Show LSP implementations"
        keymap.set("n", "gi", "<cmd>Telescope lsp_implementations<CR>", opts) -- show lsp implementations

        opts.desc = "Show LSP type definitions"
        keymap.set("n", "gt", "<cmd>Telescope lsp_type_definitions<CR>", opts) -- show lsp type definitions

        opts.desc = "See available code actions"
        keymap.set({ "n", "v" }, "<leader>ca", vim.lsp.buf.code_action, opts) -- see available code actions, in visual mode will apply to selection

        opts.desc = "Smart rename"
        keymap.set("n", "<leader>rn", vim.lsp.buf.rename, opts) -- smart rename

        opts.desc = "Show buffer diagnostics"
        keymap.set("n", "<leader>D", "<cmd>Telescope diagnostics bufnr=0<CR>", opts) -- show  diagnostics for file

        opts.desc = "Show line diagnostics"
        keymap.set("n", "<leader>d", vim.diagnostic.open_float, opts) -- show diagnostics for line

        -- vim.diagnostic.goto_prev/goto_next were deprecated in Nvim 0.11
        opts.desc = "Go to previous diagnostic"
        keymap.set("n", "[d", function()
          vim.diagnostic.jump({ count = -1, float = true })
        end, opts) -- jump to previous diagnostic in buffer

        opts.desc = "Go to next diagnostic"
        keymap.set("n", "]d", function()
          vim.diagnostic.jump({ count = 1, float = true })
        end, opts) -- jump to next diagnostic in buffer

        opts.desc = "Show documentation for what is under cursor"
        keymap.set("n", "K", vim.lsp.buf.hover, opts) -- show documentation for what is under cursor

        -- Nvim 0.12 has its own :lsp command; nvim-lspconfig only defines :LspRestart
        -- (and friends) on 0.11 and older.
        opts.desc = "Restart LSP"
        keymap.set("n", "<leader>rs", function()
          vim.cmd(vim.fn.exists(":lsp") == 2 and "lsp restart" or "LspRestart")
        end, opts) -- mapping to restart lsp if necessary
      end,
    })

    -- Autocompletion capabilities for every server ("*" is the fallback config that
    -- vim.lsp.config merges into all of them).
    vim.lsp.config("*", {
      capabilities = require("cmp_nvim_lsp").default_capabilities(),
    })

    vim.lsp.config("svelte", {
      on_attach = function(client, _)
        vim.api.nvim_create_autocmd("BufWritePost", {
          pattern = { "*.js", "*.ts" },
          callback = function(ctx)
            -- Here use ctx.match instead of ctx.file
            client:notify("$/onDidChangeTsOrJsFile", { uri = ctx.match })
          end,
        })
      end,
    })

    vim.lsp.config("graphql", {
      filetypes = { "graphql", "gql", "svelte", "typescriptreact", "javascriptreact" },
    })

    vim.lsp.config("emmet_ls", {
      filetypes = { "html", "typescriptreact", "javascriptreact", "css", "sass", "scss", "less", "svelte" },
    })

    vim.lsp.config("lua_ls", {
      -- LuaLS defaults its log + metadata directories to its own install directory,
      -- which is the shared read-only Mason root in a sandbox (see mason.lua); left
      -- alone it exits 1 with "create_directories … Permission denied".
      cmd = {
        "lua-language-server",
        "--logpath=" .. vim.fn.stdpath("state") .. "/lua-language-server/log",
        "--metapath=" .. vim.fn.stdpath("state") .. "/lua-language-server/meta",
      },
      settings = {
        Lua = {
          -- make the language server recognize "vim" global
          diagnostics = {
            globals = { "vim" },
          },
          completion = {
            callSnippet = "Replace",
          },
        },
      },
    })

    -- Auto-detect and set PYENV_VERSION and the Python path from the Pyenv version
    local function find_pyenv_version_file(start_dir)
      local Path = require("plenary.path")

      local dir = Path:new(start_dir):absolute()
      while dir ~= "/" do
        local candidate = Path:new(dir) / ".python-version"
        if candidate:exists() then
          return candidate:read():gsub("%s+$", "")
        end
        dir = Path:new(dir):parent().filename
      end
      return nil
    end

    -- Pyenv is shared and lives under /opt in the sandbox (PYENV_ROOT), not in ~/.pyenv.
    local pyenv_root = vim.env.PYENV_ROOT
    if not pyenv_root or pyenv_root == "" then
      pyenv_root = vim.fn.expand("~/.pyenv")
    end

    local lsp_pyenv_version = find_pyenv_version_file(vim.fn.getcwd())
    if lsp_pyenv_version and lsp_pyenv_version ~= "" then
      -- print("Set LSP PYENV_VERSION to: " .. lsp_pyenv_version)
      vim.fn.setenv("PYENV_VERSION", lsp_pyenv_version)

      -- Also set the python host prog
      local lsp_python_path = vim.fn.system("pyenv prefix " .. lsp_pyenv_version .. " 2>/dev/null"):gsub("%s+$", "")
        .. "/bin/python"
      if vim.fn.executable(lsp_python_path) == 1 then
        -- print("Set LSP PYTHON_PATH to: " .. lsp_python_path)
        vim.g.python3_host_prog = lsp_python_path
      end
    end

    vim.lsp.config("basedpyright", {
      settings = {
        basedpyright = {
          typeCheckingMode = "standard",
          venvPath = pyenv_root .. "/versions",
          venv = lsp_pyenv_version,
          analysis = {
            exclude = { "**/log/**", "**/.cache/**" },
          },
        },
      },
      on_attach = function(client, bufnr)
        -- Auto-detect and set PYENV_VERSION and the Python path from .python-version
        local bufname = vim.api.nvim_buf_get_name(bufnr)
        local file_dir = vim.fn.fnamemodify(bufname, ":p:h")
        -- print("Set LSP on_attach file_dir to: " .. file_dir)
        local venv = find_pyenv_version_file(file_dir)
        if venv and venv ~= "" then
          -- print("Set LSP on_attach PYENV_VERSION to: " .. venv)
          vim.fn.setenv("PYENV_VERSION", venv)

          -- Also set the python host prog
          local python_path = vim.fn.system(
            'bash -lc "(cd ' .. file_dir .. " && pyenv prefix " .. venv .. ') 2>/dev/null"'
          ):gsub("%s+$", "") .. "/bin/python"
          if vim.fn.executable(python_path) == 1 then
            -- print("Set LSP on_attach PYTHON_PATH to: " .. python_path)
            vim.g.python3_host_prog = python_path
          end

          -- Update the basedpyright settings
          client.config.settings.basedpyright.venv = venv
          client.config.settings.basedpyright.venvPath = pyenv_root .. "/versions"
          client:notify("workspace/didChangeConfiguration", {
            settings = client.config.settings,
          })
        end
      end,
    })

    -- Configure how diagnostics are displayed
    vim.diagnostic.config({
      virtual_text = {
        prefix = "●", -- could be "■", "●", "▎", or ""
        spacing = 2, -- space between text and message
        source = "if_many", -- show source name (like [BasedPyright])
      },
      signs = true, -- show signs in the gutter
      underline = true, -- underline errors
      update_in_insert = false, -- don't update while typing
      severity_sort = true, -- sort by severity
    })
  end,
}
