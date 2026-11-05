package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func touch(t *testing.T, p string) {
	os.MkdirAll(filepath.Dir(p), 0755)
	if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
}

// fakeEnvs builds XAMPP, Herd and FlyEnv layouts in a temp dir and points the scanner at them.
func fakeEnvs(t *testing.T) (xampp, herd, fly string) {
	base := t.TempDir()
	xampp, herd, fly = filepath.Join(base, "xampp"), filepath.Join(base, `herd\bin`), filepath.Join(base, `FlyEnv-Data\app`)
	for _, f := range []string{`php\php-cgi.exe`, `php\php.ini`, `apache\bin\httpd.exe`, `apache\conf\httpd.conf`, `mysql\bin\mysqld.exe`, `mysql\bin\my.ini`, `htdocs\dashboard\index.php`} {
		touch(t, filepath.Join(xampp, f))
	}
	for _, f := range []string{`php84\php-cgi.exe`, `php83\php-cgi.exe`, `nvm\v20.11.1\node.exe`} {
		touch(t, filepath.Join(herd, f))
	}
	touch(t, filepath.Join(fly, `php-8.3.10\php-cgi.exe`))
	touch(t, filepath.Join(fly, `redis-7.2.4\redis-server.exe`))
	orig := externalSources
	t.Cleanup(func() { externalSources = orig })
	externalSources = func() []extSource {
		return []extSource{
			{"XAMPP", "xampp", filepath.Join(xampp, "htdocs"), []string{xampp}, 3},
			{"Laravel Herd", "herd", "", []string{herd}, 3},
			{"FlyEnv", "flyenv", "", []string{fly}, 4},
		}
	}
	return
}

func TestExternalScanAndLink(t *testing.T) {
	xampp, _, _ := fakeEnvs(t)
	a, root := standaloneApp(t)
	envs := a.ExternalEnvironments()
	got := map[string][]string{}
	for _, e := range envs {
		for _, it := range e.Items {
			got[e.Name] = append(got[e.Name], it.Service+":"+it.LinkName)
		}
	}
	want := map[string][]string{
		"XAMPP":        {"apache:xampp-apache", "mysql:xampp-mysql", "php:xampp-php"},
		"Laravel Herd": {"nodejs:herd-v20.11.1", "php:herd-php83", "php:herd-php84"},
		"FlyEnv":       {"php:flyenv-php-8.3.10", "redis:flyenv-redis-7.2.4"},
	}
	for name, w := range want {
		g := got[name]
		slices.Sort(g)
		if !slices.Equal(g, w) {
			t.Errorf("%s: %v, want %v", name, g, w)
		}
	}
	if envs[0].WWW != filepath.Join(xampp, "htdocs") {
		t.Errorf("XAMPP www = %q", envs[0].WWW)
	}

	// Link XAMPP's PHP: a junction, the version shows up, nothing is copied.
	if _, err := a.LinkExternal("php", filepath.Join(xampp, "php")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, `bin\php\xampp-php`)
	if !isLink(link) || !slices.Contains(a.PHPVersions(), "xampp-php") {
		t.Fatalf("linked php not visible: %v", a.PHPVersions())
	}
	if !strings.EqualFold(realPath(link), filepath.Join(xampp, "php")) {
		t.Fatalf("realPath = %s", realPath(link))
	}
	if _, err := a.LinkExternal("php", `C:\Windows`); err == nil {
		t.Fatal("linked a folder the scan did not find")
	}
	// Unlink removes only the junction; XAMPP's files stay.
	if err := a.UnlinkExternal("php", "xampp-php"); err != nil {
		t.Fatal(err)
	}
	if exists(link) || !exists(filepath.Join(xampp, `php\php-cgi.exe`)) {
		t.Fatal("unlink must remove the link only")
	}
	if a.UnlinkExternal("php", "..") == nil {
		t.Fatal("bad link name accepted")
	}
	// UseExternalWWW: XAMPP's htdocs becomes the projects folder.
	if err := a.UseExternalWWW("XAMPP"); err != nil || a.www() != filepath.Join(xampp, "htdocs") {
		t.Fatalf("UseExternalWWW: %v %s", err, a.www())
	}
}

func TestAdoptedServicesKeepTheirFiles(t *testing.T) {
	xampp, _, _ := fakeEnvs(t)
	a, root := standaloneApp(t)
	// MySQL from XAMPP keeps my.ini in bin\ and its port can't be rewritten from DevPanel.
	a.LinkExternal("mysql", filepath.Join(xampp, "mysql"))
	a.cfg.Versions["mysql"] = "xampp-mysql"
	if !a.isAdopted("mysql") || findMyIni(a.svcDir("mysql")) != filepath.Join(a.svcDir("mysql"), `bin\my.ini`) {
		t.Fatalf("xampp my.ini not found: %s", findMyIni(a.svcDir("mysql")))
	}
	if err := a.SetPort("mysql", 3399); err == nil || !strings.Contains(err.Error(), "linked") {
		t.Fatalf("port change on a linked service: %v", err)
	}
	// Apache from XAMPP: DevPanel writes its own -f config; XAMPP's httpd.conf is untouched.
	a.LinkExternal("apache", filepath.Join(xampp, "apache"))
	a.cfg.Versions["apache"] = "xampp-apache"
	before, _ := os.ReadFile(filepath.Join(xampp, `apache\conf\httpd.conf`))
	if err := a.writeAdoptedApacheConf(a.svcDir("apache")); err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(a.adoptedApacheConf())
	c := string(conf)
	if !strings.Contains(c, `Define SRVROOT "`+filepath.ToSlash(filepath.Join(xampp, "apache"))+`"`) || !strings.Contains(c, filepath.ToSlash(filepath.Join(root, `data\apache\error.log`))) {
		t.Fatalf("adopted conf:\n%s", c)
	}
	if after, _ := os.ReadFile(filepath.Join(xampp, `apache\conf\httpd.conf`)); string(after) != string(before) {
		t.Fatal("XAMPP's httpd.conf was changed")
	}
	if args := a.apacheArgs(); len(args) != 2 || args[0] != "-f" {
		t.Fatalf("apacheArgs = %v", args)
	}
	// Uninstalling a linked version unlinks it; the files stay.
	os.MkdirAll(filepath.Join(root, `bin\apache\httpd-own`), 0755)
	a.cfg.Versions["apache"] = "httpd-own"
	if err := a.UninstallVersion("apache", "xampp-apache", "xampp-apache"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(xampp, `apache\bin\httpd.exe`)) {
		t.Fatal("uninstalling a linked version deleted the other environment's files")
	}
}

// Processes started from the linked folder count as DevPanel's (so Stop works), found through the junction.
func TestLinkedProcessOwnership(t *testing.T) {
	target := filepath.Join(t.TempDir(), `xampp\redis`)
	exe := filepath.Join(target, "redis-server.exe")
	copyFile(t, filepath.Join(os.Getenv("SystemRoot"), `System32\PING.EXE`), exe)
	link := filepath.Join(t.TempDir(), `bin\redis\xampp-redis`)
	if err := makeJunction(link, target); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !slices.Contains(processesUnder(link), uint32(cmd.Process.Pid)) || !anyUnder(processPaths(), link) {
		t.Fatal("process from the junction target not recognized")
	}
	if n := killUnder(link); n != 1 {
		t.Fatalf("killed %d", n)
	}
	cmd.Wait()
}

// "Use what's already there" includes other environments: check_requirement points to link_external
// and install is refused while a fitting version exists elsewhere.
func TestRequirementFindsExternal(t *testing.T) {
	defer func(orig func(*App, string, string) []Package) { onlineLookup = orig }(onlineLookup)
	onlineLookup = func(*App, string, string) []Package { return nil }
	_, herd, _ := fakeEnvs(t)
	// make Herd's php84 report a real version through its folder name
	os.Rename(filepath.Join(herd, "php84"), filepath.Join(herd, "php-8.4.12"))
	a, _ := standaloneApp(t)
	r, err := a.CheckRequirement("php", "^8.4")
	if err != nil || len(r.External) == 0 || r.External[0].Env != "Laravel Herd" || !strings.Contains(r.Advice, "link_external") || r.Suggestion != "" {
		t.Fatalf("external fit: %+v %v", r, err)
	}
	if _, err := a.InstallChecked("php", "", "^8.4", "x"); err == nil || !strings.Contains(err.Error(), "Laravel Herd") {
		t.Fatalf("install must be refused while Herd has a fitting PHP: %v", err)
	}
}

// Linking Laragon's PostgreSQL must keep using Laragon's cluster (<root>\data\postgresql), not create a new one.
func TestLinkedPostgresKeepsItsData(t *testing.T) {
	lar := filepath.Join(t.TempDir(), "laragon")
	bin := filepath.Join(lar, `bin\postgresql\postgresql`)
	touch(t, filepath.Join(bin, `bin\pg_ctl.exe`))
	touch(t, filepath.Join(lar, `data\postgresql\PG_VERSION`))
	a, root := standaloneApp(t)
	link := filepath.Join(root, `bin\postgresql\laragon-postgresql`)
	if err := makeJunction(link, bin); err != nil {
		t.Fatal(err)
	}
	a.cfg.Versions["postgresql"] = "laragon-postgresql"
	if got := a.pgData(); !strings.EqualFold(got, filepath.Join(lar, `data\postgresql`)) {
		t.Fatalf("pgData = %s, want Laragon's data folder", got)
	}
	// Bundled next to the binaries (…\data) wins.
	touch(t, filepath.Join(bin, `data\PG_VERSION`))
	if got := a.pgData(); !strings.EqualFold(got, filepath.Join(bin, "data")) {
		t.Fatalf("pgData = %s", got)
	}
}
