-- Replaces folke/neodev.nvim, which was archived in 2024. Same job — teach LuaLS about
-- the Neovim API and the plugins you `require` — but it loads the libraries lazily and
-- (unlike neodev) does not need Neovim's own type stubs, which ship with Nvim >= 0.10.
return {
  "folke/lazydev.nvim",
  ft = "lua",
  opts = {
    library = {
      -- Load luvit types when the `vim.uv` word is found
      { path = "${3rd}/luv/library", words = { "vim%.uv" } },
    },
  },
}
