package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// browser key codes (KeyboardEvent.code) to QEMU qcodes
var codeToQcode = map[string]string{
	"Enter": "ret", "Escape": "esc", "Backspace": "backspace", "Tab": "tab", "Space": "spc",
	"Minus": "minus", "Equal": "equal", "BracketLeft": "bracket_left", "BracketRight": "bracket_right",
	"Backslash": "backslash", "Semicolon": "semicolon", "Quote": "apostrophe", "Backquote": "grave_accent",
	"Comma": "comma", "Period": "dot", "Slash": "slash", "CapsLock": "caps_lock",
	"ShiftLeft": "shift", "ShiftRight": "shift_r", "ControlLeft": "ctrl", "ControlRight": "ctrl_r",
	"AltLeft": "alt", "AltRight": "alt_r", "MetaLeft": "meta_l", "MetaRight": "meta_r",
	"ArrowUp": "up", "ArrowDown": "down", "ArrowLeft": "left", "ArrowRight": "right",
	"Insert": "insert", "Delete": "delete", "Home": "home", "End": "end", "PageUp": "pgup", "PageDown": "pgdn",
}

func qcodeFor(code string) string {
	if q, ok := codeToQcode[code]; ok {
		return q
	}
	if len(code) == 4 && code[:3] == "Key" {
		return string(code[3] + 32)
	}
	if len(code) == 6 && code[:5] == "Digit" {
		return code[5:]
	}
	if len(code) >= 2 && code[0] == 'F' {
		return "f" + code[1:]
	}
	return ""
}

const viewPage = `<!doctype html><html><head><meta charset="utf-8"><title>veems · %[1]s</title>
<style>html,body{margin:0;height:100%%;background:#111;color:#aaa;font:13px system-ui,sans-serif}
#bar{padding:6px 10px}#s{display:block;max-width:100vw;max-height:calc(100vh - 30px);margin:auto;outline:none;cursor:crosshair}</style></head>
<body><div id="bar">%[1]s · click the screen to control it · live view</div><img id="s" tabindex="0" draggable="false">
<script>
const t=%[2]q, s=document.getElementById('s');
let busy=false;
async function frame(){ if(busy) return; busy=true;
  try{ const r=await fetch('shot?t='+t,{cache:'no-store'}); if(r.ok){ const u=URL.createObjectURL(await r.blob()); const old=s.src; s.src=u; if(old) URL.revokeObjectURL(old);} }catch(e){}
  busy=false }
setInterval(frame,250); frame();
function xy(e){ const r=s.getBoundingClientRect(); return {x:Math.round((e.clientX-r.left)*s.naturalWidth/r.width), y:Math.round((e.clientY-r.top)*s.naturalHeight/r.height)} }
function send(o){ fetch('input?t='+t,{method:'POST',body:JSON.stringify(o)}) }
const btn=['left','middle','right'];
s.addEventListener('contextmenu',e=>e.preventDefault());
s.addEventListener('mousedown',e=>{ s.focus(); send({kind:'btn',...xy(e),button:btn[e.button],down:true}); e.preventDefault() });
s.addEventListener('mouseup',e=>send({kind:'btn',...xy(e),button:btn[e.button],down:false}));
let last=0; s.addEventListener('mousemove',e=>{ const n=Date.now(); if(n-last>40){ last=n; send({kind:'move',...xy(e)}) } });
s.addEventListener('wheel',e=>{ send({kind:'wheel',...xy(e),up:e.deltaY<0}); e.preventDefault() },{passive:false});
s.addEventListener('keydown',e=>{ send({kind:'key',code:e.code,down:true}); e.preventDefault() });
s.addEventListener('keyup',e=>{ send({kind:'key',code:e.code,down:false}); e.preventDefault() });
</script></body></html>`

// view serves a live, clickable view of the VM's screen on localhost.
func view(m *Meta, port int, open bool) error {
	if !m.Desktop && m.OS != "windows" {
		fmt.Fprintln(os.Stderr, "veems: this VM has no desktop, so the view shows its text console")
	}
	tok := make([]byte, 16)
	rand.Read(tok)
	token := hex.EncodeToString(tok)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var q *qmpConn
	var w, h int
	input := func(events []map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		if q == nil {
			var err error
			if q, err = dialQMP(vmDir(m.ID), 10*time.Second); err != nil {
				return err
			}
		}
		if err := q.send(events); err != nil {
			q.Close()
			q = nil
			return err
		}
		return nil
	}
	ok := func(r *http.Request) bool { return r.URL.Query().Get("t") == token }
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		if !ok(r) {
			http.Error(rw, "forbidden", 403)
			return
		}
		fmt.Fprintf(rw, viewPage, m.ID, token)
	})
	mux.HandleFunc("/shot", func(rw http.ResponseWriter, r *http.Request) {
		if !ok(r) {
			http.Error(rw, "forbidden", 403)
			return
		}
		png, sw, sh, err := screenshot(m.ID)
		if err != nil {
			http.Error(rw, err.Error(), 502)
			return
		}
		mu.Lock()
		w, h = sw, sh
		mu.Unlock()
		touch(m.ID)
		rw.Header().Set("Content-Type", "image/png")
		rw.Write(png)
	})
	mux.HandleFunc("/input", func(rw http.ResponseWriter, r *http.Request) {
		if !ok(r) || r.Method != "POST" {
			http.Error(rw, "forbidden", 403)
			return
		}
		var ev struct {
			Kind, Button, Code string
			X, Y               int
			Down, Up           bool
		}
		if json.NewDecoder(r.Body).Decode(&ev) != nil {
			return
		}
		mu.Lock()
		sw, sh := w, h
		mu.Unlock()
		var events []map[string]any
		switch ev.Kind {
		case "move":
			events = absEvents(ev.X, ev.Y, sw, sh)
		case "btn":
			events = append(absEvents(ev.X, ev.Y, sw, sh), btnEvent(ev.Button, ev.Down)...)
		case "wheel":
			b := "wheel-down"
			if ev.Up {
				b = "wheel-up"
			}
			events = append(append(absEvents(ev.X, ev.Y, sw, sh), btnEvent(b, true)...), btnEvent(b, false)...)
		case "key":
			if qc := qcodeFor(ev.Code); qc != "" {
				events = []map[string]any{keyEvent(qc, ev.Down)}
			}
		}
		if len(events) > 0 {
			if !m.Desktop && ev.Kind != "key" {
				return // no pointer device on headless VMs
			}
			input(events)
			touch(m.ID)
		}
	})
	url := fmt.Sprintf("http://%s/?t=%s", ln.Addr(), token)
	fmt.Println(url)
	fmt.Fprintln(os.Stderr, "veems: serving a live view of", m.ID, "on this machine only; Ctrl-C to stop")
	if open {
		for _, opener := range []string{"xdg-open", "open"} {
			if p, err := exec.LookPath(opener); err == nil {
				exec.Command(p, url).Start()
				break
			}
		}
	}
	return http.Serve(ln, mux)
}
