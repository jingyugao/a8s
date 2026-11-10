package control

import (
	"errors"
	v1 "github.com/jingyugao/a8s/api/gen/a8s/v1"
	"time"
)

func IdlePolicy(policy *v1.IdleReclaimPolicy) (timeout, interval time.Duration, err error) {
	timeout, interval = 10*time.Minute, time.Minute
	if policy == nil {
		return
	}
	if policy.IdleTimeout != "" {
		timeout, err = time.ParseDuration(policy.IdleTimeout)
		if err != nil {
			return
		}
	}
	if policy.CheckInterval != "" {
		interval, err = time.ParseDuration(policy.CheckInterval)
		if err != nil {
			return
		}
	}
	if timeout <= 0 || interval <= 0 {
		err = errors.New("idleTimeout and checkInterval must be positive durations")
	}
	return
}
