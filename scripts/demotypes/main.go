// Command demotypes prints the storage and notification channel types, the
// database engines (GET /api/engines), the MCP tools (GET /api/mcp/tools) and
// the Markdown API reference (GET /api/docs.md) as JSON for the demo on
// dumpcat.cc (scripts/build-demo.py), so the demo's forms and pages match the app's.
//
//	go run ./scripts/demotypes > site-src/demo/types.json
package main

import (
	"encoding/json"
	"os"
	"strings"

	"dumpcat/internal/api"
	"dumpcat/internal/drivers"
	"dumpcat/internal/engine"
	"dumpcat/internal/notify"
	"dumpcat/internal/storage"
)

func main() {
	var engines []map[string]any
	for _, d := range drivers.All() {
		_, sb := d.(engine.Sandbox)
		e := map[string]any{"engine": d.Engine(), "label": d.Label(), "product": d.Product(), "default_port": d.DefaultPort(),
			"methods": d.Methods(), "restore_options": d.RestoreOptions(), "drills": sb}
		if la, ok := d.(engine.LogArchiver); ok {
			e["log"] = la.LogInfo()
		}
		engines = append(engines, e)
	}
	version := "dev"
	if b, err := os.ReadFile("VERSION"); err == nil {
		version = strings.TrimSpace(string(b))
	}
	reference, tools := api.DemoReference(version, "https://backup.example.com")
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(map[string]any{"storage_types": storage.Types, "notify_types": notify.Types, "engines": engines,
		"mcp_tools": tools, "api_reference": reference}); err != nil {
		os.Exit(1)
	}
}
