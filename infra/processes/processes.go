//go:build linux

// Package processes observes Linux identities without treating process names or
// low CPU use as evidence that a background task may be terminated.
package processes

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	agent "github.com/jingyugao/a8s/agentc/contract"
)

type Info struct {
	agent.ProcessIdentity
	ParentPID int
	State     string
	UIDs      []uint32
}

func Read(pid int) (Info, error) {
	result := Info{ProcessIdentity: agent.ProcessIdentity{PID: pid}}
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	data, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return result, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return result, errors.New("invalid proc stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return result, errors.New("incomplete proc stat")
	}
	result.State = fields[0]
	result.ParentPID, err = strconv.Atoi(fields[1])
	if err != nil {
		return result, err
	}
	result.StartTicks, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return result, err
	}
	data, err = os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return result, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			for _, field := range strings.Fields(line)[1:] {
				uid, err := strconv.ParseUint(field, 10, 32)
				if err != nil {
					return result, err
				}
				result.UIDs = append(result.UIDs, uint32(uid))
			}
		}
	}
	if len(result.UIDs) != 4 {
		return result, fmt.Errorf("PID %d omitted UID identity", pid)
	}
	return result, nil
}

func ForUID(uid uint32) ([]Info, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var result []Info
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		info, err := Read(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, owner := range info.UIDs {
			if owner == uid {
				result = append(result, info)
				break
			}
		}
	}
	return result, nil
}
