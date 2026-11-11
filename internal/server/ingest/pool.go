package ingest

import "sync"

// PortPool hands out ingest ports. Safe for concurrent use.
type PortPool struct {
	mu    sync.Mutex
	ports []int
	used  map[int]bool
	next  int
}

// NewPortPool creates a pool over the given ports.
func NewPortPool(ports []int) *PortPool {
	return &PortPool{ports: append([]int(nil), ports...), used: map[int]bool{}}
}

// Acquire returns a free port, or false if all are in use. Ports are handed
// out round-robin so a just-released port is not immediately reused (a stale
// connection for an old session then cannot race a new one).
func (p *PortPool) Acquire() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.ports {
		idx := (p.next + i) % len(p.ports)
		port := p.ports[idx]
		if !p.used[port] {
			p.used[port] = true
			p.next = idx + 1
			return port, true
		}
	}
	return 0, false
}

// Release returns a port to the pool. Releasing a free port is a no-op.
func (p *PortPool) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used, port)
}

// InUse reports the number of allocated ports.
func (p *PortPool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.used)
}

// Size is the total number of ports.
func (p *PortPool) Size() int { return len(p.ports) }
