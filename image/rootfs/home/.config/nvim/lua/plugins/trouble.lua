-- trouble.nvim v3. v3 is a full rewrite: the old `:TroubleToggle <mode>` commands are
-- gone in favour of `:Trouble <mode> <action>`, and the flat v2 options (fold_open,
-- signs, use_diagnostic_signs, indent_lines, icons = false) no longer exist.
--
-- `icons` is now a table; the values below keep this config's plain-ASCII look so the
-- list stays readable in a terminal without a patched Nerd Font.
--
-- Deliberately NOT pinned with `version = "^3"`: the latest tag (v3.7.1, Feb 2025)
-- predates trouble's Nvim 0.12 fixes — most importantly the decoration-provider
-- `on_line` -> `on_range` rename, which makes every Trouble window throw "attempt to
-- call a nil value" while rendering. lazy-lock.json pins the tested `main` commit.
return {
  "folke/trouble.nvim",
  dependencies = {
    -- "nvim-tree/nvim-web-devicons",
    "folke/todo-comments.nvim",
  },
  cmd = "Trouble",
  keys = {
    { "<leader>xx", "<cmd>Trouble diagnostics toggle<CR>", desc = "Open/close trouble list" },
    { "<leader>xw", "<cmd>Trouble diagnostics toggle<CR>", desc = "Open trouble workspace diagnostics" },
    { "<leader>xd", "<cmd>Trouble diagnostics toggle filter.buf=0<CR>", desc = "Open trouble document diagnostics" },
    { "<leader>xq", "<cmd>Trouble qflist toggle<CR>", desc = "Open trouble quickfix list" },
    { "<leader>xl", "<cmd>Trouble loclist toggle<CR>", desc = "Open trouble location list" },
    { "<leader>xt", "<cmd>TodoTrouble<CR>", desc = "Open todos in trouble" },
  },
  opts = {
    indent_guides = false, -- add an indent guide below the fold icons
    icons = {
      indent = {
        top = "| ",
        middle = "|-",
        last = "`-",
        fold_open = "v ",
        fold_closed = "> ",
        ws = "  ",
      },
      folder_closed = "> ",
      folder_open = "v ",
    },
  },
}
