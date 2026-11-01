package main

import (
	"archive/tar"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const chunk = 4 << 20

// The Windows guest agent stalls for good when one reply is much over 512 KB,
// so reads from Windows go in smaller pieces.
func readChunk(m *Meta) int {
	if m.OS == "windows" {
		return 256 << 10
	}
	return chunk
}

// guestPath makes a guest path absolute, relative paths starting at the work dir.
func guestPath(m *Meta, p string) string {
	prof, _ := profile(m.OS)
	if m.OS == "windows" {
		p = strings.ReplaceAll(p, "/", `\`)
		if p == "" || p == "." {
			return prof.WorkDir
		}
		if len(p) >= 2 && p[1] == ':' || strings.HasPrefix(p, `\\`) {
			return p
		}
		return prof.WorkDir + `\` + p
	}
	if p == "" || p == "." {
		return prof.WorkDir
	}
	if strings.HasPrefix(p, "/") {
		return p
	}
	return path.Join(prof.WorkDir, p)
}

func guestBase(m *Meta, p string) string {
	if m.OS == "windows" {
		p = strings.TrimRight(p, `\`)
		return p[strings.LastIndexAny(p, `\:`)+1:]
	}
	return path.Base(p)
}

func guestJoin(m *Meta, dir, name string) string {
	if m.OS == "windows" {
		return strings.TrimRight(dir, `\`) + `\` + name
	}
	return path.Join(dir, name)
}

func guestTemp(m *Meta) string {
	if m.OS == "windows" {
		return fmt.Sprintf(`C:\Windows\Temp\veems-%d.tar`, time.Now().UnixNano())
	}
	return fmt.Sprintf("/tmp/veems-%d.tar", time.Now().UnixNano())
}

// prepTarget makes a file's parent directory and says whether dst is a directory.
func prepTarget(m *Meta, dst string) (isDir bool, err error) {
	var out string
	if m.OS == "windows" {
		out, err = mustExec(m, ExecOpts{Cwd: `C:\`, Line: fmt.Sprintf(
			`$p=%s; if (Test-Path -LiteralPath $p -PathType Container) { 'dir' } else { New-Item -ItemType Directory -Force -Path (Split-Path -Parent $p) | Out-Null; 'file' }`, psQuote(dst))})
	} else {
		out, err = mustExec(m, ExecOpts{Cwd: "/", Argv: []string{"/bin/sh", "-c",
			`if [ -d "$1" ]; then echo dir; else mkdir -p -- "$(dirname -- "$1")" && echo file; fi`, "veems", dst}})
	}
	return strings.TrimSpace(out) == "dir", err
}

func openGuest(m *Meta, p, mode string) (int, error) {
	var h int
	err := guestCall(m.ID, "guest-file-open", map[string]any{"path": p, "mode": mode}, &h, 30*time.Second)
	return h, err
}

func closeGuest(m *Meta, h int) {
	guestCall(m.ID, "guest-file-close", map[string]any{"handle": h}, nil, 30*time.Second)
}

// writeGuest streams r into a new guest file.
func writeGuest(m *Meta, dst string, r io.Reader) error {
	h, err := openGuest(m, dst, "wb")
	if err != nil {
		return err
	}
	defer closeGuest(m, h)
	buf := make([]byte, chunk)
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if err := guestCall(m.ID, "guest-file-write", map[string]any{"handle": h,
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

// readGuest streams a guest file into w. On an error before any byte is
// written (a directory, say) it returns errNotFile.
var errNotFile = errors.New("not a regular file")

func readGuest(m *Meta, src string, w io.Writer) error {
	h, err := openGuest(m, src, "rb")
	if err != nil {
		return fmt.Errorf("%w: %v", errNotFile, err)
	}
	defer closeGuest(m, h)
	first := true
	for {
		var fr fileRead
		if err := guestCall(m.ID, "guest-file-read", map[string]any{"handle": h, "count": readChunk(m)}, &fr, 120*time.Second); err != nil {
			if first {
				return fmt.Errorf("%w: %v", errNotFile, err)
			}
			return err
		}
		first = false
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

func removeGuest(m *Meta, p string) {
	if m.OS == "windows" {
		guestExec(m, ExecOpts{Cwd: `C:\`, Line: "Remove-Item -Force -LiteralPath " + psQuote(p)})
	} else {
		guestExec(m, ExecOpts{Cwd: "/", Argv: []string{"rm", "-f", "--", p}})
	}
}

// Push copies a host file or directory into the guest. dst is the final path;
// if it is an existing guest directory (or ends in a separator) a file lands inside it.
func push(m *Meta, src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	trailing := strings.HasSuffix(dst, "/") || strings.HasSuffix(dst, `\`) || dst == ""
	dst = guestPath(m, dst)
	if !st.IsDir() {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		isDir, err := prepTarget(m, dst)
		if err != nil {
			return err
		}
		if isDir || trailing {
			dst = guestJoin(m, dst, filepath.Base(src))
		}
		return writeGuest(m, dst, f)
	}
	// Directory: tar on the host, stream it over, unpack with the guest's tar.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, src)) }()
	tmp := guestTemp(m)
	if err := writeGuest(m, tmp, pr); err != nil {
		pr.CloseWithError(err)
		return err
	}
	defer removeGuest(m, tmp)
	if m.OS == "windows" {
		_, err = mustExec(m, ExecOpts{Cwd: `C:\`, Line: fmt.Sprintf(
			`New-Item -ItemType Directory -Force -Path %s | Out-Null; tar.exe -xf %s -C %s`, psQuote(dst), psQuote(tmp), psQuote(dst))})
	} else {
		_, err = mustExec(m, ExecOpts{Cwd: "/", Argv: []string{"/bin/sh", "-c",
			`mkdir -p -- "$2" && tar --no-same-owner -xf "$1" -C "$2"`, "veems", tmp, dst}})
	}
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

// Pull copies a guest file or directory to the host.
func pull(m *Meta, src, dst string) error {
	src = guestPath(m, src)
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		dst = filepath.Join(dst, guestBase(m, src))
	}
	gtmp := guestTemp(m)
	var err error
	if m.OS == "windows" {
		// The Windows agent opens a directory but then blocks reading it, so
		// look first: a directory is tarred in the same step.
		var out string
		out, err = mustExec(m, ExecOpts{Cwd: `C:\`, Line: fmt.Sprintf(
			`$p=%[1]s; if (Test-Path -LiteralPath $p -PathType Container) { tar.exe -cf %[2]s -C $p .; if ($LASTEXITCODE) { exit $LASTEXITCODE }; 'dir' } elseif (Test-Path -LiteralPath $p) { 'file' } else { [Console]::Error.WriteLine("no such file or directory: $p"); exit 2 }`,
			psQuote(src), psQuote(gtmp))})
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "file" {
			return pullFile(m, src, dst)
		}
	} else {
		err = pullFile(m, src, dst)
		if !errors.Is(err, errNotFile) {
			return err
		}
		// Probably a directory: tar it up in the guest and unpack safely here.
		_, err = mustExec(m, ExecOpts{Cwd: "/", Argv: []string{"/bin/sh", "-c",
			`[ -d "$1" ] || { echo "no such file or directory: $1" >&2; exit 2; }; tar -cf "$2" -C "$1" .`, "veems", src, gtmp}})
		if err != nil {
			return err
		}
	}
	defer removeGuest(m, gtmp)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(readGuest(m, gtmp, pw)) }()
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
