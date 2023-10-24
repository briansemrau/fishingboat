package main

import (
	"fmt"
	"sync"
)

type MutexMap struct {
	mutexmap map[interface{}]*entry
	mutex    sync.Mutex
}

type entry struct {
	mm    *MutexMap
	mutex *sync.Mutex
	count int
	key   interface{}
}

func NewMutexMap() *MutexMap {
	return &MutexMap{mutexmap: make(map[interface{}]*entry)}
}

func (mm *MutexMap) Lock(key interface{}) {
	mm.mutex.Lock()
	defer mm.mutex.Unlock()
	e, ok := mm.mutexmap[key]
	if !ok {
		e = &entry{mm: mm, mutex: new(sync.Mutex), count: 0, key: key}
		mm.mutexmap[key] = e
	}
	e.count++
	e.mutex.Lock()
}

func (mm *MutexMap) Unlock(key interface{}) {
	mm.mutex.Lock()
	defer mm.mutex.Unlock()
	e, ok := mm.mutexmap[key]
	if !ok {
		panic(fmt.Errorf("unlocking entry not found in mutexmap for key %v", key))
	}
	e.count--
	if e.count <= 0 {
		delete(mm.mutexmap, key)
	}
	e.mutex.Unlock()
}
