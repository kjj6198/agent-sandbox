# Login shells reset PATH from /etc/profile; every sbx exec is a login shell.
export PATH="$HOME/.local/bin:$HOME/go/bin:/usr/local/go/bin:$HOME/.local/share/pnpm:$PATH"
export PNPM_HOME="$HOME/.local/share/pnpm"
export CHROME_PATH=/usr/bin/chromium
export PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium
# The desktop that `sbx view` shows; GUI programs open on it.
export DISPLAY=:0
# fcitx5, which sbx-desktop starts, is the desktop's input method for Japanese and Chinese.
export GTK_IM_MODULE=fcitx QT_IM_MODULE=fcitx XMODIFIERS=@im=fcitx
export DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/dbus-session
