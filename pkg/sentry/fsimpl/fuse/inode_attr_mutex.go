package fuse

import (
	"reflect"

	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/sync/locking"
)

// Mutex is sync.Mutex with the correctness validator.
type inodeAttrMutex struct {
	mu sync.Mutex
}

var inodeAttrprefixIndex *locking.MutexClass

// lockNames is a list of user-friendly lock names.
// Populated in init.
var inodeAttrlockNames []string

// lockNameIndex is used as an index passed to NestedLock and NestedUnlock,
// referring to an index within lockNames.
// Values are specified using the "consts" field of go_template_instance.
type inodeAttrlockNameIndex int

// DO NOT REMOVE: The following function automatically replaced with lock index constants.
// LOCK_NAME_INDEX_CONSTANTS
const ()

// Lock locks m.
// +checklocksignore
func (m *inodeAttrMutex) Lock() {
	locking.AddGLock(inodeAttrprefixIndex, -1)
	m.mu.Lock()
}

// NestedLock locks m knowing that another lock of the same type is held.
// +checklocksignore
func (m *inodeAttrMutex) NestedLock(i inodeAttrlockNameIndex) {
	locking.AddGLock(inodeAttrprefixIndex, int(i))
	m.mu.Lock()
}

// Unlock unlocks m.
// +checklocksignore
func (m *inodeAttrMutex) Unlock() {
	locking.DelGLock(inodeAttrprefixIndex, -1)
	m.mu.Unlock()
}

// NestedUnlock unlocks m knowing that another lock of the same type is held.
// +checklocksignore
func (m *inodeAttrMutex) NestedUnlock(i inodeAttrlockNameIndex) {
	locking.DelGLock(inodeAttrprefixIndex, int(i))
	m.mu.Unlock()
}

// DO NOT REMOVE: The following function is automatically replaced.
func inodeAttrinitLockNames() {}

func init() {
	inodeAttrinitLockNames()
	inodeAttrprefixIndex = locking.NewMutexClass(reflect.TypeFor[inodeAttrMutex](), inodeAttrlockNames)
}
