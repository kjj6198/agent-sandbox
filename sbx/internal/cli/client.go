// Package cli is the sbx command line client.
package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

type client struct {
	base  string
	token string
	hc    *http.Client
}

type clientConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "sbx", "config.json")
}

// newClient reads SBX_URL and SBX_TOKEN, then the file `sbx login` writes, then, on the sbx
// host itself, the daemon's token file.
func newClient() (*client, error) {
	cfg := clientConfig{URL: os.Getenv("SBX_URL"), Token: os.Getenv("SBX_TOKEN")}
	if cfg.URL == "" || cfg.Token == "" {
		var saved clientConfig
		if b, err := os.ReadFile(configPath()); err == nil {
			json.Unmarshal(b, &saved)
		}
		cfg.URL = cmp.Or(cfg.URL, saved.URL)
		cfg.Token = cmp.Or(cfg.Token, saved.Token)
	}
	cfg.URL = cmp.Or(cfg.URL, "http://127.0.0.1:7070")
	if cfg.Token == "" {
		if b, err := os.ReadFile("/etc/sbx/token"); err == nil {
			cfg.Token = strings.TrimSpace(string(b))
		}
	}
	if cfg.Token == "" {
		return nil, errors.New("no API token: run `sbx login URL` or set SBX_URL and SBX_TOKEN")
	}
	return &client{base: strings.TrimSuffix(cfg.URL, "/"), token: cfg.Token, hc: &http.Client{}}, nil
}

func (c *client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

// send performs req and turns an error status into an error carrying the daemon's message.
func (c *client) send(req *http.Request) (*http.Response, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach sbxd at %s: %w", c.base, err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		var e proto.Error
		b, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(resp.Status + " " + string(b))
		}
		return nil, errors.New(e.Error)
	}
	return resp, nil
}

// call sends in as JSON and decodes the answer into out; either may be nil.
func (c *client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.send(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// upgrade sends a request that switches the connection to a raw byte stream.
func (c *client) upgrade(ctx context.Context, method, path, protocol string, body []byte) (io.ReadWriteCloser, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := c.request(ctx, method, path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", protocol)
	resp, err := c.send(req)
	if err != nil {
		return nil, err
	}
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if resp.StatusCode != http.StatusSwitchingProtocols || !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("expected a protocol switch, got %s", resp.Status)
	}
	return rw, nil
}

// readFrames copies output frames to stdout and stderr until the exit frame.
func readFrames(r io.Reader, stdout, stderr io.Writer) (proto.ExecResult, error) {
	for {
		typ, p, err := proto.ReadFrame(r)
		if err != nil {
			return proto.ExecResult{}, fmt.Errorf("connection to the sandbox ended early: %w", err)
		}
		switch typ {
		case proto.FrameStdout:
			stdout.Write(p)
		case proto.FrameStderr:
			stderr.Write(p)
		case proto.FrameExit:
			var res proto.ExecResult
			return res, json.Unmarshal(p, &res)
		}
	}
}
