package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

func templateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "template",
		Aliases: []string{"templates", "tpl"},
		Short:   "Manage the templates sandboxes start from",
	}
	cmd.AddCommand(templateLsCmd(), templateBuildCmd(), templateRmCmd())
	return cmd
}

func templateLsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List templates",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var list []proto.Template
			if err := c.call(cmd.Context(), "GET", "/v1/templates", nil, &list); err != nil {
				return err
			}
			if asJSON {
				return printJSON(list)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tCPUS\tMEMORY\tDISK\tPARENT\tCREATED\tSIZE")
			for _, t := range list {
				fmt.Fprintf(tw, "%s\t%d\t%d MiB\t%d GiB\t%s\t%s ago\t%.1f GiB\n", t.Name, t.CPUs, t.MemoryMiB,
					t.DiskGiB, t.Parent, human(time.Since(t.CreatedAt)), float64(t.DiskBytes)/(1<<30))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func templateBuildCmd() *cobra.Command {
	var cpus, memory, disk int
	var warmup string
	cmd := &cobra.Command{
		Use:   "build NAME DIR",
		Short: "Build a template from a directory with a Dockerfile",
		Long: "Upload DIR as a build context. The sbx host builds the Dockerfile with podman, turns " +
			"the image into a disk, boots it, runs --warmup, and snapshots the running VM. " +
			"Rebuilding a name replaces the template; running sandboxes are not affected.\n\n" +
			"Any Debian or Ubuntu based image works. Add a user named \"user\" to run commands " +
			"as someone other than root.",
		Example: "  sbx template build default templates/default\n" +
			"  sbx template build browser templates/default --warmup sbx-browser",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			if fi, err := os.Stat(args[1]); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", args[1])
			}
			q := url.Values{
				"cpus": {strconv.Itoa(cpus)}, "memory_mib": {strconv.Itoa(memory)},
				"disk_gib": {strconv.Itoa(disk)}, "warmup": {warmup},
			}
			pr, pw := io.Pipe()
			go func() { pw.CloseWithError(proto.WriteTar(pw, args[1])) }()
			req, err := c.request(cmd.Context(), "POST", "/v1/templates/"+args[0]+"?"+q.Encode(), pr)
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/x-tar")
			resp, err := c.send(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			res, err := readFrames(resp.Body, os.Stderr, os.Stderr)
			if err != nil {
				return err
			}
			if res.Error != "" {
				return errors.New(res.Error)
			}
			fmt.Println(args[0])
			return nil
		},
	}
	cmd.Flags().IntVar(&cpus, "cpus", 2, "vCPUs of every sandbox from this template (1 or even)")
	cmd.Flags().IntVar(&memory, "memory", 2048, "memory in MiB; sandboxes only use what they touch")
	cmd.Flags().IntVar(&disk, "disk", 16, "writable disk in GiB; sparse, so it costs only what is written")
	cmd.Flags().StringVar(&warmup, "warmup", "", "script to run in the booted VM before the snapshot, e.g. to start a server")
	return cmd
}

func templateRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME...",
		Short: "Delete templates; sandboxes created from them keep running",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			var errs []error
			for _, name := range args {
				errs = append(errs, c.call(cmd.Context(), "DELETE", "/v1/templates/"+name, nil, nil))
			}
			return errors.Join(errs...)
		},
	}
}
