package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kjj6198/agent-sandbox/internal/proto"
)

const envHelp = `An environment is a named set of variables and files stored on the sbx host. New
sandboxes start with the "base" environment when it exists, or the one --env-name names,
so secrets such as API tokens stay on the host instead of in every client. Values never
leave the host: these commands show only names and paths.

A sandbox keeps the environment it started with. Changes apply to sandboxes created later.
A command sees the template's defaults, overridden by the environment's variables, then by
"sbx create --env", then by "sbx exec --env".`

func envCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "env",
		Aliases: []string{"environment", "environments"},
		Short:   "Manage the stored variables and files sandboxes start with",
		Long:    envHelp,
	}
	cmd.AddCommand(envLsCmd(), envSetCmd(), envUnsetCmd(), envFileCmd(), envRmCmd())
	return cmd
}

func envLsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List environments with their variable names and file paths",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var list []proto.Environment
			if err := c.call(cmd.Context(), "GET", "/v1/environments", nil, &list); err != nil {
				return err
			}
			if asJSON {
				return printJSON(list)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tVARS\tFILES")
			for _, e := range list {
				var files []string
				for _, f := range e.Files {
					files = append(files, fmt.Sprintf("%s (%04o)", f.Path, f.Mode))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Name, strings.Join(e.Vars, ", "), strings.Join(files, ", "))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func updateEnv(cmd *cobra.Command, name string, u proto.EnvironmentUpdate) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.call(cmd.Context(), "PUT", "/v1/environments/"+name, u, nil)
}

func envSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set NAME KEY=VALUE...",
		Short: "Set variables, creating the environment if needed",
		Long:  "Set variables in environment NAME. A bare KEY copies the value from your own environment.",
		Example: "  sbx env set base CLAUDE_CODE_OAUTH_TOKEN GH_TOKEN\n" +
			"  sbx env set ci NODE_ENV=test",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := parseEnv(args[1:])
			if err != nil {
				return err
			}
			u := proto.EnvironmentUpdate{Vars: map[string]*string{}}
			for k, v := range env {
				u.Vars[k] = &v
			}
			return updateEnv(cmd, args[0], u)
		},
	}
}

func envUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "unset NAME KEY|PATH...",
		Short:   "Remove variables, or files given by their absolute guest path",
		Example: "  sbx env unset base GH_TOKEN /home/user/.config/gh/hosts.yml",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			u := proto.EnvironmentUpdate{Vars: map[string]*string{}, Files: map[string]*proto.EnvironmentFile{}}
			for _, k := range args[1:] {
				if strings.HasPrefix(k, "/") {
					u.Files[k] = nil
				} else {
					u.Vars[k] = nil
				}
			}
			return updateEnv(cmd, args[0], u)
		},
	}
}

func envFileCmd() *cobra.Command {
	var to, mode string
	cmd := &cobra.Command{
		Use:   "file NAME LOCAL-PATH --to GUEST-PATH",
		Short: "Store a file that sandboxes get at GUEST-PATH, owned by user",
		Example: "  sbx env file base ~/.codex/auth.json --to /home/user/.codex/auth.json\n" +
			"  sbx env file base ./id_ed25519 --to /home/user/.ssh/id_ed25519 --mode 0600",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return errors.New("--to is required")
			}
			m, err := strconv.ParseUint(mode, 8, 32)
			if err != nil || m > 0o777 {
				return fmt.Errorf("--mode %q: use octal permission bits such as 0600", mode)
			}
			content, err := os.ReadFile(args[1])
			if err != nil {
				return err
			}
			f := &proto.EnvironmentFile{Content: content, Mode: proto.FileMode(m)}
			return updateEnv(cmd, args[0], proto.EnvironmentUpdate{Files: map[string]*proto.EnvironmentFile{to: f}})
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "absolute path in the sandbox")
	cmd.Flags().StringVar(&mode, "mode", "0600", "permission bits in octal")
	return cmd
}

func envRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME...",
		Short: "Delete environments; running sandboxes keep what they started with",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var errs []error
			for _, name := range args {
				errs = append(errs, c.call(cmd.Context(), "DELETE", "/v1/environments/"+name, nil, nil))
			}
			return errors.Join(errs...)
		},
	}
}
