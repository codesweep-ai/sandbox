return {
"nvim-treesitter/nvim-treesitter",
  cmd = "TSUpdate",
  event = { "BufReadPost", "BufNewFile" },
  config = function()
    require("nvim-treesitter").setup({
      -- A list of parser names, or "all"
      ensure_installed = {
        "c",
        "cpp",
        "lua",
        "go",
        "bash",
        "python",
        "html",
        "javascript",
        "typescript",
        "java",
        "scala",
        "json",
        "nix",
        "rust",
        "yaml",
        "css",
        "markdown",
        "markdown_inline",
      },
      sync_install = false,
      highlight = {
        enable = true,
        additional_vim_regex_highlighting = false,
        disable = function(_, buf)
          return vim.b[buf].large_file or false
        end
      },
      indent = {
        enable = true,
        disable = function(_, buf)
          return vim.b[buf].large_file or false
        end
      }
    })
  end,
}
