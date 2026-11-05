package main

import (
	"encoding/json"
	"strings"
)

// onlineCandidates finds versions on the official feeds when the catalog has none that fit
// (e.g. an old project needs PHP ^7.4, a new one Node ^24). They become installable "Online" packages with SHA-256.
func (a *App) onlineCandidates(name, constraint string) []Package {
	var out []Package
	switch name {
	case "php":
		b, err := httpGet("https://windows.php.net/downloads/releases/releases.json", 4<<20)
		if err != nil {
			return nil
		}
		var rel map[string]map[string]json.RawMessage
		if json.Unmarshal(b, &rel) != nil {
			return nil
		}
		for _, r := range rel {
			var ver string
			json.Unmarshal(r["version"], &ver)
			v, _, ok := parseVer(ver)
			if !ok || !satisfies(v, constraint) {
				continue
			}
			for key, raw := range r { // nts-vs17-x64, nts-vs16-x64, nts-vc15-x64 (PHP 7.4)
				if !strings.HasPrefix(key, "nts-") || !strings.HasSuffix(key, "-x64") {
					continue
				}
				var build struct {
					Zip struct{ Path, Sha256 string } `json:"zip"`
				}
				if json.Unmarshal(raw, &build) == nil && build.Zip.Path != "" {
					p := a.pkg("PHP-"+verString(v), "https://windows.php.net/downloads/releases/"+build.Zip.Path, "php", "Online")
					p.SHA256 = build.Zip.Sha256
					out = append(out, p)
					break
				}
			}
		}
	case "nodejs":
		b, err := httpGet("https://nodejs.org/dist/index.json", 8<<20)
		if err != nil {
			return nil
		}
		var idx []struct {
			Version string   `json:"version"`
			Files   []string `json:"files"`
		}
		json.Unmarshal(b, &idx)
		for _, r := range idx { // newest first: take the newest stable fit only
			ver := strings.TrimPrefix(r.Version, "v")
			v, _, ok := parseVer(ver)
			if !ok || strings.Contains(ver, "-") || !satisfies(v, constraint) {
				continue
			}
			p := a.pkg("node-"+ver, "https://nodejs.org/dist/v"+ver+"/node-v"+ver+"-win-x64.zip", "nodejs", "Online")
			if sums, err := httpGet("https://nodejs.org/dist/v"+ver+"/SHASUMS256.txt", 1<<20); err == nil {
				p.SHA256 = shaFor(string(sums), "node-v"+ver+"-win-x64.zip")
			}
			out = append(out, p)
			break
		}
	}
	// Make them installable through the normal catalog path (Install / PackagesFor).
	a.srcMu.Lock()
	for _, p := range out {
		dup := false
		for _, u := range a.updates {
			dup = dup || u.Name == p.Name
		}
		if !dup {
			a.updates = append(a.updates, p)
		}
	}
	a.srcMu.Unlock()
	return out
}
