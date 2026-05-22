package monitor

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

type GitHubListener struct {
	dispatcher *GitHubDispatcher

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}

	pollFn       func(context.Context) ([]GitHubEvent, error)
	pollInterval time.Duration
	logFn        func(string, ...any)
}

func NewGitHubListener(d *GitHubDispatcher) *GitHubListener {
	if d == nil {
		return nil
	}
	return &GitHubListener{
		dispatcher:   d,
		pollInterval: GitHubPollInterval(),
		logFn: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "[github listener] "+format+"\n", args...)
		},
	}
}

func (l *GitHubListener) Start() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return nil
	}
	if !GitHubPollingEnabled() {
		l.logFn("not starting: set FLOW_GH_ENABLED=1 and FLOW_GH_SELF_LOGINS")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.done = make(chan struct{})
	l.running = true
	go func() {
		defer close(l.done)
		l.run(ctx)
	}()
	return nil
}

func (l *GitHubListener) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return
	}
	cancel := l.cancel
	done := l.done
	l.running = false
	l.cancel = nil
	l.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			l.logFn("stop: timeout waiting for listener goroutine to exit")
		}
	}
}

func (l *GitHubListener) run(ctx context.Context) {
	l.pollOnce(ctx)
	interval := l.pollInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.pollOnce(ctx)
		}
	}
}

func (l *GitHubListener) pollOnce(ctx context.Context) {
	events, err := l.poll(ctx)
	if err != nil {
		l.logFn("poll: %v", err)
		return
	}
	for _, ev := range events {
		if err := l.dispatcher.Dispatch(ctx, ev); err != nil {
			l.logFn("dispatch %s: %v", ev.EventKeyValue(), err)
		}
	}
}

func (l *GitHubListener) poll(ctx context.Context) ([]GitHubEvent, error) {
	if l.pollFn != nil {
		return l.pollFn(ctx)
	}
	p := GitHubPoller{
		DB:         l.dispatcher.DB,
		Client:     ghAPIClient{},
		SelfLogins: GitHubSelfLogins(),
		Repos:      GitHubRepos(),
	}
	return p.Poll(ctx)
}
