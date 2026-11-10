package codex

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
)

// The journal is an at-most-once dispatch fence, not a native queue. It retains
// hashes and receipts only. An incomplete entry is always an unknown outcome.
type journalEntry struct {
	Fingerprint string
	Result      json.RawMessage `json:",omitempty"`
}

func journalName(kind, id string) string {
	return fmt.Sprintf("%x.json", sha256.Sum256([]byte(kind+"\x00"+id)))
}
func (a *Agent) replay(kind, id string, request any, response any) (bool, error) {
	if id == "" || len(id) > 256 {
		return true, contract.NewError("Invalid", "request ID must contain 1 to 256 bytes")
	}
	path := filepath.Join(a.cfg.RuntimeDirectory, "receipts", journalName(kind, id))
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	var entry journalEntry
	if json.Unmarshal(b, &entry) != nil {
		return true, contract.NewError("OutcomeUnknown", "incomplete native dispatch intent")
	}
	content, err := json.Marshal(request)
	if err != nil {
		return true, err
	}
	if entry.Fingerprint != fmt.Sprintf("%x", sha256.Sum256(content)) {
		return true, contract.NewError("Conflict", "request ID already used for different content")
	}
	if len(entry.Result) == 0 || string(entry.Result) == "null" {
		return true, contract.NewError("OutcomeUnknown", "original native request may have executed; query its receipt")
	}
	return true, json.Unmarshal(entry.Result, response)
}
func (a *Agent) claim(kind, id string, request any, response any) (bool, error) {
	if id == "" || len(id) > 256 {
		return false, contract.NewError("Invalid", "request ID must contain 1 to 256 bytes")
	}
	content, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	path := filepath.Join(a.cfg.RuntimeDirectory, "receipts", journalName(kind, id))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		b, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		var entry journalEntry
		if json.Unmarshal(b, &entry) != nil {
			return false, contract.NewError("OutcomeUnknown", "incomplete native dispatch intent")
		}
		if entry.Fingerprint != hash {
			return false, contract.NewError("Conflict", "request ID already used for different content")
		}
		if len(entry.Result) == 0 || string(entry.Result) == "null" {
			return false, contract.NewError("OutcomeUnknown", "original native request may have executed; query its receipt")
		}
		return false, json.Unmarshal(entry.Result, response)
	}
	if err != nil {
		return false, err
	}
	b, _ := json.Marshal(journalEntry{Fingerprint: hash})
	_, writeErr := file.Write(b)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr, syncDirectory(filepath.Dir(path))); err != nil {
		return false, err
	}
	a.journalMu.Lock()
	a.pendingIntents[journalName(kind, id)] = true
	a.journalMu.Unlock()
	return true, nil
}
func (a *Agent) complete(kind, id string, response any) error {
	path := filepath.Join(a.cfg.RuntimeDirectory, "receipts", journalName(kind, id))
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var entry journalEntry
	if err := json.Unmarshal(b, &entry); err != nil {
		return err
	}
	entry.Result, err = json.Marshal(response)
	if err != nil {
		return err
	}
	b, err = json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := atomicFile(path, b); err != nil {
		return err
	}
	a.journalMu.Lock()
	switch value := response.(type) {
	case *v1.OpenThreadResponse:
		a.emptyThreads[value.ThreadId] = true
	case *v1.SubmitInputResponse:
		if value.Result != nil && value.Result.TurnId != "" {
			delete(a.emptyThreads, value.Result.ThreadId)
		}
	}
	delete(a.pendingIntents, journalName(kind, id))
	a.journalMu.Unlock()
	return nil
}
func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func atomicFile(path string, content []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(content)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
