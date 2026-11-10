// Package state stores rebuildable terminal input results beside native history.
// It lets a sleeping Guest answer result polls without starting the harness.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"google.golang.org/protobuf/proto"
)

type result struct {
	Binding *v1.Binding
	Input   *v1.InputResult
}

func resultPath(directory, id string) string {
	hash := sha256.Sum256([]byte(id))
	return filepath.Join(directory, "results", hex.EncodeToString(hash[:])+".json")
}
func LoadResult(directory string, request *v1.GetInputResultRequest) (*v1.GetInputResultResponse, error) {
	file, err := os.Open(resultPath(directory, request.GetRequestId()))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entry result
	if err := json.NewDecoder(io.LimitReader(file, 32<<20)).Decode(&entry); err != nil {
		return nil, err
	}
	if !proto.Equal(entry.Binding, request.Binding) || entry.Input.GetRequestId() != request.RequestId || entry.Input.GetThreadId() != request.ThreadId || (request.TurnId != "" && entry.Input.GetTurnId() != request.TurnId) {
		return nil, errors.New("cached result binding does not match")
	}
	switch entry.Input.GetState() {
	case "Succeeded", "Failed", "Cancelled":
	default:
		return nil, errors.New("cached result is not terminal")
	}
	return &v1.GetInputResultResponse{Result: entry.Input}, nil
}
func SaveResult(directory string, binding *v1.Binding, input *v1.InputResult) error {
	switch input.GetState() {
	case "Succeeded", "Failed", "Cancelled":
	default:
		return nil
	}
	path := resultPath(directory, input.RequestId)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(result{Binding: binding, Input: input})
	if err != nil {
		return err
	}
	if len(data) > 32<<20 {
		return errors.New("terminal result exceeds cache limit")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".result-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
