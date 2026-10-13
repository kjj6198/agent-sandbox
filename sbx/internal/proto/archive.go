package proto

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// WriteTar archives src, a file or a directory, into w without following symlinks. The
// archive has exactly one top-level entry, named after src's base name.
func WriteTar(w io.Writer, src string) error {
	src = filepath.Clean(src)
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	parent := filepath.Dir(src)
	add := func(p string, info fs.FileInfo) error {
		var link string
		switch {
		case info.Mode().IsRegular(), info.IsDir():
		case info.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		default:
			return nil // sockets, devices, and pipes have no place in a copy
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(parent, p)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	}
	if info.IsDir() {
		err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			return add(p, info)
		})
	} else {
		err = add(src, info)
	}
	if err != nil {
		return err
	}
	return tw.Close()
}

// TarOwner, when non-nil, owns every extracted entry and every directory ReadTar creates.
type TarOwner struct{ UID, GID int }

// ReadTar extracts an archive made by WriteTar with `cp -r` semantics. If dst is an existing
// directory, the top-level entry lands inside it. Otherwise the top-level entry becomes dst,
// and missing parent directories are created.
//
// The archive may come from an untrusted sandbox, so entries cannot leave the destination and
// no file is written through a symlink: symlinks are created only after every other entry.
func ReadTar(r io.Reader, dst string, owner *TarOwner) error {
	dst = filepath.Clean(dst)
	base, rename := dst, ""
	if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
		base, rename = filepath.Dir(dst), filepath.Base(dst)
		if err := mkdirAll(base, owner); err != nil {
			return err
		}
	}

	type symlink struct{ target, link string }
	var links []symlink
	top := ""
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." || name == ".." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
			return fmt.Errorf("archive entry %q is outside the destination", hdr.Name)
		}
		first, rest, _ := strings.Cut(name, "/")
		if top == "" {
			top = first
		} else if first != top {
			return fmt.Errorf("archive has more than one top-level entry (%q and %q)", top, first)
		}
		if rename != "" {
			first = rename
		}
		target := filepath.Join(base, filepath.FromSlash(path.Join(first, rest)))
		if err := noSymlinkBetween(base, filepath.Dir(target)); err != nil {
			return err
		}
		perm := fs.FileMode(hdr.Mode) & fs.ModePerm

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(target, perm|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if fi, err := os.Lstat(target); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", target)
			}
		case tar.TypeReg:
			if fi, err := os.Lstat(target); err == nil && !fi.Mode().IsRegular() {
				if fi.IsDir() {
					return fmt.Errorf("%s is a directory", target)
				}
				os.Remove(target)
			}
			if err := writeFile(target, tr, perm); err != nil {
				return err
			}
			os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		case tar.TypeSymlink:
			links = append(links, symlink{target, hdr.Linkname})
			continue
		default:
			continue
		}
		if owner != nil {
			if err := os.Lchown(target, owner.UID, owner.GID); err != nil {
				return err
			}
		}
	}
	if top == "" {
		return errors.New("archive is empty")
	}
	for _, l := range links {
		if fi, err := os.Lstat(l.target); err == nil {
			if fi.IsDir() {
				return fmt.Errorf("%s is a directory", l.target)
			}
			os.Remove(l.target)
		}
		if err := os.Symlink(l.link, l.target); err != nil {
			return err
		}
		if owner != nil {
			os.Lchown(l.target, owner.UID, owner.GID)
		}
	}
	return nil
}

func writeFile(target string, r io.Reader, perm fs.FileMode) error {
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(target, perm)
}

// noSymlinkBetween fails if any path component below base, down to and including dir, is a symlink.
func noSymlinkBetween(base, dir string) error {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == "." {
		return err
	}
	p := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write through symlink %s", p)
		}
	}
	return nil
}

func mkdirAll(dir string, owner *TarOwner) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		return nil
	}
	if err := mkdirAll(filepath.Dir(dir), owner); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if owner != nil {
		return os.Lchown(dir, owner.UID, owner.GID)
	}
	return nil
}
