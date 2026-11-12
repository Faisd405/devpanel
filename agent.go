package main

// Two processes from one exe:
//   agent (devpanel.exe --agent): tray, watchers, services, project processes, share. Long-lived, no WebView2.
//   UI    (devpanel.exe / --ui):  the WebView2 window. Exits when closed, so its memory is freed;
//                                  every call is forwarded to the agent over 127.0.0.1 with a random token.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type agentInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func agentFile(cfgDir string) string { return filepath.Join(cfgDir, "agent.json") }

func readAgent(cfgDir string) (agentInfo, bool) {
	var info agentInfo
	b, err := os.ReadFile(agentFile(cfgDir))
	if err != nil || json.Unmarshal(b, &info) != nil || info.Port == 0 {
		return info, false
	}
	c := http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/ping", info.Port), nil)
	req.Header.Set("X-DevPanel-Token", info.Token)
	resp, err := c.Do(req)
	if err != nil {
		return info, false
	}
	resp.Body.Close()
	return info, resp.StatusCode == 200
}

// ---- Events: agent -> UI ----

type event struct {
	Seq  int64         `json:"seq"`
	Name string        `json:"name"`
	Data []interface{} `json:"data"`
}

type eventLog struct {
	mu   sync.Mutex
	cond *sync.Cond
	seq  int64
	list []event
}

func newEventLog() *eventLog {
	l := &eventLog{}
	l.cond = sync.NewCond(&l.mu)
	return l
}

func (l *eventLog) add(name string, data []interface{}) {
	l.mu.Lock()
	l.seq++
	l.list = append(l.list, event{l.seq, name, data})
	if len(l.list) > 500 {
		l.list = l.list[len(l.list)-500:]
	}
	l.mu.Unlock()
	l.cond.Broadcast()
}

// since returns events after seq, waiting up to `wait` for new ones.
func (l *eventLog) since(seq int64, wait time.Duration) []event {
	timer := time.AfterFunc(wait, l.cond.Broadcast)
	defer timer.Stop()
	deadline := time.Now().Add(wait)
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq > l.seq { // agent restarted: start over
		seq = 0
	}
	for l.seq <= seq && time.Now().Before(deadline) {
		l.cond.Wait()
	}
	var out []event
	for _, e := range l.list {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out
}

// ---- RPC dispatch ----

var errType = reflect.TypeOf((*error)(nil)).Elem()

// dispatch calls an exported App method by name with JSON-encoded arguments.
func (a *App) dispatch(method string, args []json.RawMessage) (interface{}, error) {
	m := reflect.ValueOf(a).MethodByName(method)
	if !m.IsValid() {
		return nil, fmt.Errorf("unknown method %q", method)
	}
	t := m.Type()
	if t.NumIn() != len(args) {
		return nil, fmt.Errorf("%s expects %d arguments, got %d", method, t.NumIn(), len(args))
	}
	in := make([]reflect.Value, len(args))
	for i, raw := range args {
		p := reflect.New(t.In(i))
		if err := json.Unmarshal(raw, p.Interface()); err != nil {
			return nil, fmt.Errorf("%s argument %d: %w", method, i+1, err)
		}
		in[i] = p.Elem()
	}
	var res interface{}
	var err error
	for _, o := range m.Call(in) {
		if o.Type() == errType {
			if !o.IsNil() {
				err = o.Interface().(error)
			}
		} else {
			res = o.Interface()
		}
	}
	return res, err
}

type rpcRequest struct {
	Method string            `json:"method"`
	Args   []json.RawMessage `json:"args"`
}

type rpcResponse struct {
	Result interface{} `json:"result"`
	Error  string      `json:"error,omitempty"`
}

// agentHandler serves /ping, /call and /events. Every request needs the token; the Host check
// also stops DNS-rebinding pages in a browser from reaching it.
func (a *App) agentHandler(port int, token string) http.Handler {
	host := "127.0.0.1:" + strconv.Itoa(port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-DevPanel-Token")), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/ping":
			w.Write([]byte("ok"))
		case "/call":
			var req rpcRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			res, err := a.dispatch(req.Method, req.Args)
			out := rpcResponse{Result: res}
			if err != nil {
				out.Error = err.Error()
			}
			json.NewEncoder(w).Encode(out)
		case "/events":
			seq, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
			evs := a.events.since(seq, 25*time.Second)
			if evs == nil {
				evs = []event{}
			}
			json.NewEncoder(w).Encode(evs)
		default:
			http.NotFound(w, r)
		}
	})
}

// ---- Agent process ----

// runAgent runs the background process until "Quit" in the tray. Returns the exit code.
func (a *App) runAgent() int {
	dir := filepath.Dir(a.cfgPath)
	if _, ok := readAgent(dir); ok {
		return 0 // one agent per install
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1
	}
	port := ln.Addr().(*net.TCPAddr).Port
	tok := make([]byte, 32)
	rand.Read(tok)
	token := hex.EncodeToString(tok)
	a.events = newEventLog()
	a.quitCh = make(chan struct{})
	go http.Serve(ln, a.agentHandler(port, token))
	b, _ := json.Marshal(agentInfo{port, token, os.Getpid()})
	if err := os.WriteFile(agentFile(dir), b, 0600); err != nil {
		return 1
	}
	defer os.Remove(agentFile(dir))

	a.ensureLayout() // standalone: bin, www, data, etc ... on first start
	a.runTray()
	go a.watchSites()
	go a.watchServices()
	go a.watchExternal()
	go a.watchBackups()
	if launchedMinimized() && a.cfg.StartGroup != "" {
		go func() { a.trayResult("Start "+a.cfg.StartGroup, a.StartGroup(a.cfg.StartGroup)) }()
	}
	for _, p := range a.cfg.Procs {
		if p.Autostart {
			a.StartProcess(p.Name)
		}
	}

	<-a.quitCh
	a.emitEvent("quit") // closes an open window too
	time.Sleep(300 * time.Millisecond)
	a.stopAllProcesses()
	a.StopShare()
	a.StopAdminer()
	systray.Quit()
	return 0
}

func (a *App) quitAgent() { a.quitOnce.Do(func() { close(a.quitCh) }) }

// openUI starts the window process; if one is already open, its single-instance lock brings it to front.
func openUI() {
	exe, _ := os.Executable()
	exec.Command(exe, "--ui").Start()
}

// ---- Dialog-backed calls, split so the agent does the work and the UI only picks the file ----

func (a *App) BackupFolder(engine string) string { return a.backupDir(engine) }

func (a *App) ExportConfigTo(f string) (string, error) {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	return "exported to " + f, os.WriteFile(f, b, 0644)
}

func (a *App) ImportConfigFrom(f string) (string, error) { return a.importFrom(f) }

// ---- UI process ----

// Proxy is the only object bound to the window. The frontend calls Proxy.Call(method, args).
type Proxy struct {
	ctx    context.Context
	cfgDir string
	mu     sync.Mutex
	info   agentInfo
}

// ensureAgent connects to the running agent or starts one.
func (p *Proxy) ensureAgent() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if info, ok := readAgent(p.cfgDir); ok {
		p.info = info
		return nil
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "--agent")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | NEW_PROCESS_GROUP
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		if info, ok := readAgent(p.cfgDir); ok {
			p.info = info
			return nil
		}
	}
	return errors.New("DevPanel background agent did not start")
}

func (p *Proxy) post(path string, body []byte, timeout time.Duration) (*http.Response, error) {
	p.mu.Lock()
	info := p.info
	p.mu.Unlock()
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d%s", info.Port, path), bytes.NewReader(body))
	req.Header.Set("X-DevPanel-Token", info.Token)
	return (&http.Client{Timeout: timeout}).Do(req)
}

func (p *Proxy) call(method string, args []json.RawMessage) (json.RawMessage, error) {
	body, _ := json.Marshal(rpcRequest{method, args})
	resp, err := p.post("/call", body, 3*time.Hour) // installs/restores can take long
	if err != nil {
		if e := p.ensureAgent(); e != nil { // agent gone: start it and retry once
			return nil, e
		}
		if resp, err = p.post("/call", body, 3*time.Hour); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agent: %s", resp.Status)
	}
	if out.Error != "" {
		return nil, errors.New(out.Error)
	}
	return out.Result, nil
}

func raw(v interface{}) json.RawMessage { b, _ := json.Marshal(v); return b }

// Call forwards a method to the agent. File dialogs need the window, so those run here.
func (p *Proxy) Call(method string, args []json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "PickFolder":
		var title string
		if len(args) > 0 {
			json.Unmarshal(args[0], &title)
		}
		d, err := runtime.OpenDirectoryDialog(p.ctx, runtime.OpenDialogOptions{Title: title})
		return raw(d), err
	case "PickSQLFile":
		dir, err := p.call("BackupFolder", args)
		if err != nil {
			return nil, err
		}
		var d string
		json.Unmarshal(dir, &d)
		os.MkdirAll(d, 0755)
		f, err := runtime.OpenFileDialog(p.ctx, runtime.OpenDialogOptions{Title: "Choose a .sql dump", DefaultDirectory: d,
			Filters: []runtime.FileFilter{{DisplayName: "SQL dump (*.sql)", Pattern: "*.sql"}}})
		return raw(f), err
	case "ExportConfig":
		f, err := runtime.SaveFileDialog(p.ctx, runtime.SaveDialogOptions{Title: "Export DevPanel settings",
			DefaultFilename: "devpanel-settings.json", Filters: []runtime.FileFilter{{DisplayName: "JSON", Pattern: "*.json"}}})
		if err != nil || f == "" {
			return raw(""), err
		}
		return p.call("ExportConfigTo", []json.RawMessage{raw(f)})
	case "ImportConfig":
		f, err := runtime.OpenFileDialog(p.ctx, runtime.OpenDialogOptions{Title: "Import DevPanel settings",
			Filters: []runtime.FileFilter{{DisplayName: "JSON", Pattern: "*.json"}}})
		if err != nil || f == "" {
			return raw(""), err
		}
		return p.call("ImportConfigFrom", []json.RawMessage{raw(f)})
	}
	return p.call(method, args)
}

func (p *Proxy) startup(ctx context.Context) {
	p.ctx = ctx
	go p.pumpEvents()
}

func (p *Proxy) show() {
	if p.ctx != nil {
		runtime.WindowUnminimise(p.ctx)
		runtime.WindowShow(p.ctx)
	}
}

// pumpEvents long-polls the agent and re-emits its events in the window.
func (p *Proxy) pumpEvents() {
	var seq int64
	for {
		resp, err := p.post("/events?since="+strconv.FormatInt(seq, 10), nil, 40*time.Second)
		if err != nil {
			time.Sleep(time.Second)
			p.ensureAgent()
			continue
		}
		var evs []event
		json.NewDecoder(resp.Body).Decode(&evs)
		resp.Body.Close()
		for _, e := range evs {
			if e.Seq > seq {
				seq = e.Seq
			}
			if e.Name == "quit" {
				runtime.Quit(p.ctx)
				return
			}
			runtime.EventsEmit(p.ctx, e.Name, e.Data...)
		}
	}
}
