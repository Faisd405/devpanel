package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// Languages & runtimes in the Services list (they have no process, like PHP: just versions, PATH and tools).
var runtimeNames = []string{"nodejs", "go", "python", "java", "dotnet", "ruby", "bun", "deno", "erlang"}

type RuntimeInfo struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Version  string   `json:"version"`
	Versions []string `json:"versions"`
	Category string   `json:"category"`
}

// Runtimes is cheap (folder listing only), so the Services list can poll it with the status.
func (a *App) Runtimes() []RuntimeInfo {
	out := []RuntimeInfo{}
	for _, n := range runtimeNames {
		out = append(out, RuntimeInfo{n, serviceLabel(n), folderName(a.svcDir(n)), a.versions(n), categoryOf(n)})
	}
	return out
}

// runtimeVersionCmds: commands that print the version(s); paths are relative to the version folder.
var runtimeVersionCmds = map[string][][]string{
	"nodejs": {{"node.exe", "-v"}, {"npm.cmd", "-v"}},
	"go":     {{`bin\go.exe`, "version"}},
	"python": {{"python.exe", "--version"}, {`Scripts\pip.exe`, "--version"}},
	"java":   {{`bin\java.exe`, "-version"}},
	"dotnet": {{"dotnet.exe", "--version"}},
	"ruby":   {{`bin\ruby.exe`, "-v"}, {`bin\gem.cmd`, "-v"}},
	"bun":    {{"bun.exe", "--version"}},
	"deno":   {{"deno.exe", "--version"}},
	"erlang": {{`bin\erl.exe`, "-noshell", "-eval", `io:format("OTP ~s~n",[erlang:system_info(otp_release)]), halt().`}},
}

var runtimeEnvVar = map[string]string{"java": "JAVA_HOME", "dotnet": "DOTNET_ROOT", "erlang": "ERLANG_HOME"}

func isRuntime(name string) bool {
	for _, n := range runtimeNames {
		if n == name {
			return true
		}
	}
	return false
}

// runtimeDetail fills the detail panel of a language/runtime.
func (a *App) runtimeDetail(d *Detail) {
	dir := d.Dir
	for _, t := range envTools {
		if t.Name == d.Name {
			d.Info = append(d.Info, [2]string{"On PATH", strings.Join(a.toolPaths(t.Name, t.Paths), "; ")})
		}
	}
	if v := runtimeEnvVar[d.Name]; v != "" {
		d.Info = append(d.Info, [2]string{v, dir})
	}
	d.Info = append(d.Info, [2]string{"Used by", "DevPanel terminal, Processes, and Windows PATH (Environment → Apply)"})
	d.Folders = []FileRef{ref(d.Label+" folder", dir, false)}
	d.Tools = []Tool{{"version", "Show version", ""}, {"terminal", "Open terminal", ""}}
}

// runtimeTool runs a runtime's version commands (or opens a terminal with it on PATH).
func (a *App) runtimeTool(name, id string) (string, error) {
	dir := a.svcDir(name)
	switch id {
	case "terminal":
		return "terminal opened", a.openConsole(a.www())
	case "version":
		var out []string
		for _, c := range runtimeVersionCmds[name] {
			exe := filepath.Join(dir, c[0])
			if !exists(exe) {
				continue
			}
			var res string
			var err error
			if strings.HasSuffix(exe, ".cmd") { // npm.cmd, gem.cmd need cmd.exe, with this runtime first on PATH
				cmd := hidden(exec.Command("cmd", append([]string{"/c", exe}, c[1:]...)...))
				cmd.Env = a.procEnv()
				b, e := cmd.CombinedOutput()
				res, err = strings.TrimSpace(string(b)), e
			} else {
				res, err = output(exe, c[1:]...)
			}
			if err != nil && res == "" {
				res = err.Error()
			}
			out = append(out, filepath.Base(c[0])+": "+firstNonEmpty(res))
		}
		if len(out) == 0 {
			return "", errors.New(name + " is not installed")
		}
		return strings.Join(out, "\n"), nil
	}
	return "", errors.New("unknown tool " + name + "/" + id)
}
