package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGitURLAndFolder(t *testing.T) {
	for _, ok := range []string{"https://github.com/org/shop.git", "git@github.com:org/shop.git", "ssh://git@host:22/org/shop", "https://gitlab.com/a/b/c"} {
		if !gitURLRe.MatchString(ok) {
			t.Errorf("rejected %s", ok)
		}
	}
	for _, bad := range []string{"-oProxyCommand=calc", "--upload-pack=x", "file:///C:/x", `C:\repo`, "https://x/y z", "ext::sh -c x"} {
		if gitURLRe.MatchString(bad) {
			t.Errorf("accepted %s", bad)
		}
	}
	for u, want := range map[string]string{"https://github.com/org/shop.git": "shop", "git@github.com:org/my.app.git": "my-app", "https://gitlab.com/a/b/c/": "c"} {
		if got := repoFolder(u); got != want {
			t.Errorf("repoFolder(%s) = %s, want %s", u, got, want)
		}
	}
	a, _ := standaloneApp(t)
	if _, err := a.CloneProject("-oProxyCommand=calc", "", ""); err == nil {
		t.Fatal("option-like URL accepted")
	}
}

// setupProject on a cloned folder: .env from the example, database created (stubbed) and wired into .env.
func TestSetupProject(t *testing.T) {
	defer func(o func(*App, string, string) error) { createDatabase = o }(createDatabase)
	var created []string
	createDatabase = func(_ *App, engine, name string) error { created = append(created, engine+":"+name); return nil }
	a, root := standaloneApp(t)
	a.cfg.Hostnames = map[string]string{}
	os.MkdirAll(filepath.Join(root, `bin\postgresql\pg17`), 0755)
	dir := filepath.Join(root, `www\my-app`)
	touch(t, filepath.Join(dir, ".env.example"))
	os.WriteFile(filepath.Join(dir, ".env.example"), []byte("APP_NAME=App\nDB_CONNECTION=pgsql\nDB_DATABASE=laravel\n"), 0644)
	notes, err := a.setupProject("my-app", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(created, []string{"postgresql:my_app"}) {
		t.Fatalf("created %v", created)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	for _, want := range []string{"APP_NAME=App", "DB_CONNECTION=pgsql", "DB_DATABASE=my_app", "DB_USERNAME=postgres"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s in .env:\n%s\nnotes: %v", want, b, notes)
		}
	}
	if ex, _ := os.ReadFile(filepath.Join(dir, ".env.example")); strings.Contains(string(ex), "my_app") {
		t.Fatal(".env.example was changed")
	}
}

// A PHP constraint nothing installed meets stops before composer install, with a suggestion, not an install.
func TestSetupProjectWrongPHP(t *testing.T) {
	defer func(o func(*App, string, string) []Package) { onlineLookup = o }(onlineLookup)
	onlineLookup = func(*App, string, string) []Package { return nil }
	a, root := standaloneApp(t)
	touch(t, filepath.Join(root, `bin\php\php-8.4.1\php-cgi.exe`))
	dir := filepath.Join(root, `www\old`)
	touch(t, filepath.Join(dir, "composer.json"))
	os.WriteFile(filepath.Join(dir, "composer.json"), []byte(`{"require":{"php":"^7.2"}}`), 0644)
	if _, err := a.setupProject("old", ""); err == nil || !strings.Contains(err.Error(), "no installed PHP fits ^7.2") {
		t.Fatalf("err = %v", err)
	}
	if exists(filepath.Join(dir, "vendor")) {
		t.Fatal("composer ran on the wrong PHP")
	}
}

func TestBackupJobs(t *testing.T) {
	defer func(d func(*App, string, string, string) (string, error), r func(*App, string) bool) { backupDump, backupRunning = d, r }(backupDump, backupRunning)
	var fail error
	backupDump = func(_ *App, _, _, file string) (string, error) {
		if fail != nil {
			return "", fail
		}
		return "", os.WriteFile(file, []byte("--"), 0644)
	}
	backupRunning = func(*App, string) bool { return true }
	a, _ := standaloneApp(t)
	for _, bad := range [][2]string{{"oracle", "shop"}, {"mysql", "a;b"}} {
		if a.SetBackupJob(bad[0], bad[1], "daily", 2) == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	if a.SetBackupJob("mysql", "shop", "monthly", 2) == nil {
		t.Fatal("accepted monthly")
	}
	if err := a.SetBackupJob("mysql", "shop", "daily", 2); err != nil {
		t.Fatal(err)
	}
	dir := a.backupDir("mysql")
	os.MkdirAll(dir, 0755)
	manual := filepath.Join(dir, "shop_20200101-000000.sql")      // made by hand: never pruned
	other := filepath.Join(dir, "shop_old_auto_20200101-000000.sql") // another database with the same prefix
	os.WriteFile(manual, nil, 0644)
	os.WriteFile(other, nil, 0644)

	t0 := time.Date(2026, 11, 12, 3, 0, 0, 0, time.Local)
	for i, at := range []time.Time{t0, t0.Add(time.Hour), t0.Add(25 * time.Hour), t0.Add(50 * time.Hour)} {
		done := a.runBackupJobs(at)
		if due := i != 1; due != (len(done) == 1) {
			t.Fatalf("run %d at %s: %v", i, at, done)
		}
	}
	autos, _ := filepath.Glob(filepath.Join(dir, "shop_auto_*.sql"))
	if len(autos) != 2 || !exists(manual) || !exists(other) {
		t.Fatalf("retention: autos=%v manual=%v other=%v", autos, exists(manual), exists(other))
	}
	// failure is recorded and shown in Check; a late job warns
	fail = errors.New("mysqldump: access denied")
	a.runBackupJobs(t0.Add(100 * time.Hour))
	if c := a.backupChecks(t0.Add(100 * time.Hour)); len(c) != 1 || c[0].Level != "error" {
		t.Fatalf("checks after failure: %+v", c)
	}
	fail = nil
	a.runBackupJobs(t0.Add(101 * time.Hour))
	if c := a.backupChecks(t0.Add(160 * time.Hour)); len(c) != 1 || c[0].Level != "warn" {
		t.Fatalf("checks when late: %+v", c)
	}
	if err := a.SetBackupJob("mysql", "shop", "", 0); err != nil || len(a.BackupJobs()) != 0 {
		t.Fatalf("remove: %v %v", err, a.BackupJobs())
	}
}
