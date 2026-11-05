package app

import (
	"context"
	"errors"
	"time"
)

func (a *App) beginEvolution(ctx context.Context) (context.Context, func(), error) {
	a.stateMu.Lock()
	if a.closed {
		a.stateMu.Unlock()
		return nil, nil, errors.New("application is closed")
	}
	a.evolutionWork.Add(1)
	shutdown := a.evolutionContext
	a.stateMu.Unlock()
	worker, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(shutdown, cancel)
	return worker, func() { stop(); cancel(); a.evolutionWork.Done() }, nil
}

func (a *App) joinEvolution() bool {
	done := make(chan struct{})
	go func() { a.evolutionWork.Wait(); close(done) }()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
