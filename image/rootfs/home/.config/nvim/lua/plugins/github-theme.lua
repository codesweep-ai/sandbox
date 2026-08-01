-- The colorscheme is selected (and `setup()` called with its options) at the bottom of
-- init.lua, so this spec only has to make the plugin available before anything else
-- draws — no second setup() here, which would compile every theme variant twice.
return {
  'projekt0n/github-nvim-theme',
  lazy = false,
  priority = 1000,
}
