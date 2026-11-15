package main

import (
	"context"
	"errors"
	"time"
)

type shutdowner interface {
	Shutdown(context.Context) error
}

type sshShutdowner interface {
	shutdowner
	Close() error
}

func shutdownDaemon(
	ctx context.Context,
	sshGrace time.Duration,
	public, metrics shutdowner,
	ssh sshShutdowner,
	waitFinalize func(),
) error {
	results := make(chan error, 3)
	var result error
	for _, drain := range []shutdowner{public, metrics} {
		go func() { results <- drain.Shutdown(ctx) }()
	}

	sshCtx, cancelSSH := context.WithTimeout(ctx, sshGrace)
	sshDone := make(chan error, 1)
	go func() { sshDone <- ssh.Shutdown(sshCtx) }()
	select {
	case err := <-sshDone:
		cancelSSH()
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				result = errors.Join(result, err)
			}
			closeDone := make(chan error, 1)
			go func() { closeDone <- ssh.Close() }()
			select {
			case closeErr := <-closeDone:
				result = errors.Join(result, closeErr)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	case <-ctx.Done():
		cancelSSH()
		return ctx.Err()
	}

	go func() {
		waitFinalize()
		results <- nil
	}()

	for range 3 {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				result = errors.Join(result, err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return result
}
