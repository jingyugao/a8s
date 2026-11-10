package codex

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	"google.golang.org/protobuf/proto"
)

var eventSequence atomic.Uint64
var eventPrefix = rand.Text()

func (a *Agent) onLost() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.streamLost = true
	for sub := range a.subscribers {
		close(sub.failed)
		delete(a.subscribers, sub)
	}
}
func (a *Agent) onFrame(f frame) {
	var p struct {
		ThreadID, TurnID, Delta string
		RequestID               json.RawMessage
		Turn                    nativeTurn
	}
	if json.Unmarshal(f.Params, &p) != nil {
		a.onLost()
		return
	}
	if p.TurnID == "" {
		p.TurnID = p.Turn.ID
	}
	e := &v1.SubscribeResponse{EventId: fmt.Sprintf("live-%s-%d", eventPrefix, eventSequence.Add(1)), ThreadId: p.ThreadID, TurnId: p.TurnID}
	a.mu.Lock()
	defer a.mu.Unlock()
	if f.Method == "turn/started" || f.Method == "turn/completed" || f.Method == "serverRequest/resolved" || len(f.ID) > 0 {
		a.lastActivity = time.Now()
	}
	if len(f.ID) > 0 {
		id := string(f.ID)
		a.interactions[id] = interaction{ID: append(json.RawMessage(nil), f.ID...), ThreadID: p.ThreadID, TurnID: p.TurnID, Kind: f.Method}
		e.Type = "InteractionRequired"
		e.InteractionId = id
		e.InteractionKind = f.Method
	} else {
		switch f.Method {
		case "item/agentMessage/delta":
			e.Type = "OutputDelta"
			e.Text = p.Delta
		case "turn/started":
			e.Type = "TurnStarted"
		case "turn/completed":
			e.Type = "TurnCompleted"
		case "thread/status/changed":
			e.Type = "StatusChanged"
		case "serverRequest/resolved":
			delete(a.interactions, string(p.RequestID))
			if reply, ok := a.replies[string(p.RequestID)]; ok {
				delete(a.replies, string(p.RequestID))
				go func() {
					reply.done <- a.complete("interaction", reply.requestID, &v1.RespondInteractionResponse{Accepted: true})
				}()
			}
			e.Type = "InteractionResolved"
			e.InteractionId = string(p.RequestID)
		default:
			return // Never forward raw tool calls, environment, auth, error bodies or input.
		}
	}
	for sub := range a.subscribers {
		if sub.thread != p.ThreadID {
			continue
		}
		select {
		case sub.events <- proto.Clone(e).(*v1.SubscribeResponse):
		default:
			close(sub.failed)
			delete(a.subscribers, sub)
		}
	}
}
func (a *Agent) Subscribe(ctx context.Context, req *v1.SubscribeRequest, send func(*v1.SubscribeResponse) error) error {
	if req == nil || send == nil {
		return contract.NewError("Invalid", "subscription request and callback are required")
	}
	_, done, err := a.enter(req.Binding, false)
	if err != nil {
		return err
	}
	done()
	if err := a.ownThread(req.ThreadId); err != nil {
		return err
	}
	sub := &subscription{thread: req.ThreadId, events: make(chan *v1.SubscribeResponse, 64), failed: make(chan struct{})}
	a.mu.Lock()
	if a.streamLost {
		a.mu.Unlock()
		return contract.NewError("OutcomeUnknown", "native stream unavailable; query current input state")
	}
	if len(a.subscribers) >= 256 {
		a.mu.Unlock()
		return contract.NewError("NotReady", "subscriber limit reached")
	}
	a.subscribers[sub] = struct{}{}
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.subscribers, sub); a.mu.Unlock() }()
	if err := send(&v1.SubscribeResponse{EventId: fmt.Sprintf("live-%s-%d", eventPrefix, eventSequence.Add(1)), ThreadId: req.ThreadId, Type: "ResyncRequired"}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sub.failed:
			return contract.NewError("OutcomeUnknown", "live stream has a gap; query final input state")
		case event := <-sub.events:
			if err := send(event); err != nil {
				return err
			}
		}
	}
}
