package main

// Version requirements for AI-driven project setup: check what is installed first, and only install
// (after the user confirms in a DevPanel dialog) when nothing installed fits.

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type semver [3]int

var (
	semverRe  = regexp.MustCompile(`(\d+)(?:\.(\d+))?(?:\.(\d+))?`)
	dottedVer = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)
)

// parseVer reads the first version in s ("php-8.4.5-nts" -> 8.4.5, "v22" -> 22) and how many parts it had.
func parseVer(s string) (v semver, parts int, ok bool) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return v, 0, false
	}
	for i := 0; i < 3; i++ {
		if m[i+1] != "" {
			v[i], _ = strconv.Atoi(m[i+1])
			parts++
		}
	}
	return v, parts, true
}

func cmpVer(a, b semver) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// satisfies checks a version against a composer/npm style constraint:
// "8.4", "^8.2", "~8.3", ">=18 <23", ">=8.1,<8.5", "8.*", "16.x", "^7.4 || ^8.0", "*" or "".
func satisfies(v semver, constraint string) bool {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" || constraint == "*" {
		return true
	}
	for _, alt := range strings.Split(constraint, "||") {
		ok := true
		for _, tok := range strings.FieldsFunc(alt, func(r rune) bool { return r == ' ' || r == ',' }) {
			if !satisfiesOne(v, tok) {
				ok = false
				break
			}
		}
		if ok && strings.TrimSpace(alt) != "" {
			return true
		}
	}
	return false
}

func satisfiesOne(v semver, tok string) bool {
	tok = strings.TrimPrefix(strings.ToLower(tok), "v")
	op := ""
	for _, p := range []string{">=", "<=", "^", "~", ">", "<", "="} {
		if strings.HasPrefix(tok, p) {
			op, tok = p, strings.TrimPrefix(tok, p)
			break
		}
	}
	tok = strings.TrimPrefix(tok, "v")
	w, parts, ok := parseVer(strings.NewReplacer(".x", "", ".*", "").Replace(tok))
	if !ok {
		return tok == "*" || tok == "x"
	}
	upper := func(level int) semver { // first version past the given precision
		u := w
		u[level]++
		for i := level + 1; i < 3; i++ {
			u[i] = 0
		}
		return u
	}
	switch op {
	case ">=":
		return cmpVer(v, w) >= 0
	case ">":
		return cmpVer(v, w) > 0
	case "<=":
		return cmpVer(v, w) <= 0
	case "<":
		return cmpVer(v, w) < 0
	case "^": // same major (or same minor when major is 0)
		lvl := 0
		if w[0] == 0 {
			lvl = 1
		}
		return cmpVer(v, w) >= 0 && cmpVer(v, upper(lvl)) < 0
	case "~": // ~8.3 -> >=8.3 <9 ; ~8.3.1 -> >=8.3.1 <8.4
		lvl := 0
		if parts >= 3 {
			lvl = 1
		}
		return cmpVer(v, w) >= 0 && cmpVer(v, upper(lvl)) < 0
	}
	// "8.4" / "8.*" / "=8.4.5": match on the given precision
	return cmpVer(v, w) >= 0 && cmpVer(v, upper(parts-1)) < 0
}

// versionCmds read the exact version when a folder name is not precise enough (e.g. "postgresql", "node-v22").
var versionCmds = map[string][]string{
	"php": {"php.exe", "-v"}, "postgresql": {`bin\postgres.exe`, "--version"}, "mysql": {`bin\mysqld.exe`, "--version"},
	"apache": {`bin\httpd.exe`, "-v"}, "nginx": {"nginx.exe", "-v"}, "redis": {"redis-server.exe", "--version"},
	"nodejs": {"node.exe", "-v"}, "python": {"python.exe", "--version"}, "go": {`bin\go.exe`, "version"},
}

// installedVersion is the version of one installed folder of a service/runtime.
func (a *App) installedVersion(name, folder string) (semver, bool) {
	v, parts, ok := parseVer(folder)
	if ok && parts == 3 {
		return v, true
	}
	if c, has := versionCmds[name]; has {
		dir := a.path(`bin\` + name + `\` + folder)
		// The first X.Y(.Z) in the output is the version ("postgres (PostgreSQL) 17.2", "go version go1.26.5").
		if out, _ := output(filepath.Join(dir, c[0]), c[1:]...); out != "" {
			if w, p, ok2 := parseVer(dottedVer.FindString(out)); ok2 && p >= 2 {
				return w, true
			}
		}
	}
	return v, ok
}

func verString(v semver) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

type InstalledVer struct {
	Folder  string `json:"folder"`
	Version string `json:"version"`
	Fits    bool   `json:"fits"`
	Active  bool   `json:"active"`
}

type Requirement struct {
	Name           string         `json:"name"`
	Constraint     string         `json:"constraint"`
	Installed      []InstalledVer `json:"installed"`
	UseInstalled   string         `json:"useInstalled,omitempty"`   // folder of the best installed version that fits
	ActiveFits     bool           `json:"activeFits"`               // the currently selected version already fits
	Suggestion     string         `json:"suggestion,omitempty"`     // catalog package to propose to the user
	SuggestVersion string         `json:"suggestVersion,omitempty"` // its version
	Candidates     []string       `json:"candidates"`               // all catalog packages that fit
	Advice         string         `json:"advice"`                   // what to do next, for the AI
	External       []ExternalFit  `json:"external,omitempty"`       // fitting installs in other environments
}

type ExternalFit struct {
	Env      string `json:"env"`
	Dir      string `json:"dir"`
	Version  string `json:"version"`
	LinkName string `json:"linkName"`
}

// externalFits lists not-yet-linked installs in other environments (XAMPP, Herd, ...) that satisfy constraint.
func (a *App) externalFits(name, constraint string) []ExternalFit {
	var out []ExternalFit
	for _, e := range a.ExternalEnvironments() {
		for _, it := range e.Items {
			if it.Service != name || it.Linked || it.Version == "" {
				continue
			}
			if v, _, ok := parseVer(it.Version); ok && satisfies(v, constraint) {
				out = append(out, ExternalFit{e.Name, it.Dir, it.Version, it.LinkName})
			}
		}
	}
	return out
}

func (a *App) activeFolder(name string) string {
	if name == "php" {
		return a.CurrentPHP()
	}
	return folderName(a.svcDir(name))
}

// CheckRequirement compares installed versions of a service/runtime with a constraint and suggests a catalog version if none fits.
func (a *App) CheckRequirement(name, constraint string) (Requirement, error) {
	if svc(name).Name == "" && !isRuntime(name) && name != "php" {
		return Requirement{}, fmt.Errorf("unknown service or runtime %q", name)
	}
	r := Requirement{Name: name, Constraint: constraint, Installed: []InstalledVer{}, Candidates: []string{}}
	folders := a.versions(name)
	if name == "php" {
		folders = a.PHPVersions()
	}
	active := a.activeFolder(name)
	var best semver
	for _, f := range folders {
		v, ok := a.installedVersion(name, f)
		iv := InstalledVer{Folder: f, Active: f == active}
		if ok {
			iv.Version = verString(v)
			iv.Fits = satisfies(v, constraint)
		}
		if iv.Fits && (r.UseInstalled == "" || cmpVer(v, best) > 0) {
			r.UseInstalled, best = f, v
		}
		if iv.Fits && iv.Active {
			r.ActiveFits = true
		}
		r.Installed = append(r.Installed, iv)
	}
	var sugV semver
	pkgs := a.PackagesFor(name)
	if r.UseInstalled == "" && !hasFit(pkgs, constraint) { // catalog has nothing: try the official feeds (PHP, Node)
		onlineLookup(a, name, constraint)
		pkgs = a.PackagesFor(name)
	}
	for _, p := range pkgs {
		v, _, ok := parseVer(p.Folder)
		if !ok {
			v, _, ok = parseVer(p.Name)
		}
		if !ok || p.Installed || !satisfies(v, constraint) {
			continue
		}
		r.Candidates = append(r.Candidates, p.Name)
		if r.Suggestion == "" || cmpVer(v, sugV) > 0 {
			r.Suggestion, sugV = p.Name, v
			r.SuggestVersion = verString(v)
		}
	}
	if r.UseInstalled == "" { // nothing in DevPanel fits: maybe XAMPP/Laragon/Herd/FlyEnv already has it
		r.External = a.externalFits(name, constraint)
	}
	if r.UseInstalled != "" || len(r.External) > 0 { // something already on this PC fits: no install suggestion
		r.Suggestion, r.SuggestVersion = "", ""
	}
	switch {
	case r.ActiveFits:
		r.Advice = "The selected version already fits. Nothing to install."
	case r.UseInstalled != "" && name == "php":
		r.Advice = "Use the installed " + r.UseInstalled + ": call set_site_php for the project (no install needed)."
	case r.UseInstalled != "":
		r.Advice = "Use the installed " + r.UseInstalled + ": call set_version (stop the service first). No install needed."
	case len(r.External) > 0:
		e := r.External[0]
		r.Advice = fmt.Sprintf("%s already has %s %s at %s: call link_external (service=%s, dir=%s), then select %s with %s. No install needed.",
			e.Env, serviceLabel(name), e.Version, e.Dir, name, e.Dir, e.LinkName, map[bool]string{true: "set_site_php", false: "set_version"}[name == "php"])
	case r.Suggestion != "":
		r.Advice = "Nothing installed fits " + constraint + ". Ask the user before installing; suggest " + r.Suggestion +
			" (" + r.SuggestVersion + ") and explain why. Only after they agree, call install with package=" + r.Suggestion + "."
	default:
		r.Advice = "Nothing installed fits and the catalog has no matching version. Tell the user; they can add a download link in DevPanel > Install."
	}
	return r, nil
}

// confirmInstall asks the user on the desktop (not through the AI). Replaced in tests.
var confirmInstall = func(title, text string) bool {
	const yesNo, question, topmost, foreground, idYes = 0x4, 0x20, 0x40000, 0x10000, 6
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	// MessageBoxTimeoutW: unanswered after 5 minutes counts as "No".
	r, _, _ := windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxTimeoutW").Call(0,
		uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), yesNo|question|topmost|foreground, 0, 5*60*1000)
	return r == idYes
}

// InstallChecked installs a catalog package for an AI request: refused when an installed version already
// fits the constraint, or the package doesn't fit it, and always confirmed by the user in a dialog.
func (a *App) InstallChecked(name, pkg, constraint, reason string) (string, error) {
	req, err := a.CheckRequirement(name, constraint)
	if err != nil {
		return "", err
	}
	if req.UseInstalled != "" {
		return "", fmt.Errorf("not installed: %s is already installed and fits %q. %s", req.UseInstalled, constraint, req.Advice)
	}
	if len(req.External) > 0 {
		return "", fmt.Errorf("not installed: a fitting version exists in %s. %s", req.External[0].Env, req.Advice)
	}
	if pkg == "" {
		pkg = req.Suggestion
	}
	var p *Package
	for _, c := range a.PackagesFor(name) {
		if c.Name == pkg {
			p = &c
		}
	}
	if p == nil {
		return "", fmt.Errorf("no package %q for %s; use one of: %s", pkg, name, strings.Join(req.Candidates, ", "))
	}
	if v, _, ok := parseVer(p.Folder); ok && !satisfies(v, constraint) {
		return "", fmt.Errorf("%s (%s) does not fit %q", p.Name, verString(v), constraint)
	}
	var have []string
	for _, iv := range req.Installed {
		have = append(have, iv.Folder)
	}
	if len(have) == 0 {
		have = []string{"none"}
	}
	msg := fmt.Sprintf("An AI assistant wants to install %s.\n\nPackage:  %s\nNeeded:  %s\nReason:  %s\nInstalled now:  %s\n\nNo installed version fits, so this version is suggested. Install it?",
		serviceLabel(name), p.Name, nonEmpty(constraint, "(any)"), nonEmpty(reason, "(not given)"), strings.Join(have, ", "))
	if !confirmInstall("DevPanel – confirm install", msg) {
		return "", errors.New("the user declined (or did not answer) the install; ask them how to proceed")
	}
	out, err := a.Install(p.Name)
	if err != nil {
		return "", err
	}
	if svc(name).Name != "" {
		a.SetServiceEnabled(name, true)
	}
	next := "set_version"
	if name == "php" {
		next = "set_site_php (per project) or set_version"
	}
	return out + ". Switch to it with " + next + ".", nil
}

// onlineLookup is swapped out in unit tests so they never depend on the internet.
var onlineLookup = (*App).onlineCandidates

func hasFit(pkgs []Package, constraint string) bool {
	for _, p := range pkgs {
		if v, _, ok := parseVer(p.Folder); ok && !p.Installed && satisfies(v, constraint) {
			return true
		}
	}
	return false
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
