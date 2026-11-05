package main

// devpanel.exe mcp — Model Context Protocol server over stdio, so AI coding tools (Claude Code, Copilot CLI, ...)
// can check and drive the local stack. Calls go to the running agent, same as the window.
// Destructive actions (drop/restore databases, delete projects, uninstall, passwords) are deliberately not exposed.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = "DevPanel manages the local web stack on this Windows PC (Laragon folders): services (Apache, Nginx, " +
	"PostgreSQL, MySQL, Redis, ...), sites under www with *.test hostnames, PHP/Node/... versions and project processes. " +
	"Hostnames resolve only on this machine.\n\n" +
	"Setting up a project: read its requirements (composer.json require.php, package.json engines, .env DB_CONNECTION, " +
	"queue/Vite needs). For EVERY runtime or service it needs, call check_requirement first. " +
	"If an installed version fits, use it (set_site_php for PHP per project, set_version for services, enable_service). " +
	"Only if nothing installed fits: tell the user which version you suggest and why, ask them, and only after they agree call install. " +
	"DevPanel additionally shows the user a confirmation dialog for every install. Never install just to get a newer version. " +
	"Then: create_database, edit .env yourself, save_process for queue workers / npm run dev, set_site_proxy for dev servers, sync_vhosts."

type mcpTool struct {
	Name, Description string
	Props             map[string]string // property -> description (all strings unless listed in Ints)
	Required          []string
	Ints              []string
	Bools             []string
	Method            string   // agent method; "" = handled in callTool
	Args              []string // properties passed to Method, in order
}

var mcpTools = []mcpTool{
	{Name: "status", Description: "List services with running state, port, version and whether they are enabled.", Method: "Status"},
	{Name: "start_service", Description: "Start one service, e.g. apache, nginx, postgresql, mysql, redis.", Props: map[string]string{"name": "service name"}, Required: []string{"name"}, Method: "Start", Args: []string{"name"}},
	{Name: "stop_service", Description: "Stop one service.", Props: map[string]string{"name": "service name"}, Required: []string{"name"}, Method: "Stop", Args: []string{"name"}},
	{Name: "start_all", Description: "Start every enabled service (only one web server).", Method: "StartAll"},
	{Name: "list_groups", Description: "List service groups (name -> services).", Method: "Groups"},
	{Name: "start_group", Description: "Start a group's services and stop the others.", Props: map[string]string{"name": "group name"}, Required: []string{"name"}, Method: "StartGroup", Args: []string{"name"}},
	{Name: "list_sites", Description: "Sites in www: folder, hostname, document root, PHP version, framework, APP_URL, proxy.", Method: "Sites"},
	{Name: "php_versions", Description: "Installed PHP versions.", Method: "PHPVersions"},
	{Name: "set_site_php", Description: "Use a specific PHP version for one site (empty version = default).", Props: map[string]string{"folder": "site folder in www", "version": "PHP folder name from php_versions, or empty"}, Required: []string{"folder", "version"}, Method: "SetSitePHP", Args: []string{"folder", "version"}},
	{Name: "sync_vhosts", Description: "Regenerate web server config for all sites and reload the running web server. Does not edit the hosts file.", Method: "SyncVhostConfig"},
	{Name: "service_log", Description: "Last lines of a service's log (e.g. apache error log, postgresql, mysql).", Props: map[string]string{"name": "service name", "lines": "number of lines (default 100)"}, Required: []string{"name"}, Ints: []string{"lines"}},
	{Name: "list_databases", Description: "Databases of the local PostgreSQL or MySQL server.", Props: map[string]string{"engine": "postgresql or mysql"}, Required: []string{"engine"}, Method: "Databases", Args: []string{"engine"}},
	{Name: "create_database", Description: "Create an empty database (letters, digits, _ and -).", Props: map[string]string{"engine": "postgresql or mysql", "name": "database name"}, Required: []string{"engine", "name"}, Method: "CreateDatabase", Args: []string{"engine", "name"}},
	{Name: "list_processes", Description: "Project processes (queue workers, Vite, ...) and whether they run.", Method: "Processes"},
	{Name: "start_process", Description: "Start a project process by name.", Props: map[string]string{"name": "process name"}, Required: []string{"name"}, Method: "StartProcess", Args: []string{"name"}},
	{Name: "stop_process", Description: "Stop a project process by name.", Props: map[string]string{"name": "process name"}, Required: []string{"name"}, Method: "StopProcess", Args: []string{"name"}},
	{Name: "process_log", Description: "Recent output of a project process.", Props: map[string]string{"name": "process name"}, Required: []string{"name"}, Method: "ProcessLog", Args: []string{"name"}},

	// ---- Project setup ----
	{Name: "check_requirement", Description: "ALWAYS call before installing anything. Compares installed versions of a service/runtime (php, nodejs, postgresql, mysql, redis, ...) with a constraint like ^8.2, >=20, ~16.4, 8.* and tells whether an installed one fits, or which catalog version to suggest to the user.",
		Props:    map[string]string{"name": "service or runtime: php, nodejs, go, python, java, dotnet, ruby, bun, deno, postgresql, mysql, redis, ...", "constraint": "version constraint from the project, e.g. ^8.2 (composer), >=20 (package.json engines); empty = any"},
		Required: []string{"name", "constraint"}, Method: "CheckRequirement", Args: []string{"name", "constraint"}},
	{Name: "install", Description: "Install a service/runtime version from the catalog. Only when check_requirement found nothing installed that fits AND the user agreed to your suggested version. Refused if an installed version fits; the user also confirms in a DevPanel dialog.",
		Props:    map[string]string{"name": "service or runtime", "package": "catalog package name from check_requirement (empty = its suggestion)", "constraint": "the project's version constraint", "reason": "why the project needs it (shown to the user)"},
		Required: []string{"name", "constraint", "reason"}, Method: "InstallChecked", Args: []string{"name", "package", "constraint", "reason"}},
	{Name: "list_packages", Description: "Catalog versions available for a service/runtime (installed ones are marked).", Props: map[string]string{"name": "service or runtime"}, Required: []string{"name"}, Method: "PackagesFor", Args: []string{"name"}},
	{Name: "set_version", Description: "Switch the active installed version of a service or runtime (services must be stopped first).", Props: map[string]string{"name": "service or runtime", "version": "installed folder name (see check_requirement)"}, Required: []string{"name", "version"}, Method: "SetVersion", Args: []string{"name", "version"}},
	{Name: "enable_service", Description: "Tick (enabled=true) or untick a service in DevPanel so it shows and is started by start_all.", Props: map[string]string{"name": "service name", "enabled": "true to use it"}, Bools: []string{"enabled"}, Required: []string{"name", "enabled"}, Method: "SetServiceEnabled", Args: []string{"name", "enabled"}},
	{Name: "create_app", Description: "Create a new project in www from a template, with its database and vhost. Templates: laravel, wordpress, symfony, codeigniter, cakephp, drupal, blank.",
		Props: map[string]string{"kind": "template", "folder": "folder name in www (letters, digits, - _)", "engine": "pgsql, mysql or sqlite"}, Required: []string{"kind", "folder", "engine"}, Method: "CreateApp", Args: []string{"kind", "folder", "engine"}},
	{Name: "save_process", Description: "Add or update a project process (e.g. php artisan queue:work, npm run dev) run with the selected versions.",
		Props: map[string]string{"name": "process name", "folder": "project folder in www", "command": "command line", "autostart": "start when DevPanel starts"}, Bools: []string{"autostart"},
		Required: []string{"name", "folder", "command"}, Method: "SaveProcess", Args: []string{"p"}},
	{Name: "php_extensions", Description: "PHP extensions of the selected PHP and whether they are enabled.", Method: "PHPExtensions"},
	{Name: "set_php_extension", Description: "Enable or disable a PHP extension (e.g. pdo_pgsql, intl, sodium) in php.ini; restarts the web server.", Props: map[string]string{"name": "extension name", "enabled": "true to enable"}, Bools: []string{"enabled"}, Required: []string{"name", "enabled"}, Method: "SetPHPExtension", Args: []string{"name", "enabled"}},
	{Name: "set_site_proxy", Description: "Forward a site to a local dev server (e.g. 127.0.0.1:5173 for Vite, :3000 for Node); empty target = serve files/PHP again.", Props: map[string]string{"folder": "site folder in www", "target": "http://127.0.0.1:PORT or empty"}, Required: []string{"folder", "target"}, Method: "SetSiteProxy", Args: []string{"folder", "target"}},
	{Name: "set_hostname", Description: "Set a site's hostname (e.g. shop or shop.test); empty = default name.", Props: map[string]string{"folder": "site folder in www", "hostname": "new hostname or empty"}, Required: []string{"folder", "hostname"}, Method: "SetHostname", Args: []string{"folder", "hostname"}},
	{Name: "set_xdebug", Description: "Enable or disable Xdebug for the selected PHP (downloads the matching DLL when enabling).", Props: map[string]string{"enabled": "true to enable"}, Bools: []string{"enabled"}, Required: []string{"enabled"}, Method: "SetXdebug", Args: []string{"enabled"}},
	{Name: "set_mail_catch", Description: "Send PHP mail() of all PHP versions to Mailpit (enabled=true) or back to default.", Props: map[string]string{"enabled": "true to catch mail"}, Bools: []string{"enabled"}, Required: []string{"enabled"}, Method: "SetMailCatch", Args: []string{"enabled"}},
}

func (t mcpTool) schema() map[string]interface{} {
	props := map[string]interface{}{}
	for k, d := range t.Props {
		typ := "string"
		if slices.Contains(t.Ints, k) {
			typ = "integer"
		} else if slices.Contains(t.Bools, k) {
			typ = "boolean"
		}
		props[k] = map[string]interface{}{"type": typ, "description": d}
	}
	s := map[string]interface{}{"type": "object", "properties": props}
	if len(t.Required) > 0 {
		s["required"] = t.Required
	}
	return s
}

// SyncVhostConfig is sync without the admin-only hosts file step (for MCP and other non-interactive callers).
func (a *App) SyncVhostConfig() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	hosts, changed, err := a.syncVhostFiles()
	if err != nil {
		return "", err
	}
	if changed > 0 {
		err = a.restartWeb()
	}
	return fmt.Sprintf("%d sites, %d config changes", len(hosts), changed), err
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type mcpServer struct {
	call func(method string, args []json.RawMessage) (json.RawMessage, error) // to the agent
}

func textResult(text string, isErr bool) map[string]interface{} {
	return map[string]interface{}{"resultType": "complete", "content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr}
}

func (m *mcpServer) callTool(name string, args map[string]interface{}) map[string]interface{} {
	i := slices.IndexFunc(mcpTools, func(t mcpTool) bool { return t.Name == name })
	if i < 0 {
		return textResult("unknown tool "+name, true)
	}
	t := mcpTools[i]
	for _, r := range t.Required {
		if _, ok := args[r]; !ok {
			return textResult("missing argument "+r, true)
		}
	}
	str := func(k string) string { s, _ := args[k].(string); return s }
	if t.Name == "service_log" {
		lines := 100
		if f, ok := args["lines"].(float64); ok && f > 0 {
			lines = int(f)
		}
		d, err := m.call("Detail", []json.RawMessage{raw(str("name"))})
		if err != nil {
			return textResult(err.Error(), true)
		}
		var det Detail
		json.Unmarshal(d, &det)
		for _, f := range det.Files {
			if f.Log && f.Exists {
				out, err := m.call("TailFile", []json.RawMessage{raw(f.Path), raw(lines)})
				if err != nil {
					return textResult(err.Error(), true)
				}
				var s string
				json.Unmarshal(out, &s)
				return textResult("── "+f.Label+" ("+f.Path+")\n"+s, false)
			}
		}
		return textResult("no log file found for "+str("name"), true)
	}
	if t.Name == "save_process" { // SaveProcess takes one struct
		auto, _ := args["autostart"].(bool)
		args = map[string]interface{}{"p": Proc{Name: str("name"), Dir: str("folder"), Cmd: str("command"), Autostart: auto}}
	}
	var callArgs []json.RawMessage
	for _, k := range t.Args {
		v, ok := args[k]
		switch {
		case ok && v != nil:
			callArgs = append(callArgs, raw(v))
		case slices.Contains(t.Bools, k):
			callArgs = append(callArgs, raw(false))
		default:
			callArgs = append(callArgs, raw(""))
		}
	}
	out, err := m.call(t.Method, callArgs)
	if err != nil {
		return textResult(err.Error(), true)
	}
	var v interface{}
	json.Unmarshal(out, &v)
	if v == nil {
		return textResult("ok", false)
	}
	if s, ok := v.(string); ok {
		return textResult(s, false)
	}
	pretty, _ := json.MarshalIndent(v, "", "  ")
	return textResult(string(pretty), false)
}

func serverInfo() map[string]string {
	return map[string]string{"name": "devpanel", "title": "DevPanel", "version": "1.0"}
}

// handle answers one request. Notifications (no id) are filtered out by the caller.
func (m *mcpServer) handle(method string, params json.RawMessage) (interface{}, *rpcError) {
	var p struct {
		ProtocolVersion string                 `json:"protocolVersion"`
		Name            string                 `json:"name"`
		Arguments       map[string]interface{} `json:"arguments"`
		Meta            map[string]interface{} `json:"_meta"`
	}
	if len(params) > 0 {
		json.Unmarshal(params, &p)
	}
	// 2026-07-28: every request may carry its version in _meta.
	if v, ok := p.Meta["io.modelcontextprotocol/protocolVersion"].(string); ok && !slices.Contains(mcpVersions, v) {
		return nil, &rpcError{-32022, "Unsupported protocol version", map[string]interface{}{"supported": mcpVersions, "requested": v}}
	}
	switch method {
	case "initialize": // pre-2026 lifecycle
		v := p.ProtocolVersion
		if !slices.Contains(mcpVersions, v) {
			v = mcpVersions[1]
		}
		return map[string]interface{}{"protocolVersion": v, "capabilities": map[string]interface{}{"tools": map[string]bool{"listChanged": false}},
			"serverInfo": serverInfo(), "instructions": mcpInstructions}, nil
	case "server/discover":
		return map[string]interface{}{"resultType": "complete", "supportedVersions": mcpVersions, "capabilities": map[string]interface{}{"tools": map[string]interface{}{}},
			"_meta": map[string]interface{}{"io.modelcontextprotocol/serverInfo": serverInfo()}, "instructions": mcpInstructions}, nil
	case "ping":
		return map[string]interface{}{}, nil
	case "tools/list":
		var tools []map[string]interface{}
		for _, t := range mcpTools {
			tools = append(tools, map[string]interface{}{"name": t.Name, "description": t.Description, "inputSchema": t.schema()})
		}
		return map[string]interface{}{"resultType": "complete", "tools": tools}, nil
	case "tools/call":
		if p.Arguments == nil {
			p.Arguments = map[string]interface{}{}
		}
		return m.callTool(p.Name, p.Arguments), nil
	}
	return nil, &rpcError{-32601, "Method not found: " + method, nil}
}

// serve reads newline-delimited JSON-RPC from r and writes responses to w (one line each, nothing else).
func (m *mcpServer) serve(r io.Reader, w io.Writer) {
	in := bufio.NewScanner(r)
	in.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(w) // Encode writes one line per message
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			enc.Encode(map[string]interface{}{"jsonrpc": "2.0", "id": nil, "error": rpcError{-32700, "Parse error", nil}})
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" { // notification, e.g. notifications/initialized
			continue
		}
		res, rerr := m.handle(req.Method, req.Params)
		msg := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
		if rerr != nil {
			msg["error"] = rerr
		} else {
			msg["result"] = res
		}
		enc.Encode(msg)
	}
}

func runMCP(app *App) int {
	p := &Proxy{cfgDir: filepath.Dir(app.cfgPath)}
	connected := false
	m := &mcpServer{call: func(method string, args []json.RawMessage) (json.RawMessage, error) {
		if !connected { // start the agent lazily, so initialize answers instantly
			if err := p.ensureAgent(); err != nil {
				return nil, err
			}
			connected = true
		}
		return p.call(method, args)
	}}
	m.serve(os.Stdin, os.Stdout)
	return 0
}
