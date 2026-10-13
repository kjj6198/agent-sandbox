package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

// splitRemote parses ID:PATH. A colon after a slash belongs to a local path, as in docker cp.
func splitRemote(arg string) (id, path string, ok bool) {
	id, path, ok = strings.Cut(arg, ":")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	return id, path, true
}

func cpCmd() *cobra.Command {
	var user string
	cmd := &cobra.Command{
		Use:   "cp SRC DST",
		Short: "Copy files or directories between your machine and a sandbox",
		Long: "Copy a file or directory with `cp -r` semantics: if DST is an existing directory, " +
			"SRC is copied into it, otherwise SRC is copied to DST. Exactly one side is ID:PATH. " +
			"Relative sandbox paths are under the user's home, and files land owned by that user.",
		Example: "  sbx cp ./app $id:app\n  sbx cp report.csv $id:/tmp/\n  sbx cp $id:app/dist ./dist",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			srcID, srcPath, srcRemote := splitRemote(args[0])
			dstID, dstPath, dstRemote := splitRemote(args[1])
			q := url.Values{"user": {user}}
			switch {
			case srcRemote == dstRemote:
				return errors.New("exactly one of SRC and DST must be ID:PATH")
			case dstRemote:
				q.Set("path", dstPath)
				pr, pw := io.Pipe()
				go func() { pw.CloseWithError(proto.WriteTar(pw, args[0])) }()
				req, err := c.request(cmd.Context(), "PUT", "/v1/sandboxes/"+dstID+"/files?"+q.Encode(), pr)
				if err != nil {
					return err
				}
				req.Header.Set("Content-Type", "application/x-tar")
				resp, err := c.send(req)
				if err != nil {
					return err
				}
				return resp.Body.Close()
			default:
				q.Set("path", srcPath)
				req, err := c.request(cmd.Context(), "GET", "/v1/sandboxes/"+srcID+"/files?"+q.Encode(), nil)
				if err != nil {
					return err
				}
				resp, err := c.send(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				return proto.ReadTar(resp.Body, args[1], nil)
			}
		},
	}
	cmd.Flags().StringVarP(&user, "user", "u", "", `sandbox user that resolves relative paths and owns copied files (default "user")`)
	return cmd
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %q must be 1-65535", s)
	}
	return p, nil
}

func forwardCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "forward ID PORT [LOCAL_PORT]",
		Short: "Forward a local port to a port inside the sandbox",
		Long: "Listen on 127.0.0.1:LOCAL_PORT (default PORT) and tunnel every connection to PORT " +
			"inside the sandbox through the sbx API. Runs until interrupted. Unlike expose, " +
			"nothing is published on the LAN.",
		Example: "  sbx forward $id 9222 &   # then connectOverCDP('http://127.0.0.1:9222')\n" +
			"  sbx forward $id 5432 15432",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			port, err := parsePort(args[1])
			if err != nil {
				return err
			}
			local := port
			if len(args) == 3 {
				if local, err = parsePort(args[2]); err != nil {
					return err
				}
			}
			// Check that the sandbox exists before reporting the tunnel as up.
			var sb proto.Sandbox
			if err := c.call(cmd.Context(), "GET", "/v1/sandboxes/"+args[0], nil, &sb); err != nil {
				return err
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(local)))
			if err != nil {
				return err
			}
			go func() {
				sig := make(chan os.Signal, 1)
				signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
				<-sig
				ln.Close()
			}()
			fmt.Fprintf(os.Stderr, "forwarding %s to port %d of %s\n", ln.Addr(), port, args[0])
			path := fmt.Sprintf("/v1/sandboxes/%s/tcp?port=%d", args[0], port)
			for {
				conn, err := ln.Accept()
				if err != nil {
					return nil
				}
				go func() {
					defer conn.Close()
					remote, err := c.upgrade(cmd.Context(), "GET", path, "tcp", nil)
					if err != nil {
						fmt.Fprintln(os.Stderr, "sbx:", err)
						return
					}
					defer remote.Close()
					done := make(chan struct{}, 2)
					go func() { io.Copy(remote, conn); done <- struct{}{} }()
					go func() { io.Copy(conn, remote); done <- struct{}{} }()
					<-done
				}()
			}
		},
	}
	cmd.Flags().StringVar(&addr, "address", "127.0.0.1", "local address to listen on")
	return cmd
}
