package tui

import (
	"fmt"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

// Command functions return tea.Cmd for Bubble Tea

// scoped tags cmd's result with the current pool (see scopedMsg).
func (m Model) scoped(cmd func() tea.Msg) tea.Cmd {
	pool := m.pool
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		if _, ok := msg.(scopedMsg); ok {
			return msg // already tagged by a nested command
		}
		return scopedMsg{pool: pool, msg: msg}
	}
}

// clientFor returns the pool client serving origin. An empty origin falls back
// to the first client, mirroring the client's own primary-endpoint fallback.
func (m Model) clientFor(origin string) (*api.Client, error) {
	if m.pool == nil {
		return nil, fmt.Errorf("no server connection available")
	}

	clients := m.pool.Clients()
	if origin != "" {
		for _, c := range clients {
			if c.PrimaryEndpoint() == origin {
				return c, nil
			}
		}
		return nil, fmt.Errorf("no varmq pod found for endpoint %s", origin)
	}

	if len(clients) == 0 {
		return nil, fmt.Errorf("no varmq pods discovered")
	}
	return clients[0], nil
}

// checkHealthCmd returns a command that checks server health across all
// discovered pods.
func (m Model) checkHealthCmd() tea.Cmd {
	return m.scoped(func() tea.Msg {
		if m.pool == nil {
			return healthMsg{ok: false, status: map[string]bool{}}
		}

		clients := m.pool.Clients()
		status := make(map[string]bool, len(clients))
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, c := range clients {
			wg.Add(1)
			go func(endpoint string) {
				defer wg.Done()
				ok := c.GetHealth() == nil
				mu.Lock()
				status[endpoint] = ok
				mu.Unlock()
			}(c.PrimaryEndpoint())
		}
		wg.Wait()

		ok := false
		for _, healthy := range status {
			if healthy {
				ok = true
				break
			}
		}
		return healthMsg{ok: ok, status: status}
	})
}

// fetchWorkersCmd returns a command that fetches the worker list.
// If selectedPodOrigin is set, queries only that pod; otherwise merges all pods.
func (m Model) fetchWorkersCmd() tea.Cmd {
	return m.scoped(func() tea.Msg {
		if m.pool == nil {
			return nil // source construction failed; the error is already shown
		}

		if m.selectedPodOrigin != "" {
			c, err := m.clientFor(m.selectedPodOrigin)
			if err != nil {
				return errMsg{err}
			}
			workers, err := c.ListWorkers()
			if err != nil {
				return errMsg{err}
			}
			return workersMsg(workers)
		}

		type result struct {
			workers []api.WorkerSummary
			err     error
		}
		clients := m.pool.Clients()
		results := make([]result, len(clients))
		var wg sync.WaitGroup
		for i, c := range clients {
			wg.Add(1)
			go func(i int, c *api.Client) {
				defer wg.Done()
				workers, err := c.ListWorkers()
				results[i] = result{workers: workers, err: err}
			}(i, c)
		}
		wg.Wait()

		var merged []api.WorkerSummary
		var errs []error
		for _, r := range results {
			if r.err != nil {
				errs = append(errs, r.err)
				continue
			}
			merged = append(merged, r.workers...)
		}
		if len(merged) == 0 && len(errs) > 0 {
			return errMsg{errs[0]}
		}
		return workersMsg(merged)
	})
}

// switchServerCmd builds the discovery source and pool for a target server in
// the background; a Kubernetes source may take seconds to start.
func (m Model) switchServerCmd(srv *config.Server, seq int) tea.Cmd {
	return func() tea.Msg {
		source, err := sourceFor(srv)
		if err != nil {
			return serverSwitchMsg{seq: seq, srv: srv, err: err}
		}
		return serverSwitchMsg{seq: seq, srv: srv, pool: api.NewPool(source, srv.Token)}
	}
}

// fetchWorkerDetailsCmd returns a command that fetches details for a specific worker from its origin.
func (m Model) fetchWorkerDetailsCmd(workerName, origin string) tea.Cmd {
	return m.scoped(func() tea.Msg {
		c, err := m.clientFor(origin)
		if err != nil {
			return errMsg{err}
		}
		details, err := c.GetWorker(workerName, origin)
		if err != nil {
			return errMsg{err}
		}
		return workerDetailsMsg(details)
	})
}

// actionWorkerCmd returns a command that performs an action on a worker at the given origin.
func (m Model) actionWorkerCmd(workerName, origin, action string) tea.Cmd {
	return m.scoped(func() tea.Msg {
		c, err := m.clientFor(origin)
		if err != nil {
			return errMsg{wrapError(fmt.Sprintf("failed to %s worker %s", action, workerName), err)}
		}
		err = c.Action(workerName, action, origin)
		if err != nil {
			return errMsg{wrapError(fmt.Sprintf("failed to %s worker %s", action, workerName), err)}
		}
		return m.fetchWorkersCmd()()
	})
}

// actionCurrentWorkerCmd returns a command that performs an action on the
// worker currently open in the detail view.
func (m Model) actionCurrentWorkerCmd(workerName, origin, action string) tea.Cmd {
	if workerName == "" {
		return nil
	}

	return m.scoped(func() tea.Msg {
		c, err := m.clientFor(origin)
		if err != nil {
			return errMsg{wrapError(fmt.Sprintf("failed to %s worker", action), err)}
		}
		err = c.Action(workerName, action, origin)
		if err != nil {
			return errMsg{wrapError(fmt.Sprintf("failed to %s worker", action), err)}
		}
		return m.fetchWorkerDetailsCmd(workerName, origin)()
	})
}

// tickCmd returns a command that sends a tick message after the configured interval
func (m Model) tickCmd() tea.Cmd {
	interval := m.server.Interval
	if interval <= 0 {
		interval = 5 // Default safe fallback
	}

	return tea.Tick(time.Duration(interval)*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// initialTickCmd returns the first tick command to start the polling cycle
func initialTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}
