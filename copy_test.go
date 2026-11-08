package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, link string
	typ        byte
	mode       int64
}

func tarOf(t *testing.T, es ...entry) *bytes.Buffer {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: e.mode}
		body := ""
		if e.typ == tar.TypeReg {
			body = "data"
			h.Size = 4
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	return &b
}

func TestSafeUntarRefusesEscapes(t *testing.T) {
	for name, es := range map[string][]entry{
		"absolute path":         {{name: "/etc/x", typ: tar.TypeReg, mode: 0o644}},
		"parent path":           {{name: "../x", typ: tar.TypeReg, mode: 0o644}},
		"nested parent path":    {{name: "a/../../x", typ: tar.TypeReg, mode: 0o644}},
		"write through symlink": {{name: "l", link: ".", typ: tar.TypeSymlink}, {name: "l/x", typ: tar.TypeReg, mode: 0o644}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dst := filepath.Join(root, "dst")
			if err := safeUntar(tarOf(t, es...), dst); err == nil {
				t.Error("want an error")
			}
			if exists(filepath.Join(root, "x")) || exists("/etc/x") {
				t.Error("wrote outside the destination")
			}
		})
	}
}

func TestSafeUntarKeepsWhatIsSafe(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst")
	err := safeUntar(tarOf(t,
		entry{name: "dir/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "dir/f", typ: tar.TypeReg, mode: 0o4755},
		entry{name: "..hidden", typ: tar.TypeReg, mode: 0o600},
		entry{name: "in", link: "dir/f", typ: tar.TypeSymlink},
		entry{name: "out", link: "../../etc/passwd", typ: tar.TypeSymlink},
		entry{name: "abs", link: "/etc/passwd", typ: tar.TypeSymlink},
		entry{name: "dev", typ: tar.TypeChar},
	), dst)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dst, "dir/f"))
	if err != nil || st.Mode()&os.ModeSetuid != 0 || st.Mode().Perm() != 0o755 {
		t.Errorf("dir/f: %v %v (want 0755, no setuid)", st.Mode(), err)
	}
	if !exists(filepath.Join(dst, "..hidden")) {
		t.Error("a name starting with .. was refused")
	}
	if l, err := os.Readlink(filepath.Join(dst, "in")); err != nil || l != "dir/f" {
		t.Errorf("symlink inside: %q %v", l, err)
	}
	for _, skipped := range []string{"out", "abs", "dev"} {
		if _, err := os.Lstat(filepath.Join(dst, skipped)); err == nil {
			t.Errorf("%s should have been skipped", skipped)
		}
	}
}

func TestGuestPaths(t *testing.T) {
	lin, win := linuxGuest{}, windowsGuest{}
	for _, c := range []struct {
		g          guestOS
		in, abs    string
		base, join string
	}{
		{lin, "", "/work", "work", "/work/f"},
		{lin, "a/b", "/work/a/b", "b", "/work/a/b/f"},
		{lin, "/etc/x", "/etc/x", "x", "/etc/x/f"},
		{win, "", `C:\work`, "work", `C:\work\f`},
		{win, "a/b", `C:\work\a\b`, "b", `C:\work\a\b\f`},
		{win, `D:\x\`, `D:\x\`, "x", `D:\x\f`},
		{win, `\\srv\share`, `\\srv\share`, "share", `\\srv\share\f`},
	} {
		abs := c.g.abs(c.in)
		if abs != c.abs || c.g.base(abs) != c.base || c.g.join(abs, "f") != c.join {
			t.Errorf("%T %q: abs %q base %q join %q", c.g, c.in, abs, c.g.base(abs), c.g.join(abs, "f"))
		}
	}
}

func TestPsScriptQuotes(t *testing.T) {
	s := psScript(`C:\it's here`, ExecOpts{Argv: []string{"prog", "a'b", "$(boom)"}, Env: []string{"K=v'1"}})
	for _, want := range []string{
		`Set-Location -LiteralPath 'C:\it''s here'`,
		`& 'prog' 'a''b' '$(boom)'`,
		`Set-Item -LiteralPath 'env:K' -Value 'v''1'`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %s:\n%s", want, s)
		}
	}
}
