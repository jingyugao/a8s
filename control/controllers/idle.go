package controllers

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	agent "github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"github.com/jingyugao/a8s/control"
)

// IdleRuntime supplies observations and verified transitions for one already
// owned Workspace. Parking retains its logical Instance, binding and storage.
type IdleRuntime interface {
	ObserveActivity(context.Context) (agent.Activity, error)
	Park(context.Context) error
	Wake(context.Context) error
}

// IdleController owns policy, not process discovery. It is composed beside
// Guest in the runtime binary; it never creates a second Workspace owner.
type IdleController struct {
	mu                sync.Mutex
	runtime           IdleRuntime
	enabled           bool
	timeout, interval time.Duration
	lastActivity      time.Time
	active            uint32
	wasBusy           bool
	now               func() time.Time
}

func NewIdleController(policy *v1.IdleReclaimPolicy, runtime IdleRuntime) (*IdleController, error) {
	timeout, interval, err := control.IdlePolicy(policy)
	if err != nil {
		return nil, err
	}
	if runtime == nil {
		return nil, errors.New("runtime is required")
	}
	return &IdleController{runtime: runtime, enabled: policy.GetEnabled(), timeout: timeout, interval: interval, lastActivity: time.Now(), now: time.Now}, nil
}

// Begin serializes wake-up with park. Only mutations renew the idle deadline;
// result polling, subscriptions and health probes do not keep a runtime hot.
func (c *IdleController) Begin(ctx context.Context, mutation bool) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if mutation {
		if err := c.runtime.Wake(ctx); err != nil {
			return nil, err
		}
	}
	if mutation {
		c.active++
		c.lastActivity = c.now()
	}
	return func() {
		if mutation {
			c.mu.Lock()
			c.active--
			c.lastActivity = c.now()
			c.mu.Unlock()
		}
	}, nil
}

func (c *IdleController) Check(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || c.active != 0 {
		return nil
	}
	activity, err := c.runtime.ObserveActivity(ctx)
	if err != nil || !activity.Idle() {
		c.lastActivity, c.wasBusy = c.now(), true
		return err
	}
	if c.wasBusy {
		c.lastActivity, c.wasBusy = c.now(), false
	}
	if activity.LastActivity.After(c.lastActivity) {
		c.lastActivity = activity.LastActivity
	}
	if c.now().Sub(c.lastActivity) < c.timeout {
		return nil
	}
	// Park must freeze admission and repeat its own activity/process checks.
	if err := c.runtime.Park(ctx); err != nil {
		c.lastActivity = c.now()
		return err
	}
	return nil
}

func (c *IdleController) Run(ctx context.Context, onError func(error)) {
	if !c.enabled {
		return
	}
	// Randomize the initial phase while retaining the configured polling period.
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(c.interval))) + time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			check, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := c.Check(check)
			cancel()
			if err != nil && onError != nil {
				onError(err)
			}
			timer.Reset(c.interval)
		}
	}
}
