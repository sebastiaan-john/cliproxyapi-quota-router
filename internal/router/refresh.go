package router

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// AuthLister reads the host's credential list (host.auth.list).
type AuthLister interface {
	ListAuths(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error)
}

const refreshTimeout = 15 * time.Second

// errNoAuthIDs means the host answered from its on-disk fallback, which has no
// auth IDs. Applying it would wipe every cached snapshot.
var errNoAuthIDs = errors.New("host auth list has no auth IDs")

// Refresh runs one refresh pass: it syncs host auth state, drops data for
// removed auths and expires old session bindings. A failure keeps the
// previous host state, which ages out after three refresh intervals.
func (r *Router) Refresh(ctx context.Context) error {
	now := r.now()
	r.aff.prune(now)
	defer func() {
		// Bindings change on every pick; the refresh tick is the flush interval.
		// A crash between ticks loses the bindings made since the last one.
		if err := r.SaveState(); err != nil && r.log != nil {
			r.log("warn", "quota-router state save failed", map[string]any{"error": err.Error()})
		}
	}()
	if r.lister == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	entries, err := r.lister.ListAuths(ctx)
	if err == nil && len(entries) > 0 {
		err = errNoAuthIDs
		for _, e := range entries {
			if strings.TrimSpace(e.ID) != "" {
				err = nil
				break
			}
		}
	}
	if err != nil {
		r.store.RecordRefreshError(err.Error(), now)
		return err
	}
	states := make(map[string]HostAuthState, len(entries))
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		states[id] = HostAuthState{
			Disabled:       e.Disabled || strings.EqualFold(e.Status, "disabled"),
			NextRetryAfter: e.NextRetryAfter,
		}
	}
	r.store.ApplyHostStates(states, now)
	return nil
}

// StartRefresher runs Refresh immediately and then every refresh_interval.
// It is a no-op if already running.
func (r *Router) StartRefresher() {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if r.refreshStop != nil {
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	r.refreshStop, r.refreshDone = stop, done
	interval := r.Config().RefreshInterval
	go func() {
		defer close(done)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-stop
			cancel()
		}()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if err := r.Refresh(ctx); err != nil && r.log != nil && ctx.Err() == nil {
				r.log("warn", "quota-router refresh failed", map[string]any{"error": err.Error()})
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

// StopRefresher stops the background loop and waits until it has exited. The
// loop only blocks inside a host.auth.list callback, which the host answers
// without taking locks it holds while reconfiguring or shutting down a plugin,
// so the wait ends once that callback returns.
func (r *Router) StopRefresher() {
	r.refreshMu.Lock()
	stop, done := r.refreshStop, r.refreshDone
	r.refreshStop, r.refreshDone = nil, nil
	r.refreshMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

func (r *Router) refreshing() bool {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	return r.refreshStop != nil
}
