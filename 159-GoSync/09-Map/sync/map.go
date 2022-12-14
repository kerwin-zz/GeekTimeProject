package sync

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

type Map struct {
	mu sync.Mutex
	// 基本上你可以把它看成一个安全的只读的map
	// 它包含的元素其实也是通过原子操作更新的，但是已删除的entry就需要加锁操作了
	read atomic.Value // readOnly

	// 包含需要加锁才能访问的元素
	// 包括所有在read字段中但未被expunged（删除）的元素以及新加的元素
	dirty map[any]*entry

	// 记录从read中读取miss的次数，一旦miss数和dirty长度一样了，就会把dirty提升为read
	misses int
}

type readOnly struct {
	m       map[any]*entry
	amended bool // 当dirty中包含read没有的数据时为true，比如新增一条数据.
}

// expunged是用来标识此项已经删掉的指针
// 当map中的一个项目被删除了，只是把它的值标记为expunged，以后才有机会真正删除此项
var expunged = unsafe.Pointer(new(any))

// entry代表一个值
type entry struct {
	p unsafe.Pointer // *interface{}
}

func (m *Map) Load(key any) (value any, ok bool) {
	// 首先从read处理
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended { // 如果不存在并且dirty不为nil（有新元素）
		m.mu.Lock()
		// 双检查，看看read中现在是否存在此key
		read, _ := m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended { // 依然不存在，并且dirty不为nil
			e, ok = m.dirty[key] // 从dirty中读取
			// 不管dirty中存不存在，miss数都加1
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if !ok {
		return nil, false
	}
	return e.load() // 返回读取的对象，e即可能是从read中获得的，也可能是从dirty中获得的
}

// 用来设置一个键值对，或者更新一个键值对
func (m *Map) Store(key, value any) {
	read, _ := m.read.Load().(readOnly)
	// 如果read字段包括这个项，说明是更新，cas更新项目的值
	if e, ok := read.m[key]; ok && e.tryStore(&value) {
		return
	}
	// read中不存在，或者cas更新失败，就需要加锁访问dirty了
	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok { // 双检查，看看read是否已经存在了
		if e.unexpungeLocked() {
			// 此项目先前已经被设置为删除了，通过将它的值设置nil，标记为unexpunged
			m.dirty[key] = e
		}
		e.storeLocked(&value) // 更新
	} else if e, ok := m.dirty[key]; ok { // 如果dirty中有此项
		e.storeLocked(&value) // 更新
	} else { // 否则就是一个新key
		if !read.amended { // 如果dirty为nil
			// 需要创建dirty对象，并且标记read的amended为true
			// 说明有元素它不包含而dirty包含
			m.dirtyLocked()
			m.read.Store(readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value) // 将新值增加到dirty对象中
	}
	m.mu.Unlock()
}

func (m *Map) LoadAndDelete(key any) (value any, loaded bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended {
		m.mu.Lock()
		// 双检查
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended {
			e, ok = m.dirty[key]
			// 这一行长坤在1.15中实现的时候忘记加上了，导致在特殊的场景下有些key总是没有被回收
			delete(m.dirty, key)
			// miss数加1
			m.missLocked()
		}
		m.mu.Unlock()
	}
	if !ok {
		return e.delete()
	}
	return nil, false
}

func (m *Map) Delete(key any) {
	m.LoadAndDelete(key)
}

func (e *entry) delete() (value any, ok bool) {
	for {
		p := atomic.LoadPointer(&e.p)
		if p == nil || p == expunged {
			return nil, false
		}
		if atomic.CompareAndSwapPointer(&e.p, p, nil) {
			return *(*any)(p), true
		}
	}
}

func (m *Map) missLocked() {
	m.misses++                   // misses计数加一
	if m.misses < len(m.dirty) { // 如果没达到阈值(dirty字段的长度)，返回
		return
	}

	m.read.Store(readOnly{m: m.dirty}) // 把dirty字段的内存提升为read字段
	m.dirty = nil                      // 清空dirty
	m.misses = 0                       // misses数重置为0
}

func (m *Map) dirtyLocked() {
	if m.dirty != nil { // 如果dirty字段存在，不需要创建了
		return
	}

	read, _ := m.read.Load().(readOnly) // 获取read字段
	m.dirty = make(map[any]*entry, len(read.m))
	for k, e := range read.m { // 遍历read字段
		if !e.tryExpungeLocked() { // 把非punged的键值对复制到dirty中
			m.dirty[k] = e
		}
	}
}

func (e *entry) load() (value any, ok bool) {
	p := atomic.LoadPointer(&e.p)
	if p == nil || p == expunged {
		return nil, false
	}
	return *(*any)(p), true
}

// 如果entry没有被删除，tryStore会存储一个值。
//
// 如果entry被删除，tryStore返回false，并使entry保持不变。
func (e *entry) tryStore(i *any) bool {
	for {
		p := atomic.LoadPointer(&e.p)
		if p == expunged {
			return false
		}
		if atomic.CompareAndSwapPointer(&e.p, p, unsafe.Pointer(i)) {
			return true
		}
	}
}

// unexpungeLocked确保entry不会被标记为已删除。
//
// 如果该条目之前已被删除，则必须在m.mu解锁之前将其添加到dirty map中。
func (e *entry) unexpungeLocked() (wasExpunged bool) {
	return atomic.CompareAndSwapPointer(&e.p, expunged, nil)
}

// store Locked 无条件地为entry存储一个值。
//
// 必须知道该entry不会被删除。
func (e *entry) storeLocked(i *any) {
	atomic.StorePointer(&e.p, unsafe.Pointer(i))
}

func (e *entry) tryExpungeLocked() (isExpunged bool) {
	p := atomic.LoadPointer(&e.p)
	for p == nil {
		if atomic.CompareAndSwapPointer(&e.p, nil, expunged) {
			return true
		}
		p = atomic.LoadPointer(&e.p)
	}
	return p == expunged
}

func newEntry(i any) *entry {
	return &entry{p: unsafe.Pointer(&i)}
}
