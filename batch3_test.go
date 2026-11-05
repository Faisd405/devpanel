package main

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestXdebugPick(t *testing.T) {
	page := `php_xdebug-3.5.3-8.4-nts-vs17-x86_64.dll php_xdebug-3.4.9-8.4-nts-vs17-x86_64.dll
php_xdebug-3.6.0alpha1-8.4-nts-vs17-x86_64.dll php_xdebug-3.5.3-8.4-ts-vs17-x86_64.dll php_xdebug-3.10.0-8.3-nts-vs16-x86_64.dll`
	cases := map[string]string{
		"php-8.4.5-nts-Win32-vs17-x64":    "php_xdebug-3.5.3-8.4-nts-vs17-x86_64.dll",
		"php-8.4.5-Win32-vs17-x64":        "php_xdebug-3.5.3-8.4-ts-vs17-x86_64.dll",
		"php-8.3.16-nts-Win32-vs16-x64":   "php_xdebug-3.10.0-8.3-nts-vs16-x86_64.dll",
		"php-8.5.12-1-nts-Win32-vs17-x64": "",
	}
	for folder, want := range cases {
		re, err := xdebugPattern(folder)
		if err != nil {
			t.Fatal(folder, err)
		}
		if got := newestStable(page, re); got != want {
			t.Errorf("%s: %q want %q", folder, got, want)
		}
	}
	if _, err := xdebugPattern("php"); err == nil {
		t.Fatal("unparseable folder accepted")
	}
	if !verLess("3.9.1", "3.10.0") || verLess("8.4.10", "8.4.9") || verLess("1.2", "1.2.0") {
		t.Fatal("verLess")
	}
}

func TestUpdateHelpers(t *testing.T) {
	got := installedPHP([]string{"php-8.4.5-nts-Win32-vs17-x64", "php-8.4.12-nts-Win32-vs17-x64", "php-8.3.16-Win32-vs16-x64", "pear"})
	if got["8.4"] != "8.4.12" || got["8.3"] != "8.3.16" || len(got) != 2 {
		t.Fatalf("installedPHP = %v", got)
	}
	sums := "abc123  node-v22.1.0-x64.msi\ndef456  node-v22.1.0-win-x64.zip\n"
	if shaFor(sums, "node-v22.1.0-win-x64.zip") != "def456" || shaFor(sums, "nope") != "" {
		t.Fatal("shaFor")
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("php-cgi.exe")
	f.Write([]byte("x"))
	w.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.Write(buf.Bytes()) }))
	defer srv.Close()

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr"), 0755)
	os.WriteFile(filepath.Join(root, `usr\packages.conf`), nil, 0644)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}}
	p := a.pkg("PHP-9.9.9", srv.URL+"/php-9.9.9-nts-Win32-vs17-x64.zip", "php", "Updates")

	p.SHA256 = strings.Repeat("0", 64)
	a.updates = []Package{p}
	if _, err := a.Install("PHP-9.9.9"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("bad checksum accepted: %v", err)
	}
	if exists(filepath.Join(root, `bin\php\php-9.9.9-nts-Win32-vs17-x64`)) {
		t.Fatal("files installed despite checksum mismatch")
	}

	tmp := filepath.Join(t.TempDir(), "z")
	os.WriteFile(tmp, buf.Bytes(), 0644)
	p.SHA256, _ = fileSHA256(tmp)
	a.updates = []Package{p}
	if _, err := a.Install("PHP-9.9.9"); err != nil && !strings.Contains(err.Error(), "setup failed") {
		t.Fatalf("good checksum rejected: %v", err)
	}
	if !exists(filepath.Join(root, `bin\php\php-9.9.9-nts-Win32-vs17-x64\php-cgi.exe`)) {
		t.Fatal("not installed with a valid checksum")
	}
}

func TestMySQLEnv(t *testing.T) {
	defer setMySQLPassword("")
	setMySQLPassword("s3cret")
	has := func(c *exec.Cmd) bool {
		for _, e := range c.Env {
			if e == "MYSQL_PWD=s3cret" {
				return true
			}
		}
		return false
	}
	if !has(withMySQLEnv(exec.Command(`C:\x\bin\mysqldump.exe`))) || !has(withMySQLEnv(exec.Command(`C:\x\bin\mysql.exe`))) {
		t.Fatal("mysql tools miss MYSQL_PWD")
	}
	if has(withMySQLEnv(exec.Command(`C:\x\bin\psql.exe`))) {
		t.Fatal("password leaked to a non-mysql tool")
	}
	if a := (&App{}); func() error { _, err := a.SetMySQLPassword("a'b", "a'b"); return err }() == nil {
		t.Fatal("quote in password accepted")
	}
}

func TestWatchdogWanted(t *testing.T) {
	a := &App{cfg: Config{Root: t.TempDir(), Versions: map[string]string{}}, wanted: map[string]bool{"redis": true}}
	a.stop("redis") // not installed/running: must still clear the wish
	if a.wanted["redis"] {
		t.Fatal("stopped service still wanted by the watchdog")
	}
}
