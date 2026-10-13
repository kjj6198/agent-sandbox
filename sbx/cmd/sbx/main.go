// sbx is the command line client for sbxd. Run `sbx --help` for a guide.
package main

import (
	"os"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/cli"
)

func main() {
	os.Exit(cli.Main())
}
