package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
)

// Command functions return tea.Cmd for Bubble Tea

// checkHealthCmd returns a command that checks the server health across all endpoints.
func (m Model) checkHealthCmd() tea.Cmd {
	return func() tea.Msg {
		err := m.client.GetHealth()
		status := m.client.GetHealthStatus()
		return healthMsg{ok: err == nil, status: status}
	}
}

// fetchWorkersCmd returns a command that fetches the worker list.
// If selectedPodOrigin is set, queries only that pod; otherwise merges all pods.
func (m Model) fetchWorkersCmd() tea.Cmd {
	return func() tea.Msg {
		var workers []api.WorkerSummary
		var err error

		if m.selectedPodOrigin != "" {
			workers, err = m.client.ListWorkersAt(m.selectedPodOrigin)
		} else {
			workers, err = m.client.ListWorkers()
		}

		if err != nil {
			return errMsg{err}
		}
		return workersMsg(workers)
	}
}

// fetchWorkerDetailsCmd returns a command that fetches details for a specific worker from its origin.
func (m Model) fetchWorkerDetailsCmd(workerName, origin string) tea.Cmd {
	return func() tea.Msg {
		details, err := m.client.GetWorker(workerName, origin)
		if err != nil {
			return errMsg{err}
		}
		return workerDetailsMsg(details)
	}
}

// actionWorkerCmd returns a command that performs an action on a worker at the given origin.
func (m Model) actionWorkerCmd(workerName, origin, action string) tea.Cmd {
	return func() tea.Msg {
		err := m.client.Action(workerName, action, origin)
		if err != nil {
			return errMsg{wrapError(fmt.Sprintf("failed to %s worker %s", action, workerName), err)}
		}
		return m.fetchWorkersCmd()()
	}
}

// actionCurrentWorkerCmd returns a command that performs an action on the currently viewed worker.
func (m Model) actionCurrentWorkerCmd(action string) tea.Cmd {
	if m.currentWorkerName == "" {
		return nil
	}

	return func() tea.Msg {
		err := m.client.Action(m.currentWorkerName, action, m.currentWorkerOrigin)
		if err != nil {
			return errMsg{wrapError(fmt.Sprintf("failed to %s worker", action), err)}
		}
		return m.fetchWorkerDetailsCmd(m.currentWorkerName, m.currentWorkerOrigin)()
	}
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
