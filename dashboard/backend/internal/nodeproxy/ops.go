// Operations/config REST relay - tranche 4 of the rebranding/dashboard/
// client-native plan. Same relay principle as nodeproxy.go's own
// /api/info handler: every handler here dials the real node with its
// stored service
// credential (never the browser's client certificate - see this
// package's own doc comment) and translates one HAProxyService RPC into
// one plain REST/JSON request, for the per-node page (static/index.html)
// to call.
package nodeproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

func registerOpsRoutes(mux *http.ServeMux, node *store.Node) {
	mux.HandleFunc("/api/haproxy/info", func(w http.ResponseWriter, r *http.Request) {
		withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			return c.ShowInfo(ctx, &emptypb.Empty{})
		})
	})

	mux.HandleFunc("/api/haproxy/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
				resp, err := c.GetConfig(ctx, &emptypb.Empty{})
				if err != nil {
					return nil, err
				}
				return struct {
					Config string `json:"config"`
					SHA256 string `json:"sha256"`
				}{Config: string(resp.GetConfig()), SHA256: resp.GetSha256()}, nil
			})
		case http.MethodPost:
			handleApplyConfig(w, r, node)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/haproxy/backends", func(w http.ResponseWriter, r *http.Request) {
		withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			return c.BackendList(ctx, &emptypb.Empty{})
		})
	})

	mux.HandleFunc("/api/haproxy/backends/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Backend string `json:"backend"`
			Server  string `json:"server"`
			State   string `json:"state"` // "ready", "drain", "maint"
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		state, ok := serverStateFromString(req.State)
		if !ok {
			http.Error(w, fmt.Sprintf("unknown state %q (want ready, drain, or maint)", req.State), http.StatusBadRequest)
			return
		}
		withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			_, err := c.ServerSetState(ctx, &janusv1alpha1.ServerSetStateRequest{Backend: req.Backend, Server: req.Server, State: state})
			return struct{}{}, err
		})
	})

	mux.HandleFunc("/api/haproxy/maps", func(w http.ResponseWriter, r *http.Request) {
		withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			return c.MapList(ctx, &emptypb.Empty{})
		})
	})

	mux.HandleFunc("/api/haproxy/maps/", func(w http.ResponseWriter, r *http.Request) {
		mapName := strings.TrimPrefix(r.URL.Path, "/api/haproxy/maps/")
		if mapName == "" {
			http.Error(w, "missing map name", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodGet:
			withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
				return c.MapGet(ctx, &janusv1alpha1.MapGetRequest{Map: mapName})
			})
		case http.MethodPost:
			var req struct {
				Key    string `json:"key"`
				Value  string `json:"value"`
				Delete bool   `json:"delete"`
			}
			if !decodeJSON(w, r, &req) {
				return
			}
			withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
				_, err := c.MapUpdate(ctx, &janusv1alpha1.MapUpdateRequest{Map: mapName, Key: req.Key, Value: req.Value, Delete: req.Delete})
				return struct{}{}, err
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/haproxy/acls/", func(w http.ResponseWriter, r *http.Request) {
		aclName := strings.TrimPrefix(r.URL.Path, "/api/haproxy/acls/")
		if aclName == "" || r.Method != http.MethodPost {
			http.Error(w, "usage: POST /api/haproxy/acls/{acl}", http.StatusBadRequest)
			return
		}
		var req struct {
			Value  string `json:"value"`
			Delete bool   `json:"delete"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			_, err := c.ACLUpdate(ctx, &janusv1alpha1.ACLUpdateRequest{Acl: aclName, Value: req.Value, Delete: req.Delete})
			return struct{}{}, err
		})
	})

	mux.HandleFunc("/api/haproxy/certs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
				return c.CertificateList(ctx, &emptypb.Empty{})
			})
		case http.MethodPost:
			var req struct {
				Name      string   `json:"name"`
				PEMBundle string   `json:"pem_bundle"`
				CrtList   string   `json:"crt_list"`
				SNI       []string `json:"sni"`
			}
			if !decodeJSON(w, r, &req) {
				return
			}
			withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
				_, err := c.CertificateUpload(ctx, &janusv1alpha1.CertificateUploadRequest{
					Name: req.Name, PemBundle: []byte(req.PEMBundle), CrtList: req.CrtList, Sni: req.SNI,
				})
				return struct{}{}, err
			})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/haproxy/certs/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/haproxy/certs/")
		if name == "" || r.Method != http.MethodDelete {
			http.Error(w, "usage: DELETE /api/haproxy/certs/{name}[?crt_list=...]", http.StatusBadRequest)
			return
		}
		withHAProxyClient(w, r, node, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			_, err := c.CertificateDelete(ctx, &janusv1alpha1.CertificateDeleteRequest{Name: name, CrtList: r.URL.Query().Get("crt_list")})
			return struct{}{}, err
		})
	})
}

// handleApplyConfig consumes ApplyConfig's whole progress stream
// server-side and returns just the final message as plain JSON - a
// browser polling one REST endpoint doesn't need the intermediate
// "validating"/"reloading" stages, only whether it was accepted and
// why not if it wasn't.
func handleApplyConfig(w http.ResponseWriter, r *http.Request, node *store.Node) {
	var req struct {
		Config string `json:"config"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	stream, err := janusv1alpha1.NewHAProxyServiceClient(conn).ApplyConfig(ctx, &janusv1alpha1.ApplyConfigRequest{Config: []byte(req.Config)})
	if err != nil {
		http.Error(w, fmt.Sprintf("ApplyConfig: %v", err), http.StatusBadGateway)
		return
	}

	var last *janusv1alpha1.ApplyConfigResponse
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("ApplyConfig stream: %v", err), http.StatusBadGateway)
			return
		}
		last = msg
	}
	if last == nil {
		http.Error(w, "ApplyConfig returned no progress messages", http.StatusBadGateway)
		return
	}

	writeJSONBody(w, http.StatusOK, struct {
		Stage    string `json:"stage"`
		Message  string `json:"message"`
		Accepted bool   `json:"accepted"`
	}{Stage: last.GetStage(), Message: last.GetMessage(), Accepted: last.GetAccepted()})
}

func serverStateFromString(s string) (janusv1alpha1.ServerSetStateRequest_State, bool) {
	switch s {
	case "ready":
		return janusv1alpha1.ServerSetStateRequest_STATE_READY, true
	case "drain":
		return janusv1alpha1.ServerSetStateRequest_STATE_DRAIN, true
	case "maint":
		return janusv1alpha1.ServerSetStateRequest_STATE_MAINT, true
	default:
		return 0, false
	}
}

// withHAProxyClient dials node, runs call, and writes the result (or
// error) as JSON - the shared plumbing every handler above uses so the
// dial/error-handling logic isn't repeated a dozen times.
func withHAProxyClient(w http.ResponseWriter, r *http.Request, node *store.Node, call func(context.Context, janusv1alpha1.HAProxyServiceClient) (any, error)) {
	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	resp, err := call(ctx, janusv1alpha1.NewHAProxyServiceClient(conn))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSONBody(w, http.StatusOK, resp)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
