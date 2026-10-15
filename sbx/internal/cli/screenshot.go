package cli

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

func screenshotCmd() *cobra.Command {
	var format string
	var width int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "screenshot ID [FILE]",
		Short: "Save the sandbox's screen as an image file",
		Long: "Capture the sandbox's desktop as a PNG (or JPEG with --format jpeg) and write it " +
			"to FILE. Without FILE it writes sbx-ID.png in the current directory. FILE - writes " +
			"the image bytes to stdout, so another program can render or attach them directly, " +
			"for example as an image in a chat or a vision model call.\n\n" +
			"The same image is on the API as GET /v1/sandboxes/ID/screenshot, and a running " +
			"viewer serves it as screenshot.png under its URL, next to the live screen.",
		Example: "  sbx screenshot $id                  # ./sbx-$id.png, prints the path\n" +
			"  sbx screenshot $id screen.png\n" +
			"  sbx screenshot $id - > screen.png\n" +
			"  sbx screenshot --width 640 --json $id thumb.png",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			data, shot, err := fetchScreenshot(cmd.Context(), c, args[0], format, width, false)
			if err != nil {
				return err
			}
			file := ""
			if len(args) == 2 {
				file = args[1]
			} else {
				file = "sbx-" + args[0] + screenshotExt(shot.Format)
			}
			if file == "-" {
				if asJSON {
					return fmt.Errorf("--json cannot share stdout with the image; write to a file")
				}
				_, err := os.Stdout.Write(data)
				return err
			}
			if err := os.WriteFile(file, data, 0o644); err != nil {
				return err
			}
			shot.Path = file
			if asJSON {
				return printJSON(shot)
			}
			fmt.Println(file)
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "png", "image format: png or jpeg")
	cmd.Flags().IntVar(&width, "width", 0, "scale the image to this width in pixels (0: full size)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the image's path and size as JSON")
	return cmd
}

func screenshotExt(format string) string {
	if format == "jpeg" {
		return ".jpg"
	}
	return ".png"
}

// fetchScreenshot gets one desktop image through the API. passive keeps the request from
// counting as sandbox activity; the viewer passes it, the screenshot command does not.
func fetchScreenshot(ctx context.Context, c *client, id, format string, width int, passive bool) ([]byte, proto.Screenshot, error) {
	switch format {
	case "", "png":
		format = "png"
	case "jpg", "jpeg":
		format = "jpeg"
	default:
		return nil, proto.Screenshot{}, fmt.Errorf("--format %q must be png or jpeg", format)
	}
	q := url.Values{"format": {format}}
	if width > 0 {
		q.Set("width", strconv.Itoa(width))
	}
	if passive {
		q.Set("passive", "1")
	}
	data, header, err := c.getBytes(ctx, "/v1/sandboxes/"+id+"/screenshot?"+q.Encode())
	if err != nil {
		return nil, proto.Screenshot{}, err
	}
	shot := proto.Screenshot{ID: id, Format: format, Bytes: len(data)}
	shot.Width, _ = strconv.Atoi(header.Get("X-Screen-Width"))
	shot.Height, _ = strconv.Atoi(header.Get("X-Screen-Height"))
	if shot.Width == 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			shot.Width, shot.Height = cfg.Width, cfg.Height
		}
	}
	return data, shot, nil
}
