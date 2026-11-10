package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/jingyugao/a8s/agentc/contract"
)

type frame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type rpc struct {
	mu      sync.Mutex
	pending map[string]chan frame
	out     chan frame
	done    chan struct{}
	err     error
	next    atomic.Uint64
	onFrame func(frame)
	onLost  func()
}

func newRPC(reader io.Reader, writer io.Writer, onFrame func(frame), onLost func()) *rpc {
	r := &rpc{pending: make(map[string]chan frame), out: make(chan frame, 16), done: make(chan struct{}), onFrame: onFrame, onLost: onLost}
	go func() {
		enc := json.NewEncoder(writer)
		for {
			select {
			case <-r.done:
				return
			case f := <-r.out:
				if err := enc.Encode(f); err != nil {
					r.fail(err)
					return
				}
			}
		}
	}()
	go func() {
		scan := bufio.NewScanner(reader)
		scan.Buffer(make([]byte, 4096), 4*1024*1024)
		for scan.Scan() {
			var f frame
			if err := json.Unmarshal(scan.Bytes(), &f); err != nil {
				r.fail(err)
				return
			}
			if f.Method != "" {
				if r.onFrame != nil {
					r.onFrame(f)
				}
				continue
			}
			r.mu.Lock()
			ch := r.pending[string(f.ID)]
			r.mu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
		}
		err := scan.Err()
		if err == nil {
			err = io.EOF
		}
		r.fail(err)
	}()
	return r
}
func (r *rpc) fail(err error) {
	r.mu.Lock()
	if r.err != nil {
		r.mu.Unlock()
		return
	}
	r.err = err
	close(r.done)
	r.mu.Unlock()
	if r.onLost != nil {
		r.onLost()
	}
}
func (r *rpc) send(ctx context.Context, f frame) error {
	select {
	case <-r.done:
		return r.failure()
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return r.failure()
	case r.out <- f:
		return nil
	}
}
func (r *rpc) failure() error { r.mu.Lock(); defer r.mu.Unlock(); return r.err }
func (r *rpc) call(ctx context.Context, method string, params any, dest any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := strconv.FormatUint(r.next.Add(1), 10)
	ch := make(chan frame, 1)
	r.mu.Lock()
	if len(r.pending) >= 128 {
		r.mu.Unlock()
		return contract.NewError("NotReady", "native RPC concurrency limit reached")
	}
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }()
	if err := r.send(ctx, frame{ID: json.RawMessage(id), Method: method, Params: body}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return r.failure()
	case f := <-ch:
		if f.Error != nil {
			return &contract.Error{Code: "OutcomeUnknown", Source: "codex.app-server", NativeCode: strconv.Itoa(f.Error.Code), Message: "native request returned an error", Cause: errors.New(f.Error.Message)}
		}
		if dest == nil {
			return nil
		}
		if len(f.Result) == 0 {
			return fmt.Errorf("native response omitted result")
		}
		return json.Unmarshal(f.Result, dest)
	}
}
func (r *rpc) notify(ctx context.Context, method string, params any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return r.send(ctx, frame{Method: method, Params: body})
}
