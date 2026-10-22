package config

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var ErrBusy = errors.New("运行或配置保存正在进行，请结束后重试")
var ErrRevisionConflict = errors.New("配置已被其他操作更新，请重新读取后保存")

func canonicalPath(path string) string {
	p, err := filepath.Abs(path)
	if err != nil {
		p = filepath.Clean(path)
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

var apiFileLocks sync.Map

// LockAPIFile serializes read-modify-write operations across both HTTP services.
func LockAPIFile(path string) func() {
	lock, _ := apiFileLocks.LoadOrStore(canonicalPath(path), &sync.Mutex{})
	m := lock.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

type activityGate struct {
	mu          sync.Mutex
	active      map[string]bool
	globalWrite bool
}

var activityGates sync.Map

func acquireActivity(root, scope string) (func(), error) {
	g, _ := activityGates.LoadOrStore(canonicalPath(root), &activityGate{active: map[string]bool{}})
	gate := g.(*activityGate)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if scope != "" {
		scope = canonicalPath(scope)
	}
	if gate.globalWrite || (scope == "" && len(gate.active) > 0) || gate.active[scope] {
		return nil, ErrBusy
	}
	if scope == "" {
		gate.globalWrite = true
	} else {
		gate.active[scope] = true
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			gate.mu.Lock()
			defer gate.mu.Unlock()
			if scope == "" {
				gate.globalWrite = false
			} else {
				delete(gate.active, scope)
			}
		})
	}, nil
}

// A world run and a world config edit take the same scope. Global Provider edits
// take the empty scope and exclude all activity in that application root.
func BeginWorldActivity(root, worldDir string) (func(), error) {
	return acquireActivity(root, worldDir)
}
func BeginProviderEdit(root string) (func(), error) { return acquireActivity(root, "") }
