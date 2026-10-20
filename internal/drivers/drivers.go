// Package drivers lists the database engines DumpCat backs up.
package drivers

import (
	"dumpcat/internal/engine"
	"dumpcat/internal/store"
)

var all = []engine.Driver{engine.MySQL{}}

// All returns every driver, MySQL first.
func All() []engine.Driver { return all }

// For returns the driver of an engine name ("" is MySQL), nil when unknown.
func For(name string) engine.Driver {
	if name == "" {
		name = engine.EngineMySQL
	}
	for _, d := range all {
		if d.Engine() == name {
			return d
		}
	}
	return nil
}

// ForServer returns the driver of a server.
func ForServer(s *store.Server) engine.Driver { return For(s.EngineName()) }

// ForMethod returns the driver that owns a backup method, nil when unknown.
func ForMethod(method string) engine.Driver {
	for _, d := range all {
		if _, ok := engine.FindMethod(d, method); ok {
			return d
		}
	}
	return nil
}

// Method returns a backup method and its driver.
func Method(id string) (engine.Method, engine.Driver, bool) {
	for _, d := range all {
		if m, ok := engine.FindMethod(d, id); ok {
			return m, d, true
		}
	}
	return engine.Method{}, nil, false
}

// Sandbox returns the restore-drill side of a server's driver (nil: drills
// are not available for its engine).
func Sandbox(s *store.Server) engine.Sandbox {
	sb, _ := ForServer(s).(engine.Sandbox)
	return sb
}
