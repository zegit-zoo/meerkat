package contentsource

import (
	"fmt"
	"path/filepath"

	"github.com/zegit-zoo/meerkat/internal/memory"
)

// writableStore is one memory: or intake: block, with the label config
// errors name it by and its location resolved as far as load time can.
type writableStore struct {
	label string
	spec  memory.Spec
	// dir is the absolute-or-config-relative directory of a local store,
	// or "" when it resolves inside a directory only known at runtime
	// (a fetched cache entry, the embedded build).
	dir string
}

// validateStoreOverlap refuses two writable stores that share objects or
// files: equal or nested (bucket, prefix) pairs on the same backend and
// endpoint, or nested local directories. Overlapping stores read each
// other's documents — an intake store nested in a memory store would
// load raw deposits as memory pages, and two collections on one memory
// prefix would share team and global writes.
func validateStoreOverlap(cfg Config, cfgPath string) error {
	cfgDir := filepath.Dir(cfgPath)
	var stores []writableStore
	add := func(label string, spec *memory.Spec, owner *Source) {
		if spec == nil {
			return
		}
		ws := writableStore{label: label, spec: *spec}
		if spec.Type == memory.BackendLocal {
			p := spec.Path
			if p == "" {
				p = memory.DefaultLocalPath
			}
			switch {
			case filepath.IsAbs(p):
				ws.dir = filepath.Clean(p)
			case owner != nil && owner.Type == TypeLocal:
				base := owner.Path
				if !filepath.IsAbs(base) {
					base = filepath.Join(cfgDir, base)
				}
				ws.dir = filepath.Clean(filepath.Join(base, p))
			}
		}
		stores = append(stores, ws)
	}
	add("content.memory", cfg.Content.Memory, &cfg.Content)
	for i := range cfg.Collections {
		c := &cfg.Collections[i]
		add(fmt.Sprintf("collections[%s].memory", c.Name), c.Memory, &c.Source)
	}
	if cfg.Tree != nil {
		add("tree.memory", cfg.Tree.Memory, cfg.Tree)
	}
	add("intake", cfg.Intake, nil)

	for i := range stores {
		for j := i + 1; j < len(stores); j++ {
			if storesOverlap(stores[i], stores[j]) {
				return fmt.Errorf("%s and %s overlap: two writable stores may not share a location (equal or nested bucket and prefix, or nested directories) — "+
					"each would read the other's documents; give each its own prefix or directory", stores[i].label, stores[j].label)
			}
		}
	}
	return nil
}

func storesOverlap(a, b writableStore) bool {
	if a.spec.Type == memory.BackendLocal || b.spec.Type == memory.BackendLocal {
		if a.spec.Type != b.spec.Type || a.dir == "" || b.dir == "" {
			return false
		}
		return within(a.dir, b.dir) || within(b.dir, a.dir)
	}
	return a.spec.Type == b.spec.Type && a.spec.Bucket == b.spec.Bucket &&
		normEndpoint(a.spec.Endpoint) == normEndpoint(b.spec.Endpoint) &&
		prefixesNest(a.spec.Prefix, b.spec.Prefix)
}
