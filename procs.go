package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Proc is a long-running project command (Laravel queue, Vite, Reverb...), like Laragon's Procfile.
type Proc struct {
	Name      string `json:"name"`
	Dir       string `json:"dir"` // folder in www
	Cmd       string `json:"cmd"`
	Autostart bool   `json:"autostart"` // start when DevPanel starts
}

type ProcStatus struct {
	Proc
	Running bool `json:"running"`
	PID     int  `json:"pid"`
}

// ringLog keeps the last 64 KB of a process's output.
type ringLog struct {
	mu  sync.Mutex
	buf []byte
}

func (r *ringLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - 64<<10; over > 0 {
		r.buf = r.buf[over:]
	}
	return len(p), nil
}

func (r *ringLog) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

type procRun struct {
	cmd  *exec.Cmd
	log  *ringLog
	done chan struct{}
}

func (a *App) Processes() []ProcStatus {
	a.procMu.Lock()
	defer a.procMu.Unlock()
	out := []ProcStatus{}
	for _, p := range a.cfg.Procs {
		st := ProcStatus{Proc: p}
		if r := a.procs[p.Name]; r != nil {
			select {
			case <-r.done:
			default:
				st.Running, st.PID = true, r.cmd.Process.Pid
			}
		}
		out = append(out, st)
	}
	return out
}

func (a *App) SaveProcess(p Proc) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	p.Name, p.Dir, p.Cmd = strings.TrimSpace(p.Name), strings.TrimSpace(p.Dir), strings.TrimSpace(p.Cmd)
	if p.Name == "" || p.Cmd == "" {
		return errors.New("name and command are required")
	}
	if strings.ContainsAny(p.Dir, `\/`) || p.Dir == ".." {
		return errors.New("folder must be a project folder in www")
	}
	if st, err := os.Stat(a.dirOf(p.Dir)); err != nil || !st.IsDir() {
		return fmt.Errorf("no folder %q in www", p.Dir)
	}
	i := slices.IndexFunc(a.cfg.Procs, func(x Proc) bool { return x.Name == p.Name })
	if i >= 0 {
		a.cfg.Procs[i] = p
	} else {
		a.cfg.Procs = append(a.cfg.Procs, p)
	}
	return a.saveConfig()
}

func (a *App) DeleteProcess(name string) error {
	a.StopProcess(name)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Procs = slices.DeleteFunc(a.cfg.Procs, func(x Proc) bool { return x.Name == name })
	return a.saveConfig()
}

// procEnv puts the selected environment versions (PHP, Node, ...) first on PATH.
func (a *App) procEnv() []string {
	env := append(os.Environ(), a.runtimeEnv()...)
	for i, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); strings.EqualFold(k, "PATH") {
			env[i] = "PATH=" + strings.Join(append(a.envPaths(), v), ";")
		}
	}
	return env
}

func (a *App) StartProcess(name string) error {
	i := slices.IndexFunc(a.cfg.Procs, func(x Proc) bool { return x.Name == name })
	if i < 0 {
		return fmt.Errorf("no process %q", name)
	}
	p := a.cfg.Procs[i]
	a.procMu.Lock()
	defer a.procMu.Unlock()
	if r := a.procs[name]; r != nil {
		select {
		case <-r.done:
		default:
			return nil // already running
		}
	}
	log := &ringLog{}
	cmd := exec.Command("cmd", "/c", p.Cmd)
	cmd.Dir = a.dirOf(p.Dir)
	cmd.Env = a.procEnv()
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	fmt.Fprintf(log, "$ %s  (in www\\%s)\n", p.Cmd, p.Dir)
	if err := cmd.Start(); err != nil {
		return err
	}
	r := &procRun{cmd, log, make(chan struct{})}
	go func() {
		err := cmd.Wait()
		fmt.Fprintf(log, "\n[exited: %v]\n", err)
		close(r.done)
	}()
	if a.procs == nil {
		a.procs = map[string]*procRun{}
	}
	a.procs[name] = r
	return nil
}

// StopProcess kills the process tree (cmd -> php/node children).
func (a *App) StopProcess(name string) error {
	a.procMu.Lock()
	r := a.procs[name]
	a.procMu.Unlock()
	if r == nil {
		return nil
	}
	select {
	case <-r.done:
		return nil
	default:
	}
	hidden(exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(r.cmd.Process.Pid))).Run()
	<-r.done
	return nil
}

func (a *App) ProcessLog(name string) string {
	a.procMu.Lock()
	defer a.procMu.Unlock()
	if r := a.procs[name]; r != nil {
		return r.log.String()
	}
	return ""
}

func (a *App) stopAllProcesses() {
	for _, p := range a.cfg.Procs {
		a.StopProcess(p.Name)
	}
}

// ProjectFolders lists www folders for the process form.
func (a *App) ProjectFolders() []string {
	out := []string{}
	if list, err := a.sites(); err == nil || len(list) > 0 {
		for _, s := range list {
			out = append(out, s.Folder)
		}
	}
	return out
}
