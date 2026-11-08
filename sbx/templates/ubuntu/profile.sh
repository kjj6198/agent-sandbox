# Added to the default template's profile; every sbx exec is a login shell.
export RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
export PATH="/usr/local/cargo/bin:/opt/kotlinc/bin:/opt/gradle/bin:/opt/scala3/bin:/opt/sbt/bin:$HOME/.dotnet/tools:$PATH"
export LANG=en_US.UTF-8
export DOTNET_CLI_TELEMETRY_OPTOUT=1
# hinacode is pinned in the template; updating itself would make sandboxes differ.
export HINACODE_SERVE_AUTO_UPDATE=0
