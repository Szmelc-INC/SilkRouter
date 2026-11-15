{
  lib,
  buildGoModule,
  go,
  go_1_26 ? null,
  installShellFiles,
  stdenv,
  version,
  src,
}:

let
  # go.mod requires Go >= 1.26.
  goPkg = if go_1_26 != null then go_1_26 else go;
in
(buildGoModule.override { go = goPkg; }) {
  pname = "silkrouter";
  inherit version src;

  # Hash of the vendored modules. After changing go.mod / go.sum run
  # `make nix-hash` (or `nix build` and copy the "got:" hash) and paste it.
  vendorHash = "sha256-+U5X9tvdWiIJrYkWbVzfE0aiiEGLaJEJ0I8HO9yL6JI=";

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  nativeBuildInputs = [ installShellFiles ];

  # The tests sandbox their config home and temp dirs themselves; they only
  # talk to local fake proxies.
  preCheck = ''
    export HOME="$TMPDIR"
  '';

  postInstall = lib.optionalString (stdenv.buildPlatform.canExecute stdenv.hostPlatform) ''
    export HOME="$TMPDIR" SILKROUTER_HOME="$TMPDIR/silkrouter-home" SILKROUTER_CONFIG= NO_COLOR=1
    bin="$out/bin/silkrouter"
    ln -s silkrouter "$out/bin/px"
    ln -s silkrouter "$out/bin/s-proxy"

    installShellCompletion --cmd silkrouter \
      --zsh <("$bin" completion zsh) \
      --bash <("$bin" completion bash) \
      --fish <("$bin" completion fish)
    for n in px s-proxy; do
      installShellCompletion --cmd "$n" \
        --bash <("$bin" completion bash) \
        --fish <("$bin" completion fish)
    done

    mkdir -p "$out/share/silkrouter/shell"
    for sh in zsh bash fish; do
      "$bin" shell-init "$sh" --bin "$bin" > "$out/share/silkrouter/shell/init.$sh"
    done
    "$bin" -q config init --force
    cp "$SILKROUTER_HOME/config.toml" "$out/share/silkrouter/config.toml.example"

    install -Dm644 README.md docs/ARCHITECTURE-v4.md -t "$out/share/doc/silkrouter"
    if [ -f docs/TUI.md ]; then install -Dm644 docs/TUI.md -t "$out/share/doc/silkrouter"; fi
  '';

  meta = {
    description = "Route any command through fast, scanned and ranked proxies: SQLite proxy database, full scans, usage log, shell integration and TUI";
    homepage = "https://github.com/Szmelc-INC/SilkRouter";
    license = lib.licenses.mit;
    mainProgram = "silkrouter";
    platforms = lib.platforms.unix;
  };
}
