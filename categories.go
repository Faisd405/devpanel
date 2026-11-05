package main

// Categories like FlyEnv's sidebar. One list here drives every grouped view in the UI.
var categories = []struct {
	Name    string
	Members []string
}{
	{"Web & App Servers", []string{"apache", "nginx", "caddy", "frankenphp"}},
	{"Databases", []string{"postgresql", "mysql", "mongodb"}},
	{"Cache & Messaging", []string{"redis", "memcached", "rabbitmq"}},
	{"Search", []string{"meilisearch", "elasticsearch"}},
	{"AI", []string{"ollama"}},
	{"Mail & Tools", []string{"mailpit"}},
	{"Languages & Runtimes", []string{"php", "nodejs", "go", "python", "java", "dotnet", "ruby", "bun", "deno", "erlang"}},
}

func categoryOf(name string) string {
	for _, c := range categories {
		for _, m := range c.Members {
			if m == name {
				return c.Name
			}
		}
	}
	return "Other"
}

// Categories returns the category names in display order.
func (a *App) Categories() []string {
	out := []string{}
	for _, c := range categories {
		out = append(out, c.Name)
	}
	return append(out, "Other")
}
