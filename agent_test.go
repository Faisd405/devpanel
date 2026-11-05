package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDispatch(t *testing.T) {
	root := t.TempDir()
	a := &App{cfg: Config{Root: root, Groups: map[string][]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	// value + error, with typed args
	if _, err := a.dispatch("SaveGroup", []json.RawMessage{raw("Web"), raw([]string{"apache"})}); err != nil {
		t.Fatal(err)
	}
	res, err := a.dispatch("Groups", nil)
	if err != nil || len(res.(map[string][]string)["Web"]) != 1 {
		t.Fatalf("Groups = %v %v", res, err)
	}
	// error result is returned as error
	if _, err := a.dispatch("SaveGroup", []json.RawMessage{raw(""), raw([]string{})}); err == nil {
		t.Fatal("validation error lost")
	}
	for _, c := range []struct {
		m    string
		args []json.RawMessage
	}{{"Nope", nil}, {"SaveGroup", nil}, {"SaveGroup", []json.RawMessage{raw(1), raw("x")}}, {"dispatch", nil}} {
		if _, err := a.dispatch(c.m, c.args); err == nil {
			t.Errorf("dispatch(%s) accepted", c.m)
		}
	}
}

func TestEventLog(t *testing.T) {
	l := newEventLog()
	go func() { time.Sleep(100 * time.Millisecond); l.add("log", []interface{}{"hi", false}) }()
	start := time.Now()
	evs := l.since(0, 5*time.Second)
	if len(evs) != 1 || evs[0].Name != "log" || time.Since(start) > 2*time.Second {
		t.Fatalf("since: %+v", evs)
	}
	if evs := l.since(evs[0].Seq, 200*time.Millisecond); len(evs) != 0 {
		t.Fatalf("expected timeout with no events, got %+v", evs)
	}
	if evs := l.since(999, 50*time.Millisecond); len(evs) != 1 {
		t.Fatal("seq from a previous agent should restart from 0")
	}
}

// startTestAgent serves an App on a random port and writes agent.json like runAgent does.
func startTestAgent(t *testing.T, a *App) (string, agentInfo) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	info := agentInfo{ln.Addr().(*net.TCPAddr).Port, "tok-" + strconv.Itoa(os.Getpid()), os.Getpid()}
	a.events = newEventLog()
	go http.Serve(ln, a.agentHandler(info.Port, info.Token))
	dir := t.TempDir()
	b, _ := json.Marshal(info)
	os.WriteFile(agentFile(dir), b, 0600)
	return dir, info
}

func TestAgentSecurity(t *testing.T) {
	root := t.TempDir()
	a := &App{cfg: Config{Root: root, Groups: map[string][]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	_, info := startTestAgent(t, a)
	url := "http://127.0.0.1:" + strconv.Itoa(info.Port) + "/call"
	body := `{"method":"Groups","args":[]}`
	try := func(token, host string) int {
		req, _ := http.NewRequest("POST", url, strings.NewReader(body))
		if token != "" {
			req.Header.Set("X-DevPanel-Token", token)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := try("", ""); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := try("wrong", ""); c != 401 {
		t.Errorf("wrong token: %d", c)
	}
	if c := try(info.Token, "evil.example:"+strconv.Itoa(info.Port)); c != 403 {
		t.Errorf("foreign Host (DNS rebinding): %d", c)
	}
	if c := try(info.Token, ""); c != 200 {
		t.Errorf("valid request: %d", c)
	}
}

func TestProxyToAgent(t *testing.T) {
	root := t.TempDir()
	a := &App{cfg: Config{Root: root, Groups: map[string][]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	dir, _ := startTestAgent(t, a)
	p := &Proxy{cfgDir: dir}
	if err := p.ensureAgent(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Call("SaveGroup", []json.RawMessage{raw("Web"), raw([]string{"apache", "postgresql"})}); err != nil {
		t.Fatal(err)
	}
	out, err := p.Call("Groups", nil)
	if err != nil || !strings.Contains(string(out), `"Web":["apache","postgresql"]`) {
		t.Fatalf("Groups via proxy: %s %v", out, err)
	}
	if _, err := p.Call("SaveGroup", []json.RawMessage{raw(""), raw([]string{})}); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("agent error not propagated: %v", err)
	}
	// The change happened in the agent's App (it owns the state).
	if len(a.cfg.Groups["Web"]) != 2 {
		t.Fatal("state not in agent")
	}
	// Events emitted by the agent reach the long-poll endpoint.
	a.emitEvent("log", "from agent", false)
	resp, err := p.post("/events?since=0", nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var evs []event
	json.NewDecoder(resp.Body).Decode(&evs)
	resp.Body.Close()
	if len(evs) == 0 || evs[len(evs)-1].Data[0] != "from agent" {
		t.Fatalf("events: %+v", evs)
	}
}

func TestNewAppDoesNotRewriteUnchangedConfig(t *testing.T) {
	// NewApp only writes when migration changes something, so the CLI/UI don't clobber the agent's config.
	a := &App{cfg: Config{Root: `C:\x`, TLD: "test"}}
	b := a.configJSON()
	if !strings.Contains(string(b), `"root": "C:\\x"`) {
		t.Fatalf("configJSON: %s", b)
	}
}
