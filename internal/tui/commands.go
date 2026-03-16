package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Command functions return tea.Cmd for Bubble Tea

// checkHealthCmd returns a command that checks the server health
func (m Model) checkHealthCmd() tea.Cmd {
	return func() tea.Msg {
		err := m.client.GetHealth()
		return healthMsg{ok: err == nil}
	}
}

// fetchWorkersCmd returns a command that fetches the worker list
func (m Model) fetchWorkersCmd() tea.Cmd {
	return func() tea.Msg {
		workers, err := m.client.ListWorkers()
		if err != nil {
			return errMsg{err}
		}
		return workersMsg(workers)
	}
}

// fetchWorkerDetailsCmd returns a command that fetches details for a specific worker
func (m Model) fetchWorkerDetailsCmd(workerName string) tea.Cmd {
	return func() tea.Msg {
		details, err := m.client.GetWorker(workerName)
		if err != nil {
			return errMsg{err}
		}
		return workerDetailsMsg(details)
	}
}

// actionWorkerCmd returns a command that performs an action on a worker
func (m Model) actionWorkerCmd(workerName, action string) tea.Cmd {
	return func() tea.Msg {
		err := m.client.Action(workerName, action)
		if err != nil {
			return errMsg{wrapError(fmt.Sprintf("failed to %s worker %s", action, workerName), err)}
		}
		// Return the next command to fetch fresh data
		return m.fetchWorkersCmd()()
	}
}

// actionCurrentWorkerCmd returns a command that performs an action on the currently viewed worker
func (m Model) actionCurrentWorkerCmd(action string) tea.Cmd {
	if m.currentWorkerName == "" {
		return nil
	}

	return func() tea.Msg {
		err := m.client.Action(m.currentWorkerName, action)
		if err != nil {
			return errMsg{wrapError(fmt.Sprintf("failed to %s worker", action), err)}
		}
		// After action succeeds, fetch fresh worker details
		return m.fetchWorkerDetailsCmd(m.currentWorkerName)()
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
