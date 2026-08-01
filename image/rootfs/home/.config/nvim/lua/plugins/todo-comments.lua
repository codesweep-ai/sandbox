-- todo-comments needs its own setup() to build the keyword table that the highlighter
-- and the :TodoTelescope / :TodoTrouble pickers read; as a bare dependency of telescope
-- and trouble it was loaded but never configured.
return {
  "folke/todo-comments.nvim",
  event = { "BufReadPost", "BufNewFile" },
  dependencies = { "nvim-lua/plenary.nvim" },
  opts = {
    signs = false, -- the sign column is disabled in init.lua
  },
}
