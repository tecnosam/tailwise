package common

import (
	"context"
	"sync"
)

// Pool is a set of goroutines that stop together. WorkCtx is cancelled only
// when Stop runs out of time, so work in flight can finish on a normal stop.
type Pool struct {
	stop       chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
	workCtx    context.Context
	cancelWork context.CancelFunc
}

func NewPool() *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{stop: make(chan struct{}), workCtx: ctx, cancelWork: cancel}
}

// Each run should return once Stopped is closed.
func (p *Pool) Start(n int, run func()) {
	for range n {
		p.wg.Go(run)
	}
}

func (p *Pool) Stopped() <-chan struct{} { return p.stop }

func (p *Pool) Stopping() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

func (p *Pool) WorkCtx() context.Context { return p.workCtx }

// If ctx ends first, the work in flight is cancelled and Stop returns ctx's
// error once it has returned.
func (p *Pool) Stop(ctx context.Context) error {
	p.stopOnce.Do(func() { close(p.stop) })
	err := Wait(ctx, &p.wg)
	if err != nil {
		p.cancelWork()
		p.wg.Wait()
	}
	p.cancelWork()
	return err
}
