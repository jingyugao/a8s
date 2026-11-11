package codex

import (
	"context"
	"strings"

	"github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
)

func (a *Agent) ReadThread(ctx context.Context, req *v1.ReadThreadRequest) (*v1.ReadThreadResponse, error) {
	if req == nil || req.PageSize > 100 {
		return nil, contract.NewError("Invalid", "thread request and page size up to 100 are required")
	}
	native, done, err := a.enter(req.Binding, false)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := a.ownThread(req.ThreadId); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	a.journalMu.Lock()
	empty := a.emptyThreads[req.ThreadId]
	a.journalMu.Unlock()
	if empty && req.BeforeTurnId == "" {
		return &v1.ReadThreadResponse{}, nil
	}
	var read struct{ Thread nativeThread }
	if err := native.call(ctx, "thread/read", map[string]any{"threadId": req.ThreadId, "includeTurns": true}, &read); err != nil {
		return nil, err
	}
	if read.Thread.ID != req.ThreadId {
		return nil, contract.NewError("Conflict", "native history returned a different thread")
	}
	end := len(read.Thread.Turns)
	if req.BeforeTurnId != "" {
		end = -1
		for i, turn := range read.Thread.Turns {
			if turn.ID == req.BeforeTurnId {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, contract.NewError("Invalid", "history cursor does not belong to this thread")
		}
	}
	limit := int(req.PageSize)
	if limit == 0 {
		limit = 20
	}
	start := max(0, end-limit)
	result := &v1.ReadThreadResponse{}
	if start > 0 {
		result.NextBeforeTurnId = read.Thread.Turns[start].ID
	}
	for _, turn := range read.Thread.Turns[start:end] {
		for _, item := range turn.Items {
			message := &v1.ThreadMessage{TurnId: turn.ID}
			switch item.Type {
			case "agentMessage":
				message.Role, message.Text = "assistant", item.Text
			case "userMessage":
				message.Role, message.RequestId = "user", item.ClientID
				var parts []string
				for _, content := range item.Content {
					if content.Type == "text" {
						parts = append(parts, content.Text)
					}
				}
				message.Text = strings.Join(parts, "\n")
			default:
				continue // Never expose tool arguments, credentials or raw native items.
			}
			result.Messages = append(result.Messages, message)
		}
	}
	return result, nil
}
