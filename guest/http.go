//go:build linux

package guest

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/jingyugao/a8s/agentc/contract/transport"
	"github.com/jingyugao/a8s/control/controllers"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Handler exposes a single slot. Authorization is application-owned; workspace
// headers fence stale requests after that slot is assigned to another workspace.
func Handler(manager *Manager, idle *controllers.IdleController, token string) (http.Handler, error) {
	if len(token) < 16 {
		return nil, errors.New("guest API token is required")
	}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]bool{"healthy": true}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready, _ := manager.Status()
		if !ready {
			write(w, 503, map[string]bool{"warm": false})
			return
		}
		write(w, 200, map[string]bool{"warm": true})
	})
	decode := func(w http.ResponseWriter, r *http.Request, value any) bool {
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		if err := d.Decode(value); err != nil {
			write(w, 400, map[string]string{"error": "invalid request"})
			return false
		}
		if d.Decode(new(any)) != io.EOF {
			write(w, 400, map[string]string{"error": "one JSON object is required"})
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		ready, state := manager.Status()
		write(w, 200, map[string]any{"warm": ready, "slot": state})
	})
	mux.HandleFunc("POST /v1/start", func(w http.ResponseWriter, r *http.Request) {
		var req StartRequest
		if !decode(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		value, err := manager.Start(ctx, req)
		if err != nil {
			write(w, 409, map[string]any{"error": err.Error(), "slot": value})
			return
		}
		write(w, 200, value)
	})
	mux.HandleFunc("POST /v1/stop", func(w http.ResponseWriter, r *http.Request) {
		var req StopRequest
		if !decode(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		value, err := manager.Stop(ctx, req)
		if err != nil {
			write(w, 409, map[string]any{"error": err.Error(), "slot": value})
			return
		}
		write(w, 200, value)
	})
	_, managed := transport.NewHandler(&bridge{manager: manager, idle: idle}, 35*time.Second)
	methods := map[string]bool{"OpenThread": true, "ReadThread": true, "SubmitInput": true, "GetInputResult": true, "Cancel": true, "RespondInteraction": true, "Subscribe": true}
	mux.HandleFunc("/a8s.agentc.v1.AgentcService/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("A8s-Workspace") == "" {
			managed.ServeHTTP(w, r)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/a8s.agentc.v1.AgentcService/")
		if !methods[method] || r.Method != http.MethodPost {
			write(w, 404, map[string]string{"error": "execution method not found"})
			return
		}
		generation, _ := strconv.ParseUint(r.Header.Get("A8s-Generation"), 10, 64)
		_, current := manager.Status()
		if current.Request.WorkspaceID != r.Header.Get("A8s-Workspace") || current.Request.Generation != generation {
			write(w, 409, map[string]string{"error": "stale workspace generation"})
			return
		}
		managed.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			mux.ServeHTTP(w, r)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			write(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}
