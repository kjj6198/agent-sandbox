# Login shells reset PATH from /etc/profile; every sbx exec is a login shell.
export PATH="$HOME/.local/bin:$HOME/go/bin:/usr/local/go/bin:$HOME/.local/share/pnpm:$PATH"
export PNPM_HOME="$HOME/.local/share/pnpm"
export CHROME_PATH=/usr/bin/chromium
export PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium
