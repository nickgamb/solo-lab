package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

// The gateway sends the access token it verified as metadata (the policy's
// metadataContext), never trusting a header the caller could set.
const (
	metaNamespace = "xaa"
	metaToken     = "subject_token"
	idTokenHeader = "x-id-token"
)

// processor is agentgateway's external processor for the XAA route: on the
// request headers it sets x-id-token (replacing any the caller sent) or
// answers the request itself (403 refused, 503 unavailable). Only request
// headers are sent to it (the policy's processingOptions).
type processor struct {
	extproc.UnimplementedExternalProcessorServer
	x *exchanger
}

func (p *processor) Process(stream extproc.ExternalProcessor_ProcessServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var resp *extproc.ProcessingResponse
		switch req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			resp = p.requestHeaders(stream.Context(), req)
		case *extproc.ProcessingRequest_RequestBody:
			resp = &extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_RequestBody{RequestBody: &extproc.BodyResponse{}}}
		case *extproc.ProcessingRequest_ResponseHeaders:
			resp = &extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_ResponseHeaders{ResponseHeaders: &extproc.HeadersResponse{}}}
		case *extproc.ProcessingRequest_ResponseBody:
			resp = &extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_ResponseBody{ResponseBody: &extproc.BodyResponse{}}}
		case *extproc.ProcessingRequest_RequestTrailers:
			resp = &extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_RequestTrailers{RequestTrailers: &extproc.TrailersResponse{}}}
		case *extproc.ProcessingRequest_ResponseTrailers:
			resp = &extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_ResponseTrailers{ResponseTrailers: &extproc.TrailersResponse{}}}
		default:
			continue
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (p *processor) requestHeaders(ctx context.Context, req *extproc.ProcessingRequest) *extproc.ProcessingResponse {
	tok := ""
	if md := req.GetMetadataContext().GetFilterMetadata()[metaNamespace]; md != nil {
		tok = md.GetFields()[metaToken].GetStringValue()
	}
	if tok == "" {
		slog.Warn("refused", "err", "no verified subject token in the metadata")
		return immediate(typev3.StatusCode_Forbidden, "refused")
	}
	id, source, err := p.x.idToken(ctx, tok)
	switch {
	case errors.Is(err, errRefused):
		slog.Info("refused", "idp", source, "err", err)
		return immediate(typev3.StatusCode_Forbidden, "refused")
	case err != nil:
		slog.Warn("failed", "idp", source, "err", err)
		return immediate(typev3.StatusCode_ServiceUnavailable, "exchange failed")
	}
	// the ID token's claims, never the token: the trail for the ID-JAG request
	c := payload(id)
	slog.Info("id token", "iss", c.Iss, "sub", c.Sub, "aud", c.Aud, "jti", c.Jti,
		"exp", time.Unix(c.Exp, 0).UTC().Format(time.RFC3339), "request_id", header(req, "x-request-id"))
	return &extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_RequestHeaders{
		RequestHeaders: &extproc.HeadersResponse{Response: &extproc.CommonResponse{
			HeaderMutation: &extproc.HeaderMutation{SetHeaders: []*core.HeaderValueOption{{
				Header:       &core.HeaderValue{Key: idTokenHeader, RawValue: []byte(id)},
				AppendAction: core.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			}}},
		}},
	}}
}

func immediate(code typev3.StatusCode, body string) *extproc.ProcessingResponse {
	return &extproc.ProcessingResponse{Response: &extproc.ProcessingResponse_ImmediateResponse{
		ImmediateResponse: &extproc.ImmediateResponse{Status: &typev3.HttpStatus{Code: code}, Body: []byte(body)},
	}}
}

func header(req *extproc.ProcessingRequest, name string) string {
	for _, h := range req.GetRequestHeaders().GetHeaders().GetHeaders() {
		if h.GetKey() == name {
			if v := h.GetValue(); v != "" {
				return v
			}
			return string(h.GetRawValue())
		}
	}
	return ""
}
