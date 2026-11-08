package main

import (
	"archive/tar"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func openGuest(a *agentConn, p, mode string) (int, error) {
	var h int
	err := a.call("guest-file-open", map[string]any{"path": p, "mode": mode}, &h, 30*time.Second)
	return h, err
}

func closeGuest(a *agentConn, h int) {
	a.call("guest-file-close", map[string]any{"handle": h}, nil, 30*time.Second)
}

func writeGuest(m *Meta, dst string, r io.Reader) error {
	a := guestSession(m.ID)
	defer a.close()
	return writeFile(a, dst, r)
}

func writeFile(a *agentConn, dst string, r io.Reader) error {
	h, err := openGuest(a, dst, "wb")
	if err != nil {
		return err
	}
	defer closeGuest(a, h)
	buf := make([]byte, 4<<20)
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if err := a.call("guest-file-write", map[string]any{"handle": h,
				"buf-b64": base64.StdEncoding.EncodeToString(buf[:n])}, nil, 120*time.Second); err != nil {
				return err
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

type fileRead struct {
	Count int    `json:"count"`
	Buf   string `json:"buf-b64"`
	EOF   bool   `json:"eof"`
}

func readGuest(m *Meta, src string, w io.Writer) error {
	a := guestSession(m.ID)
	defer a.close()
	h, err := openGuest(a, src, "rb")
	if err != nil {
		return err
	}
	defer closeGuest(a, h)
	for {
		var fr fileRead
		if err := a.call("guest-file-read", map[string]any{"handle": h, "count": m.guest().readChunk()}, &fr, 120*time.Second); err != nil {
			return err
		}
		b, err := base64.StdEncoding.DecodeString(fr.Buf)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if fr.EOF || fr.Count == 0 {
			return nil
		}
	}
}

// push copies a host file or directory into the guest. dst is the final path;
// if it is an existing guest directory (or ends in a separator) a file lands inside it.
func push(m *Meta, src, dst string) error {
	g := m.guest()
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	trailing := strings.HasSuffix(dst, "/") || strings.HasSuffix(dst, `\`) || dst == ""
	dst = g.abs(dst)
	if !st.IsDir() {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		out, err := mustExec(m, g.prepTarget(dst))
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "dir" || trailing {
			dst = g.join(dst, filepath.Base(src))
		}
		return writeGuest(m, dst, f)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, src)) }()
	tmp := g.temp(".tar")
	if err := writeGuest(m, tmp, pr); err != nil {
		pr.CloseWithError(err)
		return err
	}
	defer guestExec(m, g.remove(tmp))
	_, err = mustExec(m, g.unpack(tmp, dst))
	return err
}

func writeTar(w io.Writer, root string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Uname, hdr.Gname, hdr.Uid, hdr.Gid = "", "", 0, 0
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// pull copies a guest file or directory to the host.
func pull(m *Meta, src, dst string) error {
	g := m.guest()
	src = g.abs(src)
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		dst = filepath.Join(dst, g.base(src))
	}
	tar := g.temp(".tar")
	out, err := mustExec(m, g.packIfDir(src, tar))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "file" {
		return pullFile(m, src, dst)
	}
	defer guestExec(m, g.remove(tar))
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(readGuest(m, tar, pw)) }()
	err = safeUntar(pr, dst)
	pr.CloseWithError(err)
	return err
}

func pullFile(m *Meta, src, dst string) error {
	tmp := dst + ".veems-partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	err = readGuest(m, src, f)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// safeUntar unpacks a tar written by an untrusted guest. Nothing may land
// outside dst: absolute names, "..", links that leave dst, and writes through
// symlinks are refused; devices, setuid bits, and ownership are dropped.
func safeUntar(r io.Reader, dst string) error {
	dst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	inside := func(p string) bool {
		return p == dst || strings.HasPrefix(p, dst+string(os.PathSeparator))
	}
	// noLinksBetween checks that no existing component between dst and p is a symlink.
	noLinksBetween := func(p string) error {
		rel, _ := filepath.Rel(dst, filepath.Dir(p))
		cur := dst
		for _, part := range strings.Split(rel, string(os.PathSeparator)) {
			if part == "." || part == "" {
				continue
			}
			cur = filepath.Join(cur, part)
			if st, err := os.Lstat(cur); err == nil && st.Mode()&fs.ModeSymlink != 0 {
				return fmt.Errorf("refusing to write through symlink %s", cur)
			}
		}
		return nil
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.ReplaceAll(hdr.Name, `\`, "/"))
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return fmt.Errorf("refusing unsafe path %q from guest", hdr.Name)
		}
		target := filepath.Join(dst, name)
		if !inside(target) {
			return fmt.Errorf("refusing unsafe path %q from guest", hdr.Name)
		}
		if err := noLinksBetween(target); err != nil {
			return err
		}
		mode := fs.FileMode(hdr.Mode) & 0o777
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target) // never follow an existing symlink
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			l := hdr.Linkname
			if filepath.IsAbs(l) || !inside(filepath.Join(filepath.Dir(target), l)) {
				fmt.Fprintf(os.Stderr, "veems: skipped symlink %s -> %s (points outside the copy)\n", hdr.Name, l)
				continue
			}
			os.Remove(target)
			if err := os.Symlink(l, target); err != nil {
				return err
			}
		default:
			// Hard links, devices, fifos: skipped.
		}
	}
}
