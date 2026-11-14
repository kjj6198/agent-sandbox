#!/bin/sh
# Starts sbxd, and builds the default and browser templates in the background on first start.
set -eu

# sbxd hard-links the kernel into each jail, so it has to live on the data volume.
mkdir -p /var/lib/sbx/kernel
cp -a /usr/local/lib/sbx/kernel/. /var/lib/sbx/kernel/

/usr/local/lib/sbx/install.sh templates &
exec "$@"
