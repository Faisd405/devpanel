package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real git clone (from a local repo, no network) + real composer install with a borrowed PHP, in a sandbox.
// engine "none": no database is touched.
func TestRuntimeCloneSetup(t *testing.T) {
	if os.Getenv("DEVPANEL_RUNTIME") == "" {
		t.Skip("set DEVPANEL_RUNTIME=1")
	}
	phps, _ := filepath.Glob(`C:\laragon\bin\php\php-8*\php.exe`)
	if len(phps) == 0 {
		t.Skip("no PHP to borrow")
	}
	a, root := standaloneApp(t)
	a.cfg.Hostnames = map[string]string{}
	if err := makeJunction(filepath.Join(root, `bin\php\borrowed`), filepath.Dir(phps[0])); err != nil {
		t.Fatal(err)
	}
	a.cfg.Versions["php"] = "borrowed"
	git, err := a.gitExe()
	if err != nil {
		t.Skip(err)
	}
	// a small project: composer.json without dependencies, .env.example
	src := filepath.Join(t.TempDir(), "demo")
	os.MkdirAll(filepath.Join(src, "public"), 0755)
	os.WriteFile(filepath.Join(src, "composer.json"), []byte(`{"name":"demo/app","require":{"php":">=8.1"},"autoload":{"psr-4":{"App\\":"src/"}}}`), 0644)
	os.WriteFile(filepath.Join(src, ".env.example"), []byte("APP_NAME=Demo\nDB_CONNECTION=sqlite\n"), 0644)
	os.WriteFile(filepath.Join(src, `public\index.php`), []byte("<?php echo 'demo';"), 0644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init"}} {
		cmd := exec.Command(git, args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := a.streamCmd(a.www(), git, "clone", "--", src, "demo"); err != nil { // CloneProject minus the URL check
		t.Fatal(err)
	}
	notes, err := a.setupProject("demo", "none")
	if err != nil {
		t.Fatalf("%v (notes %v)", err, notes)
	}
	dir := filepath.Join(a.www(), "demo")
	if !exists(filepath.Join(dir, `vendor\autoload.php`)) || !exists(filepath.Join(dir, ".env")) {
		t.Fatalf("composer install / .env missing; notes %v", notes)
	}
	t.Logf("notes: %s", strings.Join(notes, " | "))
}
