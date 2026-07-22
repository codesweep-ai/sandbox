-- colors
if false then
  vim.cmd("highlight Normal ctermbg=white guibg=white")
  vim.cmd("highlight StatusLine ctermfg=black ctermbg=white guifg=black guibg=white")
  vim.cmd("highlight Cmdline ctermfg=black ctermbg=white guifg=black guibg=white")
  vim.cmd("highlight Search cterm=NONE ctermfg=NONE ctermbg=lightgrey guibg=lightgrey")
end

-- query terminal background
vim.opt.termguicolors = true
local theme = os.getenv("THEME")
if theme == "dark" then
  vim.o.background = "dark"
else
  vim.o.background = "light"
end

-- color column
vim.opt.colorcolumn = "100"
vim.cmd("highlight ColorColumn ctermbg=lightgrey guibg=lightgrey")

-- switch to current file"s directory
vim.opt.autochdir = true

-- display current command
vim.opt.showcmd = true

-- short messages
vim.opt.shortmess = "atI"

-- compact status bar
vim.opt.laststatus = 0
-- vim.o.laststatus = 2
-- vim.o.statusline = "%F"

-- cycle buffers without writing
vim.opt.hidden = true

-- backup while writing
vim.opt.writebackup = true

-- expand tabs
vim.opt.expandtab = true
vim.opt.smarttab = true
vim.opt.smartindent = true
vim.opt.autoindent = true
vim.opt.filetype = "on"
vim.opt.tabstop = 2
vim.opt.softtabstop = 2
vim.opt.shiftwidth = 2
vim.opt.shiftround = true

-- indent folding
vim.opt.foldmethod = "indent"
vim.opt.foldlevelstart = 99

-- line numbers
vim.opt.number = true

-- disable mouse
vim.opt.mouse = ""

-- sign column
vim.opt.signcolumn = "no"
vim.cmd("highlight! link SignColumn LineNr")

-- system clipboard
if false then
  vim.opt.clipboard = ""

  -- don't yank to system clipboard on x and s
  vim.keymap.set({'n', 'v'}, 'x', '"_x')
  vim.keymap.set({'n', 'v'}, 's', '"_s')
end

-- don't highlight matching parentheses
if false then
  vim.g.loaded_matchparen = 1
end

-- mark large files and turn syntax off
local large_file_threshold = 10000 * 1024

if false then
  vim.api.nvim_create_autocmd("BufReadPre", {
    pattern = { "*.json", "*.yaml", "*.yml" },
    callback = function(args)
      local stat = vim.loop.fs_stat(args.file)
      if stat and stat.size > large_file_threshold then
        vim.b.large_file = true
        vim.cmd("syntax off")
        vim.cmd("filetype off")
      end
    end
  })
end

-- Use nvim.lazy for plugins
local lazypath = vim.fn.stdpath("data") .. "/lazy/lazy.nvim"
if not (vim.uv or vim.loop).fs_stat(lazypath) then
  vim.fn.system({
    "git",
    "clone",
    "--filter=blob:none",
    "https://github.com/folke/lazy.nvim.git",
    "--branch=stable", -- latest stable release
    lazypath,
  })
end
vim.opt.rtp:prepend(lazypath)

require("lazy").setup({{import = "plugins"}})

-- github theme
if true then
  require('github-theme').setup({
    -- theme specific configuration options
    options = {
      terminal_colors = true,
      styles = {
        comments = 'italic',
        keywords = 'bold',
        types = 'italic,bold',
      }
    },
  })
  if vim.o.background == "dark" then
    vim.cmd('colorscheme github_dark_default')
  else
    vim.cmd('colorscheme github_light_default')
  end
end

-- adwaita theme
if false then
  vim.g.adwaita_darker = false -- for darker version
  vim.g.adwaita_disable_cursorline = true -- to disable cursorline
  vim.g.adwaita_transparent = false -- makes the background transparent
  vim.cmd([[colorscheme adwaita]])
end

vim.api.nvim_create_autocmd('BufRead', {
    callback = function(opts)
    vim.api.nvim_create_autocmd('BufWinEnter', {
      once = true,
      buffer = opts.buf,
      callback = function()
        local ft = vim.bo[opts.buf].filetype
        local last_known_line = vim.api.nvim_buf_get_mark(opts.buf, '"')[1]
        if
          not (ft:match 'commit' and ft:match 'rebase')
          and last_known_line > 1
          and last_known_line <= vim.api.nvim_buf_line_count(opts.buf)
        then
            vim.api.nvim_feedkeys([[g`"]], 'nx', false)
        end
      end,
    })
  end,
})

vim.api.nvim_create_autocmd({"BufRead", "BufNewFile"}, {
    pattern = "Makefile.*",
    callback = function() vim.bo.filetype = "make" end
})

if false then
  vim.api.nvim_create_autocmd("FileType", {
    pattern = "json",
    callback = function()
      vim.opt_local.foldmethod = "expr"
      vim.opt_local.foldexpr = "nvim_treesitter#foldexpr()"
    end,
  })

  vim.api.nvim_create_autocmd("FileType", {
    pattern = "yaml",
    callback = function()
      vim.opt_local.foldmethod = "expr"
      vim.opt_local.foldexpr = "nvim_treesitter#foldexpr()"
    end,
  })
end

