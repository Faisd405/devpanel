package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mcpSession runs the MCP server against a test agent and returns one parsed response per request line.
func mcpSession(t *testing.T, a *App, lines ...string) []map[string]interface{} {
	dir, _ := startTestAgent(t, a)
	p := &Proxy{cfgDir: dir}
	if err := p.ensureAgent(); err != nil {
		t.Fatal(err)
	}
	m := &mcpServer{call: p.call}
	var out strings.Builder
	m.serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out)
	var res []map[string]interface{}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var v map[string]interface{}
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("stdout must only carry JSON-RPC lines, got %q", sc.Text())
		}
		res = append(res, v)
	}
	return res
}

func TestMCPProtocol(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `www\shop`), 0755)
	a := &App{cfg: Config{Root: root, TLD: "test", Groups: map[string][]string{"Web": {"apache"}}, Versions: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	res := mcpSession(t, a,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_groups","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_sites"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"start_group","arguments":{"name":"Nope"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"drop_database","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01"}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"nope"}`,
		`not json`,
	)
	if len(res) != 10 { // the notification gets no answer
		t.Fatalf("got %d responses: %v", len(res), res)
	}
	byID := map[float64]map[string]interface{}{}
	for _, r := range res {
		if id, ok := r["id"].(float64); ok {
			byID[id] = r
		}
	}
	result := func(id float64) map[string]interface{} { r, _ := byID[id]["result"].(map[string]interface{}); return r }
	text := func(id float64) (string, bool) {
		r := result(id)
		c := r["content"].([]interface{})[0].(map[string]interface{})
		return c["text"].(string), r["isError"].(bool)
	}

	if result(1)["protocolVersion"] != "2025-06-18" || result(1)["serverInfo"].(map[string]interface{})["name"] != "devpanel" {
		t.Fatalf("initialize: %v", result(1))
	}
	var names []string
	for _, tl := range result(2)["tools"].([]interface{}) {
		names = append(names, tl.(map[string]interface{})["name"].(string))
	}
	for _, want := range []string{"status", "start_service", "list_sites", "service_log", "create_database"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s missing", want)
		}
	}
	for _, forbidden := range []string{"drop_database", "restore_database", "delete_project"} {
		if slices.Contains(names, forbidden) {
			t.Errorf("destructive tool %s must not be exposed", forbidden)
		}
	}
	if s, isErr := text(3); isErr || !strings.Contains(s, `"Web"`) {
		t.Fatalf("list_groups: %s", s)
	}
	if s, isErr := text(4); isErr || !strings.Contains(s, `"shop.test"`) {
		t.Fatalf("list_sites: %s", s)
	}
	if s, isErr := text(5); !isErr || !strings.Contains(s, "no group") {
		t.Fatalf("errors must come back as isError results: %s %v", s, isErr)
	}
	if _, isErr := text(6); !isErr {
		t.Fatal("unknown tool must be an isError result")
	}
	if v := result(7)["supportedVersions"].([]interface{}); v[0] != "2026-07-28" {
		t.Fatalf("discover: %v", result(7))
	}
	if e := byID[8]["error"].(map[string]interface{}); e["code"].(float64) != -32022 {
		t.Fatalf("unsupported version: %v", byID[8])
	}
	if e := byID[9]["error"].(map[string]interface{}); e["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", byID[9])
	}
}
