package api

import (
	"context"
	"strings"

	"google.golang.org/protobuf/types/known/emptypb"

	haproxyosv1alpha1 "github.com/swenske/HAProxyOS/gen/haproxyos/v1alpha1"
	"github.com/swenske/HAProxyOS/internal/haproxy"
)

// HAProxy implements haproxyosv1alpha1.HAProxyServiceServer, backed by a
// single supervised haproxy process (internal/haproxy.Manager). Runtime
// map/ACL/certificate management (MapList/MapGet/MapUpdate/ACLUpdate/
// Certificate*) isn't implemented yet - falls through to
// UnimplementedHAProxyServiceServer.
type HAProxy struct {
	haproxyosv1alpha1.UnimplementedHAProxyServiceServer

	Manager *haproxy.Manager
}

func (h *HAProxy) GetConfig(_ context.Context, _ *emptypb.Empty) (*haproxyosv1alpha1.GetConfigResponse, error) {
	cfg, err := readFile(h.Manager.ConfigPath)
	if err != nil {
		return nil, err
	}
	return &haproxyosv1alpha1.GetConfigResponse{Config: cfg, Sha256: sha256Hex(cfg)}, nil
}

func (h *HAProxy) ApplyConfig(req *haproxyosv1alpha1.ApplyConfigRequest, stream haproxyosv1alpha1.HAProxyService_ApplyConfigServer) error {
	if err := stream.Send(&haproxyosv1alpha1.ApplyConfigResponse{Stage: "validating"}); err != nil {
		return err
	}

	errs, err := h.Manager.Apply(req.GetConfig())
	if err != nil {
		return stream.Send(&haproxyosv1alpha1.ApplyConfigResponse{
			Stage:    "rejected",
			Message:  strings.Join(errs, "; "),
			Accepted: false,
		})
	}

	if err := stream.Send(&haproxyosv1alpha1.ApplyConfigResponse{Stage: "reloading"}); err != nil {
		return err
	}
	return stream.Send(&haproxyosv1alpha1.ApplyConfigResponse{Stage: "done", Accepted: true})
}

func (h *HAProxy) ValidateConfig(_ context.Context, req *haproxyosv1alpha1.ValidateConfigRequest) (*haproxyosv1alpha1.ValidateConfigResponse, error) {
	ok, errs := h.Manager.Validate(req.GetConfig())
	return &haproxyosv1alpha1.ValidateConfigResponse{Valid: ok, Errors: errs}, nil
}

func (h *HAProxy) Reload(_ context.Context, _ *emptypb.Empty) (*haproxyosv1alpha1.ReloadResponse, error) {
	if err := h.Manager.Reload(); err != nil {
		return &haproxyosv1alpha1.ReloadResponse{Success: false, Message: err.Error()}, nil
	}
	return &haproxyosv1alpha1.ReloadResponse{Success: true}, nil
}

func (h *HAProxy) Stats(_ context.Context, _ *emptypb.Empty) (*haproxyosv1alpha1.HAProxyStatsResponse, error) {
	raw, err := h.Manager.ShowStat()
	if err != nil {
		return nil, err
	}
	return &haproxyosv1alpha1.HAProxyStatsResponse{RawCsv: raw}, nil
}

func (h *HAProxy) ShowInfo(_ context.Context, _ *emptypb.Empty) (*haproxyosv1alpha1.ShowInfoResponse, error) {
	info, err := h.Manager.ShowInfo()
	if err != nil {
		return nil, err
	}
	return &haproxyosv1alpha1.ShowInfoResponse{
		Version:            info.Version,
		UptimeSeconds:      info.UptimeSeconds,
		CurrentConnections: info.CurrentConnections,
		MaxConnections:     info.MaxConnections,
	}, nil
}

func (h *HAProxy) ServerSetState(_ context.Context, req *haproxyosv1alpha1.ServerSetStateRequest) (*emptypb.Empty, error) {
	state := map[haproxyosv1alpha1.ServerSetStateRequest_State]string{
		haproxyosv1alpha1.ServerSetStateRequest_STATE_READY: "ready",
		haproxyosv1alpha1.ServerSetStateRequest_STATE_DRAIN: "drain",
		haproxyosv1alpha1.ServerSetStateRequest_STATE_MAINT: "maint",
	}[req.GetState()]

	if err := h.Manager.SetServerState(req.GetBackend(), req.GetServer(), state); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
