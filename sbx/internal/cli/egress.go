package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

const egressHelp = "--allow limits the network to the hosts you name, enforced outside the VM: DNS " +
	"answers only for them, HTTP and HTTPS reach only them, and everything else is dropped. " +
	"\"*.example.com\" matches subdomains, not example.com. --offline allows nothing. " +
	"`sbx allow` adds hosts later, and `sbx inspect` lists what was blocked."

type egressFlags struct {
	allow   []string
	offline bool
}

func (f *egressFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringSliceVar(&f.allow, "allow", nil, "only reach these hosts over HTTP and HTTPS, such as github.com or *.npmjs.org")
	cmd.Flags().BoolVar(&f.offline, "offline", false, "no network at all")
}

// egress is nil when neither flag is set, which leaves the whole internet open.
func (f *egressFlags) egress() (*proto.Egress, error) {
	switch {
	case f.offline && len(f.allow) > 0:
		return nil, errors.New("--offline and --allow contradict each other")
	case f.offline:
		return &proto.Egress{Allow: []string{}}, nil
	case len(f.allow) > 0:
		return proto.NewEgress(f.allow)
	}
	return nil, nil
}

func allowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "allow ID HOST...",
		Short: "Let a sandbox created with --allow or --offline reach more hosts",
		Long: "Add hosts to a restricted sandbox's allowlist and print the result. It takes effect " +
			"for new connections at once. A sandbox created without --allow already reaches the " +
			"whole internet.\n\nThe allowlist guards against code inside the sandbox. Ask the " +
			"user before allowing a host the task did not call for.",
		Example: "  sbx allow $id pypi.org files.pythonhosted.org\n  sbx allow $id '*.githubusercontent.com'",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var sb proto.Sandbox
			if err := c.call(cmd.Context(), "GET", "/v1/sandboxes/"+args[0], nil, &sb); err != nil {
				return err
			}
			if sb.Egress == nil {
				return fmt.Errorf("sandbox %s already reaches the whole internet", sb.ID)
			}
			egress, err := proto.NewEgress(append(sb.Egress.Allow, args[1:]...))
			if err != nil {
				return err
			}
			if err := c.call(cmd.Context(), "PATCH", "/v1/sandboxes/"+sb.ID, proto.UpdateRequest{Egress: egress}, &sb); err != nil {
				return err
			}
			fmt.Println(strings.Join(sb.Egress.Allow, "\n"))
			return nil
		},
	}
}

// blockedNotice names the hosts a sandbox's allowlist refused since sbxd's time since, so an
// agent learns why a command failed instead of guessing at a network error.
func blockedNotice(ctx context.Context, c *client, id string, since time.Time) string {
	var sb proto.Sandbox
	if c.call(ctx, "GET", "/v1/sandboxes/"+id+"?passive=1", nil, &sb) != nil {
		return ""
	}
	var hosts []string
	for _, b := range sb.Blocked {
		if !b.At.Before(since) {
			hosts = append(hosts, b.Host)
		}
	}
	if len(hosts) == 0 {
		return ""
	}
	return fmt.Sprintf("the sandbox's egress allowlist blocked %s. If the task needs a host, ask the user "+
		"to allow it, then run: sbx allow %s HOST", strings.Join(hosts, ", "), id)
}
