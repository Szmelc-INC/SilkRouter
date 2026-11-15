# home-manager module: programs.silkrouter
#
#   imports = [ silkrouter.homeManagerModules.default ];
#   programs.silkrouter = {
#     enable = true;
#     settings = { route.max_ms = 500; shell.p10k = "left"; };
#   };
#
# settings is written to ~/.config/szmelc/proxy/config.toml (read-only: use
# the module instead of `px config set` for the keys you manage here).
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.programs.silkrouter;
  toml = pkgs.formats.toml { };
  bin = "${cfg.package}/bin/silkrouter";
in
{
  options.programs.silkrouter = {
    enable = lib.mkEnableOption "SilkRouter (px / s-proxy / silkrouter)";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "silkrouter.packages.\${system}.default";
      description = "The SilkRouter package.";
    };

    settings = lib.mkOption {
      type = toml.type;
      default = { };
      example = lib.literalExpression ''
        {
          route = { max_ms = 500; prefer = "smart"; };
          scan = { workers = 48; rank = "clean"; };
          shell = { p10k = "left"; icon = "󰒍"; };
        }
      '';
      description = "config.toml contents (see `px config keys`). Empty = no file is managed.";
    };

    enableZshIntegration = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Load the shell integration (px on / off, command log, p10k segment) in zsh.";
    };
    enableBashIntegration = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Load the shell integration in bash.";
    };
    enableFishIntegration = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Load the shell integration in fish.";
    };
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];

    xdg.configFile."szmelc/proxy/config.toml" = lib.mkIf (cfg.settings != { }) {
      source = toml.generate "silkrouter-config.toml" cfg.settings;
    };

    programs.zsh.initContent = lib.mkIf cfg.enableZshIntegration (
      lib.mkOrder 1000 ''
        eval "$(${bin} shell-init zsh)"
      ''
    );
    programs.bash.initExtra = lib.mkIf cfg.enableBashIntegration ''
      eval "$(${bin} shell-init bash)"
    '';
    programs.fish.interactiveShellInit = lib.mkIf cfg.enableFishIntegration ''
      ${bin} shell-init fish | source
    '';
  };
}
