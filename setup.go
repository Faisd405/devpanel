package main

// Installer / uninstaller built into the same exe (no external tools):
//   DevPanel-Setup.exe (or devpanel.exe --install): copy to a folder, Start Menu + Desktop shortcuts,
//   "Apps & features" entry (per user, no admin).
//   devpanel.exe --uninstall: remove those again. Projects, databases and installed services are never deleted.
// Portable: just run devpanel.exe from any folder; it keeps devpanel.json and its stack next to itself.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const Version = "1.3.0"

func (a *App) Version() string { return Version }

var uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\DevPanel` // var: tests use a scratch key

// isSetupExe: the downloaded installer is the same binary, recognised by its file name.
func isSetupExe() bool {
	exe, _ := os.Executable()
	return strings.Contains(strings.ToLower(filepath.Base(exe)), "setup")
}

const (
	mbOK, mbYesNo, mbYesNoCancel                    = 0x0, 0x4, 0x3
	mbIconInfo, mbIconQuestion, mbIconError         = 0x40, 0x20, 0x10
	idYes, idNo, idCancel                           = 6, 7, 2
	mbTopmost, mbSetForeground              uintptr = 0x40000, 0x10000
)

func msgBox(text string, flags uintptr) int {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString("DevPanel " + Version + " Setup")
	r, _, _ := windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags|mbTopmost|mbSetForeground)
	return int(r)
}

// pickFolder shows the Windows folder dialog (via PowerShell, so no extra GUI code here).
func pickFolder(initial string) string {
	ps := `Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
		`$d.Description = 'Choose where to install DevPanel'; $d.SelectedPath = $env:DP_DIR; $d.ShowNewFolderButton = $true; ` +
		`if ($d.ShowDialog() -eq 'OK') { $d.SelectedPath }`
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", ps)
	cmd.Env = append(os.Environ(), "DP_DIR="+initial)
	out, _ := hidden(cmd).Output()
	return strings.TrimSpace(string(out))
}

// chooseInstallDir: next to Laragon (compatible mode) or standalone in C:\DevPanel (or a folder the user picks).
func chooseInstallDir() (string, bool) {
	// Other environments only need a hint: in either mode their installs can be linked later.
	var others []string
	for _, s := range externalSources() {
		if s.name != "Laragon" && slices.ContainsFunc(s.roots, exists) {
			others = append(others, s.name)
		}
	}
	hint := ""
	if len(others) > 0 {
		hint = "\n\nAlso found: " + strings.Join(others, ", ") + ". After installing, link their PHP, databases and projects in " +
			"Settings → Other environments on this PC (nothing is moved or changed)."
	}
	if exists(`C:\laragon\laragon.exe`) {
		switch msgBox("Laragon was found in C:\\laragon.\n\nYes: install next to it (C:\\laragon\\devpanel) and use Laragon's PHP, services and projects.\n"+
			"No: install standalone with its own folders (Laragon's installs can still be linked).\nCancel: stop."+hint, mbYesNoCancel|mbIconQuestion) {
		case idYes:
			return `C:\laragon\devpanel`, true
		case idCancel:
			return "", false
		}
	}
	def := `C:\DevPanel`
	switch msgBox("Install DevPanel standalone to "+def+"?\n\nYes: install there.\nNo: choose another folder.\nCancel: stop.\n\n"+
		"Use a path without spaces (Apache, PHP and others are happier that way)."+hint, mbYesNoCancel|mbIconQuestion) {
	case idYes:
		return def, true
	case idNo:
		if d := pickFolder(def); d != "" {
			return d, true
		}
	}
	return "", false
}

func copyExe(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func shortcutDirs() []string {
	return []string{filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`), filepath.Join(os.Getenv("USERPROFILE"), "Desktop")}
}

func makeShortcuts(exe string) error {
	script := `$s = New-Object -ComObject WScript.Shell
foreach ($p in @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop'))) {
  $l = $s.CreateShortcut((Join-Path $p 'DevPanel.lnk'))
  $l.TargetPath = $env:DP_EXE; $l.WorkingDirectory = (Split-Path $env:DP_EXE); $l.IconLocation = $env:DP_EXE + ',0'; $l.Save()
}`
	cmd := hidden(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script))
	cmd.Env = append(os.Environ(), "DP_EXE="+exe)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v %s", err, out)
	}
	return nil
}

func registerUninstall(dir, exe string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	size := uint32(0)
	if st, err := os.Stat(exe); err == nil {
		size = uint32(st.Size() / 1024)
	}
	for k2, v := range map[string]string{"DisplayName": "DevPanel", "DisplayVersion": Version, "Publisher": "DevPanel",
		"InstallLocation": dir, "DisplayIcon": exe + ",0", "UninstallString": `"` + exe + `" --uninstall`} {
		if err := k.SetStringValue(k2, v); err != nil {
			return err
		}
	}
	k.SetDWordValue("NoModify", 1)
	k.SetDWordValue("NoRepair", 1)
	k.SetDWordValue("EstimatedSize", size)
	return nil
}

// runSetup installs this exe; returns the process exit code.
func runSetup() int {
	dir, ok := chooseInstallDir()
	if !ok {
		return 1
	}
	target := filepath.Join(dir, "devpanel.exe")
	if _, running := readAgent(dir); running {
		msgBox("DevPanel is running from "+dir+".\nQuit it from the tray menu first, then run Setup again.", mbOK|mbIconError)
		return 1
	}
	self, _ := os.Executable()
	err := os.MkdirAll(dir, 0755)
	if err == nil && !strings.EqualFold(filepath.Clean(self), filepath.Clean(target)) {
		if exists(target) { // update: move the old exe aside (works even if Windows still holds it)
			os.Remove(target + ".old")
			os.Rename(target, target+".old")
		}
		err = copyExe(self, target)
	}
	if err == nil {
		err = makeShortcuts(target)
	}
	if err == nil {
		err = registerUninstall(dir, target)
	}
	if err != nil {
		msgBox("Installation failed:\n"+err.Error(), mbOK|mbIconError)
		return 1
	}
	msgBox("DevPanel "+Version+" is installed in "+dir+".\nShortcuts are on the Start Menu and Desktop. It starts now.", mbOK|mbIconInfo)
	exec.Command(target).Start()
	return 0
}

// runUninstall removes DevPanel's program files, shortcuts and registry entries; data is kept.
func runUninstall(a *App) int {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	if msgBox("Uninstall DevPanel from "+dir+"?\n\nYour projects (www), databases (data) and installed services (bin) are kept.\n"+
		"Running services keep running; stop them first if you want.", mbYesNo|mbIconQuestion) != idYes {
		return 1
	}
	if info, ok := readAgent(dir); ok { // ask the agent to quit (stops project processes and shares)
		p := &Proxy{cfgDir: dir, info: info}
		p.call("QuitAgent", nil)
	}
	for _, d := range shortcutDirs() {
		os.Remove(filepath.Join(d, "DevPanel.lnk"))
	}
	registry.DeleteKey(registry.CURRENT_USER, uninstallKey)
	setAutostart(false)
	for _, f := range []string{"agent.json", "devpanel.exe.old"} {
		os.Remove(filepath.Join(dir, f))
	}
	msgBox("DevPanel was removed.\n\nKept: settings (devpanel.json) and everything in "+a.cfg.Root+" (projects, databases, services).\n"+
		"Delete them yourself if you no longer need them.", mbOK|mbIconInfo)
	// An exe can't delete itself while running: let cmd remove it a moment after we exit.
	exec.Command("cmd", "/c", "ping -n 3 127.0.0.1 >nul & del /f /q \""+exe+"\"").Start()
	return 0
}

// QuitAgent stops the background agent (used by the uninstaller and the tray's Quit).
func (a *App) QuitAgent() error {
	if a.quitCh == nil {
		return errors.New("not running as agent")
	}
	a.quitAgent()
	return nil
}
