package fuse

import (
	"reflect"

	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/sync/locking"
)

// Mutex is sync.Mutex with the correctness validator.
type inodeIOMutex struct {
	mu sync.Mutex
}

var inodeIOprefixIndex *locking.MutexClass

// lockNames is a list of user-friendly lock names.
// Populated in init.
var inodeIOlockNames []string

// lockNameIndex is used as an index passed to NestedLock and NestedUnlock,
// referring to an index within lockNames.
// Values are specified using the "consts" field of go_template_instance.
type inodeIOlockNameIndex int

// DO NOT REMOVE: The following function automatically replaced with lock index constants.
// LOCK_NAME_INDEX_CONSTANTS
const ()

// Lock locks m.
// +checklocksignore
func (m *inodeIOMutex) Lock() {
	locking.AddGLock(inodeIOprefixIndex, -1)
	m.mu.Lock()
}

// NestedLock locks m knowing that another lock of the same type is held.
// +checklocksignore
func (m *inodeIOMutex) NestedLock(i inodeIOlockNameIndex) {
	locking.AddGLock(inodeIOprefixIndex, int(i))
	m.mu.Lock()
}

// Unlock unlocks m.
// +checklocksignore
func (m *inodeIOMutex) Unlock() {
	locking.DelGLock(inodeIOprefixIndex, -1)
	m.mu.Unlock()
}

// NestedUnlock unlocks m knowing that another lock of the same type is held.
// +checklocksignore
func (m *inodeIOMutex) NestedUnlock(i inodeIOlockNameIndex) {
	locking.DelGLock(inodeIOprefixIndex, int(i))
	m.mu.Unlock()
}

// DO NOT REMOVE: The following function is automatically replaced.
func inodeIOinitLockNames() {}

func init() {
	inodeIOinitLockNames()
	inodeIOprefixIndex = locking.NewMutexClass(reflect.TypeFor[inodeIOMutex](), inodeIOlockNames)
}
