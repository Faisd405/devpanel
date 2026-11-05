package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A standalone install from nothing: real Apache 2.4.69 + mod_fcgid downloaded into an empty folder,
// DevPanel writes every config itself. Only PHP is linked from this machine (a junction) to save a download.
func TestRuntimeStandaloneApache(t *testing.T) {
	runtimeOnly(t)
	defer setPortOverrides(nil)
	root := t.TempDir()
	a := &App{cfg: Config{Root: root, TLD: "test", Versions: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	a.ensureLayout()
	junction(t, filepath.Join(root, `bin\php`, php85), filepath.Join(realRoot, `bin\php`, php85))
	os.MkdirAll(filepath.Join(root, `www\shop\public`), 0755)
	os.WriteFile(filepath.Join(root, `www\shop\public\index.php`), []byte("<?php echo 'standalone ', PHP_VERSION, ' ', $_SERVER['SERVER_NAME'];"), 0644)
	setPortOverrides(map[string]int{"apache": freePort(t), "apache_ssl": freePort(t)})

	zip := cachedDownload(t, a, "httpd-2.4.69.zip", "https://www.apachelounge.com/download/VS18/binaries/httpd-2.4.69-261002-Win64-VS18.zip")
	dest := filepath.Join(root, `bin\apache\httpd-2.4.69-261002-Win64-VS18`)
	installInto(t, a, zip, dest)
	if err := a.postInstall(`bin\apache`, dest); err != nil { // same step the Install button runs
		t.Fatal(err)
	}
	if !exists(a.fcgidModule()) {
		t.Fatal("mod_fcgid was not downloaded")
	}
	if _, _, err := a.syncVhostFiles(); err != nil {
		t.Fatal(err)
	}
	if out, err := output(filepath.Join(dest, `bin\httpd.exe`), "-t"); err != nil || !strings.Contains(out, "Syntax OK") {
		t.Fatalf("httpd -t: %v\n%s", err, out)
	}
	if err := a.start("apache"); err != nil {
		b, _ := os.ReadFile(filepath.Join(dest, `logs\error.log`))
		t.Fatalf("%v\n%s", err, b)
	}
	defer a.stop("apache")
	r := get(t, fmt.Sprintf("http://127.0.0.1:%d/", port("apache")), "shop.test")
	t.Logf("standalone apache: %s", r)
	if !strings.HasPrefix(r, "200 standalone 8.5.") || !strings.Contains(r, "shop.test") {
		t.Fatal("standalone Apache did not serve the PHP site")
	}
	if !a.running("apache") {
		t.Fatal("running() should see the standalone Apache")
	}
}
