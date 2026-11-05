package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows/registry"
)

func TestSetupHelpers(t *testing.T) {
	if isSetupExe() {
		t.Fatal("the test binary is not an installer")
	}
	// Apps & features entry, written to a scratch key (never the real one) and removed again.
	defer func(k string) { uninstallKey = k }(uninstallKey)
	uninstallKey = `Software\DevPanelTest\Uninstall\DevPanel`
	defer func() { // children first: a key with subkeys can't be deleted
		for _, k := range []string{`Software\DevPanelTest\Uninstall\DevPanel`, `Software\DevPanelTest\Uninstall`, `Software\DevPanelTest`} {
			registry.DeleteKey(registry.CURRENT_USER, k)
		}
	}()
	dir := t.TempDir()
	exe := filepath.Join(dir, "devpanel.exe")
	os.WriteFile(exe, make([]byte, 4096), 0644)
	if err := registerUninstall(dir, exe); err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, uninstallKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	for name, want := range map[string]string{"DisplayName": "DevPanel", "DisplayVersion": Version, "InstallLocation": dir, "UninstallString": `"` + exe + `" --uninstall`} {
		if got, _, _ := k.GetStringValue(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if n, _, _ := k.GetIntegerValue("EstimatedSize"); n != 4 {
		t.Errorf("EstimatedSize = %d KB", n)
	}
	// copyExe
	dst := filepath.Join(t.TempDir(), "copy.exe")
	if err := copyExe(exe, dst); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dst); st == nil || st.Size() != 4096 {
		t.Fatal("copyExe")
	}
}
