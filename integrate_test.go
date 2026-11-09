package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenPIDsAndOwner(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := l.Addr().(*net.TCPAddr).Port
	if pid := listenPIDs()[p]; pid != uint32(os.Getpid()) {
		t.Fatalf("port %d: pid %d, want %d", p, pid, os.Getpid())
	}
	if l6, err := net.Listen("tcp", "[::1]:0"); err == nil { // IPv6 rows have another layout
		defer l6.Close()
		if pid := listenPIDs()[l6.Addr().(*net.TCPAddr).Port]; pid != uint32(os.Getpid()) {
			t.Fatalf("ipv6 pid %d", pid)
		}
	}
	_, herd, _ := fakeEnvs(t)
	a, _ := standaloneApp(t)
	if o := a.ownerOf(filepath.Join(filepath.Dir(herd), `nginx\nginx.exe`)); o != "Laravel Herd (nginx.exe)" { // <herd>\bin's parent
		t.Fatalf("owner = %q", o)
	}
	if o := a.ownerOf(`D:\tools\foo.exe`); o != "foo.exe" {
		t.Fatalf("owner = %q", o)
	}
}

// Same version in two places is flagged, auto-link links the rest and drops links of removed versions.
func TestSameVersionAndAutoLink(t *testing.T) {
	_, herd, _ := fakeEnvs(t)
	os.Rename(filepath.Join(herd, "php84"), filepath.Join(herd, "php-8.4.12"))
	a, root := standaloneApp(t)
	touch(t, filepath.Join(root, `bin\php\php-8.4.12\php-cgi.exe`)) // DevPanel has 8.4.12 itself
	items := map[string]ExternalItem{}
	for _, e := range a.ExternalEnvironments() {
		if e.Name == "Laravel Herd" {
			for _, it := range e.Items {
				items[it.LinkName] = it
			}
		}
	}
	if items["herd-php-8.4.12"].Same != "php-8.4.12" || items["herd-php83"].Same != "" {
		t.Fatalf("same: %+v", items)
	}
	if err := a.SetAutoLink("Laravel Herd", true); err != nil {
		t.Fatal(err)
	}
	if !isLink(filepath.Join(root, `bin\php\herd-php83`)) || exists(filepath.Join(root, `bin\php\herd-php-8.4.12`)) {
		t.Fatal("auto-link must link new versions and skip ones DevPanel already has")
	}
	// Herd removes php83: the dead link goes, Herd's other files stay.
	os.RemoveAll(filepath.Join(herd, "php83"))
	if msg, err := a.SyncExternal(); err != nil || !strings.Contains(msg, "unlinked herd-php83") {
		t.Fatalf("sync: %q %v", msg, err)
	}
	if exists(filepath.Join(root, `bin\php\herd-php83`)) || !exists(filepath.Join(herd, `php-8.4.12\php-cgi.exe`)) {
		t.Fatal("dead link not removed / wrong files touched")
	}
}

// DevPanel's versions appear in Laragon as devpanel-* links, are not scanned back, and go away when turned off.
func TestShareWithLaragon(t *testing.T) {
	lar := filepath.Join(t.TempDir(), `laragon\bin`)
	touch(t, filepath.Join(lar, `php\php-8.3.16\php-cgi.exe`)) // Laragon's own
	orig := externalSources
	t.Cleanup(func() { externalSources = orig })
	externalSources = func() []extSource { return []extSource{{"Laragon", "laragon", "", []string{lar}, 4}} }
	a, root := standaloneApp(t)
	touch(t, filepath.Join(root, `bin\php\php-8.5.12\php-cgi.exe`))
	touch(t, filepath.Join(root, `bin\php\php-8.3.16\php-cgi.exe`)) // Laragon has this version already
	if err := a.SetShareLaragon(true); err != nil {
		t.Fatal(err)
	}
	if !isLink(filepath.Join(lar, `php\devpanel-php-8.5.12`)) || exists(filepath.Join(lar, `php\devpanel-php-8.3.16`)) {
		t.Fatal("share: want devpanel-php-8.5.12 only")
	}
	for _, e := range a.ExternalEnvironments() {
		for _, it := range e.Items {
			if strings.Contains(it.Dir, "devpanel-") {
				t.Fatalf("own shared version scanned back: %s", it.Dir)
			}
		}
	}
	if err := a.SetShareLaragon(false); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(lar, `php\devpanel-php-8.5.12`)) || !exists(filepath.Join(root, `bin\php\php-8.5.12\php-cgi.exe`)) {
		t.Fatal("unshare must remove only the link")
	}
}

func TestUseDevPanelDB(t *testing.T) {
	_, herd, _ := fakeEnvs(t)
	a, root := standaloneApp(t)
	os.MkdirAll(filepath.Join(root, `bin\mysql\mysql-8.4.3`), 0755)
	os.MkdirAll(filepath.Join(root, `bin\postgresql\pg17`), 0755)
	a.cfg.MySQLPassword = "pw"
	site := filepath.Join(root, `www\shop`)
	touch(t, filepath.Join(site, ".env"))
	os.WriteFile(filepath.Join(site, ".env"), []byte("APP_NAME=Shop\r\nDB_CONNECTION=sqlite\r\n# DB_HOST=127.0.0.1\r\nREDIS_HOST=redis\r\n"), 0644)
	msg, err := a.UseDevPanelDB("shop", "")
	if err != nil || !strings.Contains(msg, "DB_CONNECTION=sqlite") {
		t.Fatalf("%q %v", msg, err)
	}
	b, _ := os.ReadFile(filepath.Join(site, ".env"))
	for _, want := range []string{"APP_NAME=Shop\r\n", "DB_CONNECTION=mysql\r\n", "DB_HOST=127.0.0.1\r\n", "DB_DATABASE=shop", "DB_PASSWORD=pw", "REDIS_HOST=127.0.0.1"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in\n%s", want, b)
		}
	}
	// A Herd project (outside DevPanel's sites), by full path: only inside a known projects folder.
	herdWWW := t.TempDir()
	orig := externalSources
	externalSources = func() []extSource { return []extSource{{"Laravel Herd", "herd", herdWWW, []string{herd}, 3}} }
	defer func() { externalSources = orig }()
	touch(t, filepath.Join(herdWWW, `blog\.env`))
	if got := a.ProjectsIn(herdWWW); len(got) != 1 || got[0] != "blog" {
		t.Fatalf("ProjectsIn = %v", got)
	}
	if _, err := a.UseDevPanelDB(filepath.Join(herdWWW, "blog"), "pgsql"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(herdWWW, `blog\.env`)); !strings.Contains(string(b), "DB_CONNECTION=pgsql") || !strings.Contains(string(b), "DB_USERNAME=postgres") {
		t.Fatalf("herd .env:\n%s", b)
	}
	if _, err := a.UseDevPanelDB(t.TempDir(), "mysql"); err == nil {
		t.Fatal("a folder outside the projects folders was accepted")
	}
	if _, err := a.UseDevPanelDB("shop", "sqlsrv"); err == nil {
		t.Fatal("bad engine accepted")
	}
}

// A linked service's port is changed only after confirmation, in that environment's own config file.
func TestSetPortLinked(t *testing.T) {
	defer setPortOverrides(nil)
	xampp, _, _ := fakeEnvs(t)
	a, _ := standaloneApp(t)
	ini := filepath.Join(xampp, `mysql\bin\my.ini`)
	os.WriteFile(ini, []byte("[client]\nport=3306\n[mysqld]\nport=3306\n"), 0644)
	a.LinkExternal("mysql", filepath.Join(xampp, "mysql"))
	a.cfg.Versions["mysql"] = "xampp-mysql"
	if err := a.SetPort("mysql", 3398); err == nil || !strings.HasPrefix(err.Error(), "linked:") {
		t.Fatalf("SetPort: %v", err)
	}
	if b, _ := os.ReadFile(ini); strings.Contains(string(b), "3398") {
		t.Fatal("changed without confirmation")
	}
	if err := a.SetPortLinked("mysql", 3398); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ini); strings.Count(string(b), "port=3398") != 2 || port("mysql") != 3398 {
		t.Fatalf("my.ini:\n%s", b)
	}
}
