package execution

import (
	"context"

	"github.com/jingyugao/a8s/agentc/contract"
	v1 "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
)

// ListThreads reads the durable ownership registry without waking any runtime.
func (g *Gateway) ListThreads(ctx context.Context, r Resource, cursor string, limit int) ([]Record, string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	if err := g.authorize(ctx, r, "ListThreads", false); err != nil {
		return nil, "", err
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return nil, "", contract.NewError("Invalid", "page size must be between 1 and 100")
	}
	if cursor != "" {
		record, err := g.store.ByID(ctx, r, cursor)
		if err != nil {
			return nil, "", err
		}
		if record.Kind != "OpenThread" || record.Resource.ThreadID == "" {
			return nil, "", contract.NewError("Invalid", "invalid thread page token")
		}
	}
	records, err := g.store.ListThreads(ctx, r, cursor, limit+1)
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(records) > limit {
		records = records[:limit]
		next = records[len(records)-1].ID
	}
	return records, next, nil
}

// ReadThread retains the thread's original binding, including during updates.
func (g *Gateway) ReadThread(ctx context.Context, r Resource, before string, limit uint32) (*v1.ReadThreadResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	if err := g.authorize(ctx, r, "ReadThread", true); err != nil {
		return nil, err
	}
	if limit > 100 {
		return nil, contract.NewError("Invalid", "page size must not exceed 100")
	}
	record, err := g.store.Thread(ctx, r)
	if err != nil {
		return nil, err
	}
	a, err := g.agents.Resolve(ctx, record.Target)
	if err != nil {
		return nil, err
	}
	return a.ReadThread(ctx, &v1.ReadThreadRequest{Binding: record.Target, ThreadId: r.ThreadID, BeforeTurnId: before, PageSize: limit})
}
