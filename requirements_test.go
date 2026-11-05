package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSatisfies(t *testing.T) {
	v := func(s string) semver { x, _, _ := parseVer(s); return x }
	cases := []struct {
		ver, c string
		want   bool
	}{
		{"8.4.5", "^8.2", true}, {"7.4.33", "^8.2", false}, {"9.0.0", "^8.2", false},
		{"8.3.16", "~8.3", true}, {"8.9.0", "~8.3", true}, {"9.0.0", "~8.3", false},
		{"8.3.5", "~8.3.1", true}, {"8.4.0", "~8.3.1", false},
		{"22.12.0", ">=20", true}, {"18.20.0", ">=20", false},
		{"22.12.0", ">=18 <23", true}, {"23.1.0", ">=18 <23", false}, {"8.2.0", ">=8.1,<8.5", true},
		{"8.4.5", "8.4", true}, {"8.4.5", "8.4.*", true}, {"8.5.0", "8.4.*", false}, {"16.4.2", "16.x", true},
		{"7.4.33", "^7.4 || ^8.0", true}, {"8.3.0", "^7.4 || ^8.0", true}, {"7.3.0", "^7.4 || ^8.0", false},
		{"0.3.5", "^0.3", true}, {"0.4.0", "^0.3", false},
		{"17.2.0", "", true}, {"1.0.0", "*", true}, {"22.1.0", "v22", true},
	}
	for _, c := range cases {
		if got := satisfies(v(c.ver), c.c); got != c.want {
			t.Errorf("satisfies(%s, %q) = %v, want %v", c.ver, c.c, got, c.want)
		}
	}
}

func reqApp(t *testing.T) (*App, string) {
	// Unit tests must not see this PC's real Laragon/XAMPP/Herd installs.
	orig := externalSources
	t.Cleanup(func() { externalSources = orig })
	externalSources = func() []extSource { return nil }
	root := t.TempDir()
	for _, d := range []string{`bin\php\php-8.4.5-nts-Win32-vs17-x64`, `bin\php\php-8.5.12-nts-Win32-vs17-x64`, `bin\nodejs\node-v22.12.0-win-x64`, `etc\apache2`, "usr"} {
		os.MkdirAll(filepath.Join(root, d), 0755)
	}
	for _, d := range []string{"php-8.4.5-nts-Win32-vs17-x64", "php-8.5.12-nts-Win32-vs17-x64"} {
		os.WriteFile(filepath.Join(root, `bin\php`, d, "php-cgi.exe"), nil, 0644)
	}
	os.WriteFile(filepath.Join(root, `etc\apache2\fcgid.conf`), []byte(`FcgidWrapper "C:/x/bin/php/php-8.5.12-nts-Win32-vs17-x64/php-cgi.exe" .php`), 0644)
	os.WriteFile(filepath.Join(root, `usr\packages.conf`), []byte("PHP-8.3=https://x/php-8.3.18-nts-Win32-vs16-x64.zip\nPHP-8.2=https://x/php-8.2.27-nts-Win32-vs16-x64.zip\n"), 0644)
	return &App{cfg: Config{Root: root, Versions: map[string]string{"php": "php-8.5.12-nts-Win32-vs17-x64"}}, cfgPath: filepath.Join(root, "devpanel.json")}, root
}

func TestCheckRequirement(t *testing.T) {
	defer func(orig func(*App, string, string) []Package) { onlineLookup = orig }(onlineLookup)
	online := 0
	onlineLookup = func(*App, string, string) []Package { online++; return nil }
	a, _ := reqApp(t)
	// Active PHP 8.5 fits ^8.2: nothing to do.
	r, err := a.CheckRequirement("php", "^8.2")
	if err != nil || !r.ActiveFits || r.Suggestion != "" { // installed fits: no install suggestion
		t.Fatalf("^8.2: %+v %v", r, err)
	}
	// Only the installed 8.4 fits: use it, don't install.
	r, _ = a.CheckRequirement("php", "~8.4.0")
	if r.ActiveFits || r.UseInstalled != "php-8.4.5-nts-Win32-vs17-x64" || !strings.Contains(r.Advice, "set_site_php") {
		t.Fatalf("~8.4.0: %+v", r)
	}
	// Nothing installed fits 8.3: suggest the catalog version and ask the user.
	r, _ = a.CheckRequirement("php", "8.3.*")
	if r.UseInstalled != "" || r.Suggestion != "PHP-8.3" || r.SuggestVersion != "8.3.18" || !strings.Contains(r.Advice, "Ask the user") {
		t.Fatalf("8.3.*: %+v", r)
	}
	// Nothing anywhere: tell the user.
	r, _ = a.CheckRequirement("php", "^7.4")
	if r.Suggestion != "" || !strings.Contains(r.Advice, "no matching version") || online != 1 {
		t.Fatalf("^7.4: %+v (online lookups %d: only when the catalog has nothing)", r, online)
	}
	if _, err := a.CheckRequirement("cobol", "*"); err == nil {
		t.Fatal("unknown name accepted")
	}
}

func TestInstallCheckedPolicy(t *testing.T) {
	defer func(orig func(*App, string, string) []Package) { onlineLookup = orig }(onlineLookup)
	onlineLookup = func(*App, string, string) []Package { return nil }
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("php-cgi.exe")
	f.Write([]byte("MZ"))
	w.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.Write(buf.Bytes()) }))
	defer srv.Close()

	a, root := reqApp(t)
	os.WriteFile(filepath.Join(root, `usr\packages.conf`), []byte("PHP-8.3="+srv.URL+"/php-8.3.18-nts-Win32-vs16-x64.zip\n"), 0644)
	asked := 0
	answer := false
	defer func(orig func(string, string) bool) { confirmInstall = orig }(confirmInstall)
	confirmInstall = func(title, text string) bool {
		asked++
		if !strings.Contains(text, "8.3.*") || !strings.Contains(text, "PHP-8.3") || !strings.Contains(text, "legacy app") {
			t.Errorf("dialog text lacks details:\n%s", text)
		}
		return answer
	}
	// Refused without asking: an installed version fits.
	if _, err := a.InstallChecked("php", "PHP-8.3", "^8.2", "x"); err == nil || !strings.Contains(err.Error(), "already installed") || asked != 0 {
		t.Fatalf("install when installed fits: %v (asked %d)", err, asked)
	}
	// Package that doesn't fit the constraint.
	if _, err := a.InstallChecked("php", "PHP-8.3", "8.2.*", "x"); err == nil || asked != 0 {
		t.Fatalf("non-fitting package: %v", err)
	}
	// User says No.
	if _, err := a.InstallChecked("php", "", "8.3.*", "legacy app"); err == nil || !strings.Contains(err.Error(), "declined") || asked != 1 {
		t.Fatalf("declined: %v", err)
	}
	if exists(filepath.Join(root, `bin\php\php-8.3.18-nts-Win32-vs16-x64`)) {
		t.Fatal("installed although the user declined")
	}
	// User says Yes.
	answer = true
	if _, err := a.InstallChecked("php", "", "8.3.*", "legacy app"); err != nil && !strings.Contains(err.Error(), "setup failed") {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, `bin\php\php-8.3.18-nts-Win32-vs16-x64\php-cgi.exe`)) || asked != 2 {
		t.Fatal("not installed after Yes")
	}
}

func TestMCPSetupTools(t *testing.T) {
	defer func(orig func(*App, string, string) []Package) { onlineLookup = orig }(onlineLookup)
	onlineLookup = func(*App, string, string) []Package { return nil }
	a, root := reqApp(t)
	os.MkdirAll(filepath.Join(root, `www\shop`), 0755)
	res := mcpSession(t, a,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"check_requirement","arguments":{"name":"php","constraint":"^8.4"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"save_process","arguments":{"name":"queue","folder":"shop","command":"php artisan queue:work","autostart":true}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"enable_service","arguments":{"name":"redis","enabled":false}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"install","arguments":{"name":"php","constraint":"^8.4","reason":"x"}}}`,
	)
	text := func(i int) (string, bool) {
		r := res[i]["result"].(map[string]interface{})
		return r["content"].([]interface{})[0].(map[string]interface{})["text"].(string), r["isError"].(bool)
	}
	var req Requirement
	s, isErr := text(0)
	if json.Unmarshal([]byte(s), &req); isErr || !req.ActiveFits {
		t.Fatalf("check_requirement: %s", s)
	}
	if _, isErr := text(1); isErr || len(a.cfg.Procs) != 1 || a.cfg.Procs[0].Cmd != "php artisan queue:work" || !a.cfg.Procs[0].Autostart {
		t.Fatalf("save_process: %+v", a.cfg.Procs)
	}
	if _, isErr := text(2); isErr || a.enabled("redis") {
		t.Fatal("enable_service with a boolean argument")
	}
	if s, isErr := text(3); !isErr || !strings.Contains(s, "already installed") {
		t.Fatalf("install must be refused when an installed version fits: %s", s)
	}
}
