package fuse

import (
	"reflect"

	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/sync/locking"
)

// Mutex is sync.Mutex with the correctness validator.
type inodeDataMutex struct {
	mu sync.Mutex
}

var inodeDataprefixIndex *locking.MutexClass

// lockNames is a list of user-friendly lock names.
// Populated in init.
var inodeDatalockNames []string

// lockNameIndex is used as an index passed to NestedLock and NestedUnlock,
// referring to an index within lockNames.
// Values are specified using the "consts" field of go_template_instance.
type inodeDatalockNameIndex int

// DO NOT REMOVE: The following function automatically replaced with lock index constants.
// LOCK_NAME_INDEX_CONSTANTS
const ()

// Lock locks m.
// +checklocksignore
func (m *inodeDataMutex) Lock() {
	locking.AddGLock(inodeDataprefixIndex, -1)
	m.mu.Lock()
}

// NestedLock locks m knowing that another lock of the same type is held.
// +checklocksignore
func (m *inodeDataMutex) NestedLock(i inodeDatalockNameIndex) {
	locking.AddGLock(inodeDataprefixIndex, int(i))
	m.mu.Lock()
}

// Unlock unlocks m.
// +checklocksignore
func (m *inodeDataMutex) Unlock() {
	locking.DelGLock(inodeDataprefixIndex, -1)
	m.mu.Unlock()
}

// NestedUnlock unlocks m knowing that another lock of the same type is held.
// +checklocksignore
func (m *inodeDataMutex) NestedUnlock(i inodeDatalockNameIndex) {
	locking.DelGLock(inodeDataprefixIndex, int(i))
	m.mu.Unlock()
}

// DO NOT REMOVE: The following function is automatically replaced.
func inodeDatainitLockNames() {}

func init() {
	inodeDatainitLockNames()
	inodeDataprefixIndex = locking.NewMutexClass(reflect.TypeFor[inodeDataMutex](), inodeDatalockNames)
}
