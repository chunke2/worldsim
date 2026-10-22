package config

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestActivityGateBlocksBothDirectionsUntilActualRelease(t *testing.T) {
	root := t.TempDir()
	world := filepath.Join(root, "worlds", "one")
	end, err := BeginWorldActivity(root, world)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BeginProviderEdit(root); !errors.Is(err, ErrBusy) {
		t.Fatal("global edit allowed during run")
	}
	if _, err := BeginWorldActivity(root, world); !errors.Is(err, ErrBusy) {
		t.Fatal("duplicate run/edit allowed")
	}
	other, err := BeginWorldActivity(root, filepath.Join(root, "worlds", "other"))
	if err != nil {
		t.Fatal("unrelated world unnecessarily blocked")
	}
	other()
	end()
	end() // cleanup is idempotent
	edit, err := BeginProviderEdit(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BeginWorldActivity(root, world); !errors.Is(err, ErrBusy) {
		t.Fatal("run started during global edit")
	}
	edit()
	if end, err := BeginWorldActivity(root, world); err != nil {
		t.Fatal(err)
	} else {
		end()
	}
}

func TestConcurrentProviderUpdatesHaveOnlyOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.json")
	r, _ := runtimeFixture()
	base := &APIConfig{Providers: r.Providers, DefaultProvider: r.DefaultProvider}
	if err := SaveAPIConfig(base, path); err != nil {
		t.Fatal(err)
	}
	base, err := ReadAPIConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	revision := APIRevision(base)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := UpdateProviders(path, ProviderUpdate{Revision: revision, Providers: map[string]json.RawMessage{"primary": json.RawMessage(`{"name":"changed"}`)}})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		if err == nil {
			winners++
		} else if errors.Is(err, ErrRevisionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatal("concurrent writers overwrote each other")
	}
	next, err := MergeAPIConfig(base, []byte(`{"model":"new"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAPIConfig(next, path); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateProviders(path, ProviderUpdate{Revision: revision}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal("legacy save failed to invalidate Provider revision")
	}
}
