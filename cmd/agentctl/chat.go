package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	agent "github.com/jingyugao/a8s/agentc/contract/gen/a8s/agentc/v1"
	v1 "github.com/jingyugao/a8s/execution/gen/a8s/execution/v1"
)

type chat struct {
	connection     *connection
	console        *console
	thread, before string
	pending        *request
	wait           time.Duration
}

func (c *chat) out() io.Writer { return c.console }

func (c *chat) loop(ctx context.Context) error {
	for ctx.Err() == nil {
		line, err := c.console.read("you > ")
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(c.out())
			return nil
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			command, arg, _ := strings.Cut(line, " ")
			arg = strings.TrimSpace(arg)
			switch command {
			case "/quit", "/exit":
				return nil
			case "/help":
				fmt.Fprint(c.out(), usage)
			case "/new":
				c.thread, c.before, c.pending = "", "", nil
				fmt.Fprintln(c.out(), "New conversation. Send a message to create its Thread.")
			case "/resume":
				err = c.resume(ctx, arg)
			case "/threads":
				_, err = c.list(ctx, arg)
			case "/history":
				err = c.history(ctx, "")
			case "/older":
				if c.before == "" {
					fmt.Fprintln(c.out(), "No older history.")
				} else {
					err = c.history(ctx, c.before)
				}
			case "/retry":
				if c.pending == nil {
					fmt.Fprintln(c.out(), "No pending request.")
					break
				}
				if err = c.lookup(ctx, c.pending); err == nil {
					if c.pending.Method == "OpenThread" {
						if c.pending.ThreadID == "" {
							err = errors.New("thread creation remains unconfirmed")
						} else {
							c.thread, c.pending = c.pending.ThreadID, nil
						}
					} else {
						err = c.watch(ctx, c.pending)
					}
				}
			case "/cancel":
				err = c.cancel(ctx)
			default:
				err = errors.New("unknown command; use /help, or start ordinary text without /")
			}
		} else {
			err = c.send(ctx, line)
		}
		if err != nil {
			fmt.Fprintln(c.out(), "\nagentctl:", safeText(err.Error()))
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return ctx.Err()
}

func (c *chat) list(ctx context.Context, cursor string) (*v1.ListThreadsResponse, error) {
	response, err := c.connection.client.Execution.ListThreads(ctx, connect.NewRequest(&v1.ListThreadsRequest{Resource: c.resource(""), PageSize: 20, PageToken: cursor}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnimplemented {
			return nil, errors.New("this API server needs the ListThreads update; an explicit Thread ID can still be resumed")
		}
		return nil, err
	}
	for i, thread := range response.Msg.Threads {
		fmt.Fprintf(c.out(), "%2d  %s  %s\n", i+1, safeText(thread.ThreadId), safeText(thread.CreatedAt))
	}
	if len(response.Msg.Threads) == 0 {
		fmt.Fprintln(c.out(), "No conversations yet.")
	}
	if response.Msg.NextPageToken != "" {
		fmt.Fprintln(c.out(), "More: /threads", response.Msg.NextPageToken)
	}
	return response.Msg, nil
}

func (c *chat) resume(ctx context.Context, id string) error {
	if id == "" {
		var cursor string
		for {
			page, err := c.list(ctx, cursor)
			if err != nil {
				return err
			}
			if len(page.Threads) == 0 {
				return nil
			}
			choice, err := c.console.read("Resume number or Thread ID (n: next page, Enter: back) > ")
			if err != nil {
				return err
			}
			choice = strings.TrimSpace(choice)
			if choice == "" {
				return nil
			}
			if choice == "n" {
				if page.NextPageToken == "" {
					fmt.Fprintln(c.out(), "No more conversations.")
					continue
				}
				cursor = page.NextPageToken
				continue
			}
			if index, err := strconv.Atoi(choice); err == nil {
				if index < 1 || index > len(page.Threads) {
					fmt.Fprintln(c.out(), "Choose a listed number.")
					continue
				}
				id = page.Threads[index-1].ThreadId
			} else {
				id = choice
			}
			break
		}
	}
	previous := c.thread
	previousBefore := c.before
	c.thread, c.before = id, ""
	if err := c.history(ctx, ""); err != nil {
		if connect.CodeOf(err) != connect.CodeUnimplemented {
			c.thread = previous
			c.before = previousBefore
			return err
		}
		fmt.Fprintln(c.out(), "This runtime has no history-read API yet; the native context resumes on input.")
	}
	c.pending = nil
	fmt.Fprintln(c.out(), "Resumed Thread:", safeText(id))
	return nil
}

func (c *chat) history(ctx context.Context, before string) error {
	if c.thread == "" {
		fmt.Fprintln(c.out(), "New conversation; no history yet.")
		return nil
	}
	response, err := c.connection.client.Execution.ReadThread(ctx, connect.NewRequest(&v1.ReadThreadRequest{Resource: c.resource(c.thread), BeforeTurnId: before, PageSize: 20}))
	if err != nil {
		return err
	}
	for _, message := range response.Msg.Messages {
		fmt.Fprintf(c.out(), "\n%s > %s\n", safeText(message.Role), safeText(message.Text))
	}
	c.before = response.Msg.NextBeforeTurnId
	if c.before != "" {
		fmt.Fprintln(c.out(), "Use /older for earlier messages.")
	}
	return nil
}

func terminal(state string) bool {
	return state == "Succeeded" || state == "Failed" || state == "Cancelled" || state == "Rejected"
}

func (c *chat) send(ctx context.Context, text string) error {
	if c.pending != nil && !terminal(c.pending.State) {
		return errors.New("previous request remains pending; use /retry to query it, /cancel, or /new for an independent conversation")
	}
	if c.thread == "" {
		if err := c.open(ctx); err != nil {
			return err
		}
	}
	r, err := c.intent("SubmitInput")
	if err != nil {
		return err
	}
	c.pending = r
	response, callErr := c.connection.client.Execution.SubmitInput(ctx, connect.NewRequest(&v1.SubmitInputRequest{Resource: c.resource(c.thread), RequestKey: r.Key, Text: text}))
	if callErr != nil {
		if err := c.lookup(ctx, r); err != nil {
			return c.unresolved(r, callErr)
		}
	} else if err := c.record(r, response.Msg.Record); err != nil {
		return c.unresolved(r, err)
	}
	if r.State == "Rejected" {
		return errors.New("input was rejected before execution; inspect the Agent state")
	}
	return c.watch(ctx, r)
}

func (c *chat) watch(ctx context.Context, r *request) error {
	ctx, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()
	events := make(chan *agent.SubscribeResponse, 64)
	go func() {
		defer close(events)
		stream, err := c.connection.client.Execution.Subscribe(ctx, connect.NewRequest(&v1.SubscribeRequest{Resource: c.resource(r.ThreadID), InputRequestId: r.RequestID}))
		if err != nil {
			return
		}
		defer stream.Close()
		for stream.Receive() {
			select {
			case events <- stream.Msg().Event:
			case <-ctx.Done():
				return
			}
		}
	}()
	fmt.Fprintln(c.out(), "Waiting…")
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var rendered string
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if rendered != "" {
				fmt.Fprintln(c.out())
			}
			if lastErr != nil {
				return c.unresolved(r, fmt.Errorf("%w (last receipt query: %v)", ctx.Err(), lastErr))
			}
			return c.unresolved(r, ctx.Err())
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event == nil || r.TurnID == "" || event.TurnId != r.TurnID {
				continue
			}
			if event.Type == "OutputDelta" {
				if rendered == "" {
					fmt.Fprint(c.out(), "assistant > ")
				}
				text := safeText(event.Text)
				fmt.Fprint(c.out(), text)
				rendered += text
			}
			if event.Type == "InteractionRequired" {
				if rendered != "" {
					fmt.Fprintln(c.out())
				}
				if err := c.approve(ctx, r, event); err != nil {
					return c.unresolved(r, err)
				}
			}
		case <-ticker.C:
			response, err := c.connection.client.Execution.GetInputResult(ctx, connect.NewRequest(&v1.GetInputResultRequest{Resource: c.resource(r.ThreadID), RequestId: r.RequestID}))
			if err != nil {
				lastErr = err
				switch connect.CodeOf(err) {
				case connect.CodeUnauthenticated, connect.CodePermissionDenied, connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeUnimplemented:
					return c.unresolved(r, err)
				}
				continue
			}
			result := response.Msg.Result
			lastErr = nil
			if result == nil || result.RequestId != r.RequestID || result.ThreadId != r.ThreadID {
				return c.unresolved(r, errors.New("invalid native receipt identity"))
			}
			r.State = result.State
			if result.TurnId != "" {
				r.TurnID = result.TurnId
			}
			if !terminal(r.State) {
				continue
			}
			output := safeText(result.OutputText)
			if rendered == "" {
				fmt.Fprintln(c.out(), "assistant >", output)
			} else if strings.HasPrefix(output, rendered) {
				fmt.Fprintln(c.out(), strings.TrimPrefix(output, rendered))
			} else {
				fmt.Fprintln(c.out(), "\nComplete reply >", output)
			}
			if err := c.save(r); err != nil {
				return c.unresolved(r, err)
			}
			if r.State != "Succeeded" {
				fmt.Fprintln(c.out(), "Input state:", r.State)
			}
			return nil
		}
	}
}

func (c *chat) approve(ctx context.Context, input *request, event *agent.SubscribeResponse) error {
	choice, err := c.console.read("Approve " + safeText(event.InteractionKind) + "? [y/N] > ")
	if err != nil {
		return err
	}
	decision := "decline"
	if strings.EqualFold(strings.TrimSpace(choice), "y") {
		decision = "accept"
	}
	r, err := c.intent("RespondInteraction")
	if err != nil {
		return err
	}
	response, err := c.connection.client.Execution.RespondInteraction(ctx, connect.NewRequest(&v1.RespondInteractionRequest{Resource: c.resource(input.ThreadID), RequestKey: r.Key, InputRequestId: input.RequestID, TurnId: input.TurnID, InteractionId: event.InteractionId, Decision: decision}))
	if err != nil {
		return c.unresolved(r, err)
	}
	return c.record(r, response.Msg.Record)
}

func (c *chat) cancel(ctx context.Context) error {
	input := c.pending
	if input == nil || input.Method != "SubmitInput" || terminal(input.State) {
		return errors.New("no pending input to cancel")
	}
	if input.TurnID == "" {
		if err := c.lookup(ctx, input); err != nil {
			return c.unresolved(input, err)
		}
		if input.TurnID == "" {
			return errors.New("native turn is still unconfirmed; use /retry")
		}
	}
	r, err := c.intent("Cancel")
	if err != nil {
		return err
	}
	response, err := c.connection.client.Execution.Cancel(ctx, connect.NewRequest(&v1.CancelRequest{Resource: c.resource(input.ThreadID), RequestKey: r.Key, InputRequestId: input.RequestID, TurnId: input.TurnID}))
	if err != nil {
		return c.unresolved(r, err)
	}
	if err := c.record(r, response.Msg.Record); err != nil {
		return err
	}
	return c.watch(ctx, input)
}
