//go:build linux

// sbx-agent runs inside every sandbox: as PID 1 (the kernel's init=) it builds the root
// filesystem, and `sbx-agent serve` answers sbxd over vsock.
package main

import (
	"fmt"
	"os"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/agent"
	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

func main() {
	if os.Getpid() == 1 {
		agent.RunInit()
	}
	if len(os.Args) == 2 && os.Args[1] == "serve" {
		agent.RunServer()
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(proto.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: sbx-agent serve | --version (as PID 1 it initializes the guest)")
	os.Exit(2)
}
