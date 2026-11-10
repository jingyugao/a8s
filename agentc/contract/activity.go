package contract

import (
	"time"

	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
)

// ProcessIdentity includes kernel start ticks so PID reuse cannot turn a user
// process into a previously observed framework process. No argv or env is exposed.
type ProcessIdentity struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"startTicks"`
}

type Activity struct {
	Known               bool              `json:"known"`
	ActiveTurns         uint32            `json:"activeTurns"`
	ActiveRequests      uint32            `json:"activeRequests"`
	QueuedInputs        uint32            `json:"queuedInputs"`
	WaitingInteractions uint32            `json:"waitingInteractions"`
	BackgroundTasks     uint32            `json:"backgroundTasks"`
	LastActivity        time.Time         `json:"lastActivity"`
	Reason              string            `json:"reason,omitempty"`
	FrameworkProcesses  []ProcessIdentity `json:"frameworkProcesses,omitempty"`
}

func (a Activity) Idle() bool {
	return a.Known && a.ActiveTurns == 0 && a.ActiveRequests == 0 && a.QueuedInputs == 0 && a.WaitingInteractions == 0 && a.BackgroundTasks == 0
}

// RuntimeRequest is a loopback-only management request between Guest and its
// Agentc. It does not add a workload type or change the public execution RPCs.
type RuntimeRequest struct {
	Binding *v1.Binding `json:"binding"`
}
type IdleStopResult struct {
	ProcessStopped   bool `json:"processStopped"`
	HistoryPersisted bool `json:"historyPersisted"`
}
