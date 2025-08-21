package route

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
)

type HashRing struct {
	replicas int
	keys     []uint32
	ring     map[uint32]string
	nodes    map[string]struct{}
	mu       sync.RWMutex
}

func NewHashRing(replicas int) *HashRing {
	if replicas <= 0 {
		replicas = 128
	}
	return &HashRing{
		replicas: replicas,
		ring:     make(map[uint32]string),
		nodes:    make(map[string]struct{}),
	}
}

func (h *HashRing) Add(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.nodes[id]; ok {
		return
	}
	for i := 0; i < h.replicas; i++ {
		k := hash(fmt.Sprintf("%s#%d", id, i))
		h.ring[k] = id
		h.keys = append(h.keys, k)
	}
	sort.Slice(h.keys, func(i, j int) bool { return h.keys[i] < h.keys[j] })
	h.nodes[id] = struct{}{}
}

func (h *HashRing) Remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.nodes[id]; !ok {
		return
	}
	delete(h.nodes, id)
	// rebuild ring/keys
	h.ring = make(map[uint32]string)
	h.keys = h.keys[:0]
	for nid := range h.nodes {
		for i := 0; i < h.replicas; i++ {
			k := hash(fmt.Sprintf("%s#%d", nid, i))
			h.ring[k] = nid
			h.keys = append(h.keys, k)
		}
	}
	sort.Slice(h.keys, func(i, j int) bool { return h.keys[i] < h.keys[j] })
}

func (h *HashRing) Get(key string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.keys) == 0 {
		return "", false
	}
	hk := hash(key)
	idx := sort.Search(len(h.keys), func(i int) bool { return h.keys[i] >= hk })
	if idx == len(h.keys) {
		idx = 0
	}
	id := h.ring[h.keys[idx]]
	return id, true
}

func hash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}
