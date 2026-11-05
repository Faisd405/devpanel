package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sys/windows"
)

const cliHelp = `DevPanel CLI
  devpanel status                 services and their state
  devpanel start <service|group|all>  e.g. devpanel start "PG + Apache"
  devpanel stop <service|all>
  devpanel groups                 list groups
  devpanel sync                   sync vhosts (+ hosts, asks for admin)
`

// attachConsole lets the GUI-subsystem exe print to the terminal it was started from.
func attachConsole() io.Writer {
	// Redirected (pipe/file, also Git Bash/mintty): the inherited handle works as is.
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		return os.Stdout
	}
	const attachParent = ^uintptr(0) // ATTACH_PARENT_PROCESS
	if r, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole").Call(attachParent); r != 0 {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			return f
		}
	}
	return os.Stdout
}

func (a *App) cli(args []string) int {
	return a.runCLI(attachConsole(), args)
}

func (a *App) runCLI(w io.Writer, args []string) int {
	fail := func(err error) int {
		if err == nil {
			fmt.Fprintln(w, "ok")
			return 0
		}
		fmt.Fprintln(w, "error:", err)
		return 1
	}
	arg := strings.Join(args[1:], " ")
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(w, "DevPanel", Version)
		return 0
	case "status":
		for _, s := range a.Status() {
			state := "stopped"
			if s.Running {
				state = "running"
			}
			fmt.Fprintf(w, "%-14s %-8s :%-6d %s\n", s.Name, state, s.Port, s.Version)
		}
		return 0
	case "groups":
		names := make([]string, 0, len(a.cfg.Groups))
		for n := range a.cfg.Groups {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(w, "%-24s %s\n", n, strings.Join(a.cfg.Groups[n], " + "))
		}
		return 0
	case "start":
		if arg == "all" {
			note, err := a.StartAll()
			if note != "" {
				fmt.Fprintln(w, note)
			}
			return fail(err)
		}
		if _, ok := a.cfg.Groups[arg]; ok {
			return fail(a.StartGroup(arg))
		}
		if svc(arg).Name != "" {
			return fail(a.Start(arg))
		}
		return fail(fmt.Errorf("no service or group %q (see: devpanel groups)", arg))
	case "stop":
		if arg == "all" {
			var last error
			for _, s := range services {
				if a.running(s.Name) {
					if err := a.Stop(s.Name); err != nil {
						last = err
					}
				}
			}
			return fail(last)
		}
		if svc(arg).Name == "" {
			return fail(fmt.Errorf("no service %q", arg))
		}
		return fail(a.Stop(arg))
	case "sync":
		msg, err := a.SyncVhosts()
		if err == nil {
			fmt.Fprintln(w, msg)
		}
		return fail(err)
	}
	if !slices.Contains([]string{"help", "-h", "--help", "/?"}, args[0]) {
		fmt.Fprintf(w, "unknown command %q\n\n", args[0])
	}
	fmt.Fprint(w, cliHelp)
	return 2
}
