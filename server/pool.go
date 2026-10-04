package main

import "sync"

// ocrPool bounds the CPU spent on OCR and transcription across every job in
// flight: n at a time on mains power, 1 on battery. Each slot carries a
// long-running helper, reused warm by the next job (Vision's model stays
// loaded) and stopped by itself once idle. Jobs that mostly wait on the
// network (remote images) hold no slot while they wait, so many of them can
// be in flight without oversubscribing the CPU.
type ocrPool struct {
	mu   sync.Mutex
	cond *sync.Cond
	n    int
	busy int
	free []*helperProc // most recently used last: warm helpers are reused first
}

func newOCRPool(n int) *ocrPool {
	p := &ocrPool{n: max(1, n)}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *ocrPool) limit() int {
	if onBattery() {
		return 1
	}
	return p.n
}

func (p *ocrPool) acquire() *helperProc {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.busy >= p.limit() {
		p.cond.Wait()
	}
	p.busy++
	if k := len(p.free); k > 0 {
		h := p.free[k-1]
		p.free = p.free[:k-1]
		return h
	}
	return &helperProc{nice: true}
}

func (p *ocrPool) release(h *helperProc) {
	p.mu.Lock()
	p.busy--
	p.free = append(p.free, h)
	p.mu.Unlock()
	p.cond.Broadcast()
}
