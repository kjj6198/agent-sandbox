//go:build linux

package agent

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// screenshot captures the desktop on display :0 as one image, so callers outside the
// sandbox (an agent's vision model, a web page, a chat attachment) can render the screen
// without a VNC connection. It runs ImageMagick's import as the desktop's user, because
// that user owns the X session. Query: format=png|jpeg (default png), width=<px> scales
// down keeping the aspect ratio. The answer carries X-Screen-Width and X-Screen-Height
// of the returned image.
func (s *server) screenshot(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	switch format {
	case "":
		format = "png"
	case "jpg":
		format = "jpeg"
	}
	if format != "png" && format != "jpeg" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("format %q must be png or jpeg", format))
		return
	}
	width := 0
	if v := r.URL.Query().Get("width"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 16 || n > 3840 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("width %q must be 16-3840", v))
			return
		}
		width = n
	}
	if _, err := os.Stat("/tmp/.X11-unix/X0"); err != nil {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Errorf("no desktop on display :0; run sbx-desktop in the sandbox first"))
		return
	}
	importBin, err := exec.LookPath("import")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Errorf("screenshots need ImageMagick (the `import` command) in the template"))
		return
	}
	u, err := lookupUser("")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	capture := exec.CommandContext(ctx, importBin, "-window", "root", "-silent", "png:-")
	capture.Dir = u.home
	capture.Env = []string{"DISPLAY=:0", "HOME=" + u.home, "PATH=" + defaultPath}
	capture.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: u.groups},
	}
	var out, errBuf bytes.Buffer
	capture.Stdout = &out
	capture.Stderr = &errBuf
	if err := capture.Run(); err != nil {
		writeError(w, http.StatusBadGateway,
			fmt.Errorf("capture display :0: %v: %s", err, strings.TrimSpace(errBuf.String())))
		return
	}
	// import ignores -resize, so scaling and JPEG encoding go through convert, which
	// ships in the same ImageMagick package.
	if width > 0 || format == "jpeg" {
		args := []string{"png:-"}
		if width > 0 {
			args = append(args, "-resize", strconv.Itoa(width)+"x")
		}
		if format == "jpeg" {
			args = append(args, "-quality", "82")
		}
		args = append(args, format+":-")
		conv := exec.CommandContext(ctx, "convert", args...)
		conv.Stdin = bytes.NewReader(out.Bytes())
		var converted bytes.Buffer
		conv.Stdout = &converted
		conv.Stderr = &errBuf
		if err := conv.Run(); err != nil {
			writeError(w, http.StatusBadGateway,
				fmt.Errorf("encode screenshot: %v: %s", err, strings.TrimSpace(errBuf.String())))
			return
		}
		out = converted
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out.Bytes()))
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("capture display :0: unreadable image: %w", err))
		return
	}
	contentType := "image/png"
	if format == "jpeg" {
		contentType = "image/jpeg"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Screen-Width", strconv.Itoa(cfg.Width))
	w.Header().Set("X-Screen-Height", strconv.Itoa(cfg.Height))
	w.Header().Set("Content-Length", strconv.Itoa(out.Len()))
	w.Write(out.Bytes())
}
