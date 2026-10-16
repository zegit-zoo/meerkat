package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zegit-zoo/meerkat/internal/authz"
	"github.com/zegit-zoo/meerkat/internal/collections"
	"github.com/zegit-zoo/meerkat/internal/memory"
)

// memory_location_test.go: a failing memory store's error, which names
// its location, never reaches the caller (meerkat-mob#62).

const secretLocation = "s3://secret-bucket/kb/memory/personal/x.md at /srv/secret-dir"

// failingStore is a working local store whose writes fail with an error
// that names the store's location, as the S3 SDK and os errors do.
type failingStore struct{ memory.Store }

func (failingStore) Put(context.Context, string, []byte, memory.Precondition) (memory.Version, error) {
	return "", errors.New("put " + secretLocation + ": access denied")
}

func (failingStore) Stage(context.Context, string, []byte) (string, error) {
	return "", errors.New("stage " + secretLocation + ": access denied")
}

func TestSaveMemory_StoreErrorNamesNoLocation(t *testing.T) {
	store, err := memory.OpenLocal(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatal(err)
	}
	col := collections.FromPages("notes", nil)
	if err := col.AttachMemory(context.Background(), failingStore{store}); err != nil {
		t.Fatal(err)
	}
	reg, err := collections.New(col)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	h := saveMemoryHandler(reg, transportOptions{AllowAnonymousPersonal: true})
	// personal is saved; team, for a caller without team-write, is staged.
	caller := authz.NewContext(context.Background(), authz.NewGrants(
		authz.Identity{Subject: "alice", Issuer: "https://issuer.example"},
		map[string][]authz.Capability{"notes": {authz.CapRead, authz.CapPersonalWrite}}))
	for _, scope := range []string{"personal", "team"} {
		res, err := h(caller, callTool(map[string]any{"scope": scope, "title": "T", "content": "body"}))
		text := ""
		switch {
		case err != nil:
			text = err.Error()
		case res != nil:
			text = resultText(t, res)
		}
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("%s: a failing store reported success: %s", scope, text)
		}
		if strings.Contains(text, "secret") {
			t.Errorf("%s: the caller sees the store location: %s", scope, text)
		}
		if !strings.Contains(text, "server log") {
			t.Errorf("%s: the message should point at the server log: %s", scope, text)
		}
	}
}
