package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

const rootHelp = `sbx gives you disposable Linux machines. Each sandbox is a Firecracker microVM restored
from a snapshot in tens of milliseconds, with its own kernel, disk, and network. Code inside
cannot reach the host or the LAN, only the internet.

The default template is Debian with Node.js, pnpm, Go, Python, uv, Chromium, ffmpeg, and CJK
fonts. Commands run as "user", who has passwordless sudo, in /home/user.

Typical use:

  id=$(sbx create)                       # prints only the sandbox ID
  sbx cp ./app $id:app                   # copy a directory in (relative paths are under ~)
  sbx exec $id -- pnpm -C app install    # exit code and output are the command's own
  sbx exec $id 'cd app && pnpm test'     # a single argument runs as a bash script
  sbx exec -d $id 'cd app && pnpm dev'   # start in the background, log in /tmp
  sbx expose $id 5173                    # prints a URL reachable from the LAN
  sbx cp $id:app/dist ./dist             # copy results out
  sbx rm $id

  sbx run -- python3 -c 'print(1+1)'     # create, run one command, remove

Browser: "sbx create browser" starts with Chromium already running, its DevTools
protocol on port 9222. Use "sbx forward $id 9222" and connect Playwright or Puppeteer to
http://127.0.0.1:9222 with connectOverCDP.

Sandboxes are destroyed after an idle period (default 1h, see "sbx ttl"); running commands and
open connections count as activity. Errors from sbx itself exit with 125.`

// exitError carries an exit code to main without printing anything more.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Main runs the CLI and returns the process exit code.
func Main() int {
	root := &cobra.Command{
		Use:           "sbx",
		Short:         "Disposable Firecracker sandboxes for AI agents",
		Long:          rootHelp,
		Version:       proto.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(createCmd(), execCmd(), runCmd(), cpCmd(), lsCmd(), inspectCmd(), rmCmd(),
		ttlCmd(), exposeCmd(), unexposeCmd(), forwardCmd(), snapshotCmd(), forkCmd(),
		templateCmd(), statusCmd(), loginCmd())
	err := root.Execute()
	var ee exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	default:
		fmt.Fprintln(os.Stderr, "sbx:", err)
		return 125
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func parseEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			// Like docker -e NAME: take the value from our own environment.
			v, ok = os.LookupEnv(k)
		}
		if k == "" || !ok {
			return nil, fmt.Errorf("--env %q: use NAME=VALUE", p)
		}
		env[k] = v
	}
	return env, nil
}

func parseTTL(s string) (*proto.Duration, error) {
	if s == "" {
		return nil, nil
	}
	d, err := proto.ParseDuration(s)
	if err != nil {
		return nil, fmt.Errorf("--ttl: %w", err)
	}
	pd := proto.Duration(d)
	return &pd, nil
}

func create(ctx context.Context, c *client, template, ttl string, env []string) (*proto.Sandbox, error) {
	req := proto.CreateRequest{Template: template}
	var err error
	if req.TTL, err = parseTTL(ttl); err != nil {
		return nil, err
	}
	if req.Env, err = parseEnv(env); err != nil {
		return nil, err
	}
	var sb proto.Sandbox
	return &sb, c.call(ctx, "POST", "/v1/sandboxes", req, &sb)
}

func createCmd() *cobra.Command {
	var ttl string
	var env []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "create [TEMPLATE]",
		Short: "Create a sandbox and print its ID",
		Long: "Create a sandbox from TEMPLATE (default \"default\") and print its ID.\n\n" +
			"--env sets variables for every command in the sandbox.",
		Example: "  id=$(sbx create)\n  id=$(sbx create browser --ttl 2h -e OPENAI_API_KEY)",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			sb, err := create(cmd.Context(), c, strings.Join(args, ""), ttl, env)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(sb)
			}
			fmt.Println(sb.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&ttl, "ttl", "", "idle timeout such as 30m or 2d; 0 never expires (default: daemon setting)")
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "NAME=VALUE, or NAME to copy it from your environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the sandbox as JSON")
	return cmd
}

func lsCmd() *cobra.Command {
	var asJSON, quiet bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list", "ps"},
		Short:   "List sandboxes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var list []proto.Sandbox
			if err := c.call(cmd.Context(), "GET", "/v1/sandboxes", nil, &list); err != nil {
				return err
			}
			switch {
			case asJSON:
				return printJSON(list)
			case quiet:
				for _, sb := range list {
					fmt.Println(sb.ID)
				}
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTEMPLATE\tAGE\tEXPIRES IN\tMEMORY\tPORTS")
			for _, sb := range list {
				var ports []string
				for _, p := range sb.Ports {
					ports = append(ports, fmt.Sprintf("%d->%s", p.Port, p.URL))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f MiB\t%s\n", sb.ID, sb.Template,
					human(time.Since(sb.CreatedAt)), expiresIn(sb.ExpiresAt), sb.MemoryUsedMiB, strings.Join(ports, ", "))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only IDs")
	return cmd
}

func expiresIn(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return human(time.Until(*t))
}

func human(d time.Duration) string {
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func inspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect ID",
		Short: "Print a sandbox as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var sb proto.Sandbox
			if err := c.call(cmd.Context(), "GET", "/v1/sandboxes/"+args[0], nil, &sb); err != nil {
				return err
			}
			return printJSON(sb)
		},
	}
}

func rmCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "rm ID...",
		Aliases: []string{"kill", "destroy"},
		Short:   "Destroy sandboxes",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			if all {
				var list []proto.Sandbox
				if err := c.call(cmd.Context(), "GET", "/v1/sandboxes", nil, &list); err != nil {
					return err
				}
				for _, sb := range list {
					args = append(args, sb.ID)
				}
			} else if len(args) == 0 {
				return errors.New("name at least one sandbox, or use --all")
			}
			var errs []error
			for _, id := range args {
				if err := c.call(cmd.Context(), "DELETE", "/v1/sandboxes/"+id, nil, nil); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "destroy every sandbox")
	return cmd
}

func ttlCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ttl ID DURATION",
		Short: "Change a sandbox's idle timeout",
		Long: "Destroy the sandbox after DURATION without activity, counted from now. " +
			"0 keeps it until `sbx rm`.",
		Example: "  sbx ttl $id 4h\n  sbx ttl $id 0",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			ttl, err := parseTTL(args[1])
			if err != nil {
				return err
			}
			var sb proto.Sandbox
			if err := c.call(cmd.Context(), "PATCH", "/v1/sandboxes/"+args[0], proto.UpdateRequest{TTL: ttl}, &sb); err != nil {
				return err
			}
			fmt.Println("expires in", expiresIn(sb.ExpiresAt))
			return nil
		},
	}
}

func exposeCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "expose ID PORT",
		Short: "Publish a port of the sandbox and print its URL",
		Long: "Publish a TCP port of the sandbox on the sbx host and print a URL that " +
			"anyone on the LAN can open. The server inside may listen on localhost.",
		Example: "  sbx exec -d $id 'cd app && pnpm dev --port 5173'\n  sbx expose $id 5173",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			port, err := parsePort(args[1])
			if err != nil {
				return err
			}
			var p proto.Port
			if err := c.call(cmd.Context(), "POST", "/v1/sandboxes/"+args[0]+"/ports", proto.ExposeRequest{Port: port}, &p); err != nil {
				return err
			}
			if asJSON {
				return printJSON(p)
			}
			fmt.Println(p.URL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func unexposeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unexpose ID PORT",
		Short: "Stop publishing a port",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			return c.call(cmd.Context(), "DELETE", "/v1/sandboxes/"+args[0]+"/ports/"+args[1], nil, nil)
		},
	}
}

func snapshotCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snapshot ID NAME",
		Short: "Save a running sandbox as a new template",
		Long: "Save the sandbox's memory, processes, and files as template NAME. The sandbox " +
			"pauses for about a second per GiB of memory and keeps running. New sandboxes " +
			"created from NAME start exactly where this one was.",
		Example: "  sbx exec $id 'git clone https://github.com/org/repo && cd repo && pnpm install'\n" +
			"  sbx snapshot $id repo-ready\n  sbx create repo-ready",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var tpl proto.Template
			if err := c.call(cmd.Context(), "POST", "/v1/sandboxes/"+args[0]+"/snapshot", proto.SnapshotRequest{Name: args[1]}, &tpl); err != nil {
				return err
			}
			fmt.Println(tpl.Name)
			return nil
		},
	}
}

func forkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fork ID",
		Short: "Clone a running sandbox and print the clone's ID",
		Long: "Clone the sandbox with its memory, running processes, and files. Use it to try " +
			"several approaches from the same state.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var sb proto.Sandbox
			if err := c.call(cmd.Context(), "POST", "/v1/sandboxes/"+args[0]+"/fork", nil, &sb); err != nil {
				return err
			}
			fmt.Println(sb.ID)
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the sbx host's capacity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var st proto.Status
			if err := c.call(cmd.Context(), "GET", "/v1/status", nil, &st); err != nil {
				return err
			}
			if asJSON {
				return printJSON(st)
			}
			fmt.Printf("sbxd %s at %s, %s\n", st.Version, c.base, st.Firecracker)
			fmt.Printf("sandboxes  %d of %d\n", st.Sandboxes, st.MaxSandboxes)
			fmt.Printf("memory     %d MiB available of %d MiB\n", st.MemAvailMiB, st.MemTotalMiB)
			fmt.Printf("disk       %d GiB free\n", st.DiskFreeGiB)
			fmt.Printf("templates  %d\n", st.Templates)
			fmt.Printf("idle TTL   %s by default\n", time.Duration(st.DefaultTTL))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func loginCmd() *cobra.Command {
	var token string
	cmd := &cobra.Command{
		Use:     "login URL",
		Short:   "Save the sbxd address and API token",
		Long:    "Save the address and token to " + configPath() + ". The token is in /etc/sbx/token on the sbx host; without --token it is read from stdin.",
		Example: "  ssh sbx-host sudo cat /etc/sbx/token | sbx login http://192.168.1.33:7070",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				token = strings.TrimSpace(line)
			}
			if token == "" {
				return errors.New("no token given")
			}
			c := &client{base: strings.TrimSuffix(args[0], "/"), token: token, hc: &http.Client{}}
			var st proto.Status
			if err := c.call(cmd.Context(), "GET", "/v1/status", nil, &st); err != nil {
				return err
			}
			b, _ := json.MarshalIndent(clientConfig{URL: c.base, Token: token}, "", "  ")
			if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(configPath(), b, 0o600); err != nil {
				return err
			}
			fmt.Printf("logged in to sbxd %s at %s\n", st.Version, c.base)
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "API token (default: read from stdin)")
	return cmd
}
