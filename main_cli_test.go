package main

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestParseFlags(t *testing.T) {
	f, err := parse([]string{"vm", "--cwd", "/x", "--env=A=1", "-env", "B=2", "--json", "--", "prog", "--cwd", "y"}, "cwd env", "json")
	if err != nil {
		t.Fatal(err)
	}
	if f.str("cwd", "") != "/x" || !reflect.DeepEqual(f.vals["env"], []string{"A=1", "B=2"}) || !f.bools["json"] {
		t.Errorf("flags: %+v", f)
	}
	if !reflect.DeepEqual(f.pos, []string{"vm"}) || !reflect.DeepEqual(f.rest, []string{"prog", "--cwd", "y"}) {
		t.Errorf("pos %q rest %q", f.pos, f.rest)
	}
	if _, err := parse([]string{"--cwd"}, "cwd", ""); err == nil {
		t.Error("a valued flag without a value was accepted")
	}
	if _, err := parse([]string{"-V"}, "", ""); !errors.Is(err, errUnknownFlag) {
		t.Errorf("unknown flag: %v", err)
	}
	if !jsonWanted([]string{"vm", "--json", "--bad"}) || jsonWanted([]string{"--", "--json"}) {
		t.Error("jsonWanted")
	}
}

func TestDurations(t *testing.T) {
	for in, want := range map[string]time.Duration{"90": 90 * time.Second, "2h": 2 * time.Hour, "0": 0, "off": 0} {
		f, _ := parse([]string{"--ttl", in}, "ttl", "")
		if d, err := f.dur("ttl", time.Minute); err != nil || d != want {
			t.Errorf("%q: %v %v", in, d, err)
		}
	}
}

func TestRemotePaths(t *testing.T) {
	tempHome(t)
	os.MkdirAll(vmDir("lnx-abc123"), 0o700)
	saveMeta(&Meta{ID: "lnx-abc123", OS: "linux"})
	for in, want := range map[string][2]string{
		"lnx-abc123:src": {"lnx-abc123", "src"},
		"lnx-abc123:":    {"lnx-abc123", ""},
		"./lnx-abc123:x": {"", ""}, // a local path
		"nosuchvm:x":     {"", ""},
	} {
		id, p, _ := splitRemote(in)
		if id != want[0] || p != want[1] {
			t.Errorf("splitRemote(%q) = %q, %q", in, id, p)
		}
	}
	if r, l, ok := cutLast(`C:\out\x.txt:./x.txt`); !ok || r != `C:\out\x.txt` || l != "./x.txt" {
		t.Errorf("cutLast: %q %q", r, l)
	}
}
