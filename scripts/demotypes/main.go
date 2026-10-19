// Command demotypes prints the storage and notification channel types as
// JSON for the demo on dumpcat.cc (scripts/build-demo.py), so the demo's
// forms match the app's.
//
//	go run ./scripts/demotypes > site-src/demo/types.json
package main

import (
	"encoding/json"
	"os"

	"dumpcat/internal/notify"
	"dumpcat/internal/storage"
)

func main() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(map[string]any{"storage_types": storage.Types, "notify_types": notify.Types}); err != nil {
		os.Exit(1)
	}
}
