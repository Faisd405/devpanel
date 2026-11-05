package main

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func copyFile(t *testing.T, src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	os.MkdirAll(filepath.Dir(dst), 0755)
	out, _ := os.Create(dst)
	io.Copy(out, in)
	out.Close()
}

// Regression: a sandbox/other install must never kill processes of the same name from elsewhere.
func TestKillOnlyOwnProcesses(t *testing.T) {
	ping := filepath.Join(os.Getenv("SystemRoot"), `System32\PING.EXE`)
	mine := filepath.Join(t.TempDir(), `bin\apache\x\bin\PING.EXE`)
	copyFile(t, ping, mine)

	own := exec.Command(mine, "-n", "30", "127.0.0.1")
	foreign := exec.Command(ping, "-n", "30", "127.0.0.1")
	if own.Start() != nil || foreign.Start() != nil {
		t.Fatal("start")
	}
	defer foreign.Process.Kill()
	time.Sleep(300 * time.Millisecond)

	dir := filepath.Dir(filepath.Dir(mine))
	pids := processesUnder(dir)
	if !slices.Contains(pids, uint32(own.Process.Pid)) || slices.Contains(pids, uint32(foreign.Process.Pid)) {
		t.Fatalf("processesUnder = %v (own %d, foreign %d)", pids, own.Process.Pid, foreign.Process.Pid)
	}
	if n := killUnder(dir); n != 1 {
		t.Fatalf("killed %d, want 1", n)
	}
	own.Wait()
	if p, _ := os.FindProcess(foreign.Process.Pid); p == nil || exePath(uint32(foreign.Process.Pid)) == "" {
		t.Fatal("foreign process was killed")
	}
}

func TestStopRefusesForeignServer(t *testing.T) {
	defer setPortOverrides(nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0") // stands in for another program on the port
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	p := ln.Addr().(*net.TCPAddr).Port
	setPortOverrides(map[string]int{"apache": p})

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `bin\apache\httpd-x\bin`), 0755)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}}
	if err := a.stop("apache"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("stop: %v", err)
	}
	if err := a.restartApache(); err == nil || !strings.Contains(err.Error(), "another program") {
		t.Fatalf("restartApache: %v", err)
	}
	if !portOpen(p) {
		t.Fatal("foreign listener was stopped on :" + strconv.Itoa(p))
	}
}

// Regression: stopping Apache must also end its php-cgi children, which run from bin\php (another folder).
func TestKillUnderTakesChildren(t *testing.T) {
	sys := filepath.Join(os.Getenv("SystemRoot"), "System32")
	mine := filepath.Join(t.TempDir(), `bin\apache\x\bin\cmd.exe`)
	copyFile(t, filepath.Join(sys, "cmd.exe"), mine)
	parent := exec.Command(mine, "/c", filepath.Join(sys, "PING.EXE"), "-n", "30", "127.0.0.1")
	foreign := exec.Command(filepath.Join(sys, "PING.EXE"), "-n", "30", "127.0.0.1")
	if parent.Start() != nil || foreign.Start() != nil {
		t.Fatal("start")
	}
	defer foreign.Process.Kill()
	time.Sleep(500 * time.Millisecond)
	var child uint32
	for _, c := range childrenOf()[uint32(parent.Process.Pid)] {
		if strings.EqualFold(filepath.Base(exePath(c)), "PING.EXE") {
			child = c
		}
	}
	if child == 0 {
		t.Fatal("child ping not found")
	}
	if n := killUnder(filepath.Dir(filepath.Dir(mine))); n != 2 {
		t.Fatalf("killed %d, want parent + child", n)
	}
	parent.Wait()
	time.Sleep(200 * time.Millisecond)
	if exePath(child) != "" {
		t.Fatal("child from another folder survived (orphaned php-cgi bug)")
	}
	if exePath(uint32(foreign.Process.Pid)) == "" {
		t.Fatal("a foreign process was killed")
	}
}
