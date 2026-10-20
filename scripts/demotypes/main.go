// Command demotypes prints the storage and notification channel types and
// the database engines (GET /api/engines) as JSON for the demo on
// dumpcat.cc (scripts/build-demo.py), so the demo's forms match the app's.
//
//	go run ./scripts/demotypes > site-src/demo/types.json
package main

import (
	"encoding/json"
	"os"

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
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(map[string]any{"storage_types": storage.Types, "notify_types": notify.Types, "engines": engines}); err != nil {
		os.Exit(1)
	}
}
