package basic

import (
	"context"
	"syscall"

	clusteriface "github.com/guseggert/clustertest/cluster"
)

type Process struct {
	Ctx     context.Context
	Process clusteriface.Process
}

func (p *Process) Context(ctx context.Context) *Process {
	newP := *p
	newP.Ctx = ctx
	return &newP
}

func (p *Process) Wait() (*clusteriface.ProcessResult, error) {
	return p.Process.Wait(p.Ctx)
}

func (p *Process) MustWait() *clusteriface.ProcessResult {
	res, err := p.Wait()
	Must(err)
	return res
}

func (p *Process) Signal(sig syscall.Signal) error {
	return p.Process.Signal(p.Ctx, sig)
}

func (p *Process) MustSignal(sig syscall.Signal) {
	Must(p.Signal(sig))
}
