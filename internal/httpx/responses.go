package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/inference"
	"enclava-go/internal/source"
	"enclava-go/internal/store/postgres"
)

// responses.go implements the selected public Responses lifecycle:
// POST /v1/responses (unary + stream), POST /v1/responses/input_tokens,
// POST /v1/responses/compact, GET/DELETE /v1/responses/{id},
// POST /v1/responses/{id}/cancel and GET /v1/responses/{id}/input_items.
//
// Priced operations (create, compact, input_tokens) dispatch through the
// shared executor; lifecycle follow-ups (retrieve, cancel, delete,
// input_items, stream-resume) dispatch through the executor's unpriced
// management path so they stay usable when the inference budget is
// exhausted. Every retained reference is a server-minted public ID bound to
// tenant, owner and endpoint origin in response_bindings — a raw upstream
// ID supplied by a client is never authority and never resolves.
type responsesRequestMeta struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// bindingInputSnapshot is the durable record of which referenced inputs a
// binding was created under; chained follow-ups re-authorize and merge it.
type bindingInputSnapshot struct {
	Inputs        []snapshotObjectRef  `json:"inputs,omitempty"`
	Artifacts     []snapshotArtRef     `json:"artifacts,omitempty"`
	Conversations []string             `json:"conversations,omitempty"`
	Restriction   *snapshotRestriction `json:"restriction,omitempty"`
}

type snapshotObjectRef struct {
	ObjectID    string `json:"object_id"`
	ObservedSeq int64  `json:"observed_seq,omitempty"`
}

type snapshotArtRef struct {
	ID       string `json:"id"`
	Kind     string `json:"kind,omitempty"`
	Version  int    `json:"version,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	ByteSize int64  `json:"byte_size,omitempty"`
}

type snapshotRestriction struct {
	RequiredAssurance  string   `json:"required_assurance,omitempty"`
	DeniedCapabilities []string `json:"denied_capabilities,omitempty"`
	PolicyLabels       []string `json:"policy_labels,omitempty"`
}

func snapshotFromRequest(req inference.Request) bindingInputSnapshot {
	var snap bindingInputSnapshot
	for _, in := range req.Inputs {
		snap.Inputs = append(snap.Inputs, snapshotObjectRef{ObjectID: in.ObjectID, ObservedSeq: in.ObservedSeq})
	}
	for _, ar := range req.Artifacts {
		snap.Artifacts = append(snap.Artifacts, snapshotArtRef{
			ID: ar.ID, Kind: ar.Kind, Version: ar.Version, SHA256: ar.SHA256, ByteSize: ar.ByteSize,
		})
	}
	snap.Conversations = append(snap.Conversations, req.Conversations...)
	if r := req.Restriction; r != nil {
		snap.Restriction = &snapshotRestriction{
			RequiredAssurance:  r.RequiredAssurance,
			DeniedCapabilities: append([]string(nil), r.DeniedCapabilities...),
			PolicyLabels:       append([]string(nil), r.PolicyLabels...),
		}
	}
	return snap
}

// mergeInputSnapshot re-reads the parent's durable input snapshot into the
// follow-up request: the executor re-authorizes every referenced input under
// current source authorization, and inherited restrictions keep narrowing.
func mergeInputSnapshot(raw json.RawMessage, req *inference.Request) {
	var snap bindingInputSnapshot
	if len(raw) == 0 || json.Unmarshal(raw, &snap) != nil {
		return
	}
	for _, in := range snap.Inputs {
		req.Inputs = append(req.Inputs, source.ObjectRef{ObjectID: in.ObjectID, ObservedSeq: in.ObservedSeq})
	}
	for _, ar := range snap.Artifacts {
		req.Artifacts = append(req.Artifacts, artifacts.Ref{
			ID: ar.ID, Kind: ar.Kind, Version: ar.Version, SHA256: ar.SHA256, ByteSize: ar.ByteSize,
		})
	}
	req.Conversations = append(req.Conversations, snap.Conversations...)
	if snap.Restriction != nil {
		req.Restriction = &source.SourceRestriction{
			RequiredAssurance:  snap.Restriction.RequiredAssurance,
			DeniedCapabilities: snap.Restriction.DeniedCapabilities,
			PolicyLabels:       snap.Restriction.PolicyLabels,
		}
	}
}

func (a *App) handleResponses(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	if a.executor == nil {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "inference is not configured", "server_error", "inference_unavailable")
		return
	}
	if a.bindings == nil {
		WriteOpenAIError(w, http.StatusServiceUnavailable, "response bindings are not configured", "server_error", "bindings_unavailable")
		return
	}
	endpoint := strings.TrimPrefix(r.URL.Path, "/v1")
	switch {
	case endpoint == "/responses" || endpoint == "/responses/input_tokens" || endpoint == "/responses/compact":
		if r.Method != http.MethodPost {
			WriteMethodNotAllowed(w)
			return
		}
		a.responsesPriced(w, r, principal, endpoint)
	case strings.HasPrefix(endpoint, "/responses/"):
		a.responseFollowUp(w, r, principal, endpoint)
	default:
		WriteOpenAIError(w, http.StatusNotFound, "unknown responses route", "invalid_request_error", "not_found")
	}
}

// responsesPriced runs one priced Responses operation — create, input_tokens
// or compact — through the shared executor with all nested references
// resolved to bound upstream IDs first.
func (a *App) responsesPriced(w http.ResponseWriter, r *http.Request, principal auth.Principal, upstreamPath string) {
	requestBytes, ok := a.readInferenceBody(w, r, 16*1024*1024)
	if !ok {
		return
	}
	var meta responsesRequestMeta
	if err := json.Unmarshal(requestBytes, &meta); err != nil {
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
		return
	}
	if meta.Stream && upstreamPath != "/responses" {
		WriteOpenAIError(w, http.StatusBadRequest, "stream is only supported on response creation", "invalid_request_error", "unsupported_option")
		return
	}
	// Retention is server-decided: "store":false would mint an upstream
	// resource no binding can ever authorize later, so it rejects honestly
	// up front. Truthy retention/background flags are rejected deeper by
	// the executor's retained-flag gate.
	var storeProbe struct {
		Store *bool `json:"store"`
	}
	_ = json.Unmarshal(requestBytes, &storeProbe)
	if storeProbe.Store != nil && !*storeProbe.Store {
		WriteOpenAIError(w, http.StatusBadRequest, "store is not configurable; responses are retained server-side", "invalid_request_error", "unsupported_request")
		return
	}

	req := inference.Request{
		Capability: inference.CapabilityResponses,
		Model:      meta.Model,
		Stream:     meta.Stream,
		Method:     http.MethodPost,
		Path:       upstreamPath,
	}
	if upstreamPath == "/responses/compact" {
		req.Capability = inference.CapabilityCompact
	}
	body, err := a.rewriteResponseRequest(r.Context(), principal, requestBytes, &req)
	if err != nil {
		a.writeResponseError(w, err)
		return
	}
	req.Payload = body
	if upstreamPath == "/responses/input_tokens" {
		req.Estimate = billing.Usage{InputUnits: tokenEstimate(requestBytes)}
	} else {
		req.Estimate = responsesEstimate(requestBytes)
	}

	res, err := a.executor.Execute(r.Context(), principal, req)
	if err != nil {
		a.writeInferenceError(w, err)
		return
	}
	defer func() { _ = res.Body.Close() }()
	rw := &responseRewriter{
		app:      a,
		p:        principal,
		endpoint: res.EndpointID,
		snapshot: snapshotFromRequest(req),
		item2pub: map[string]string{},
		resp2pub: map[string]string{},
	}
	if meta.Stream {
		a.proxyBoundResponsesStream(w, r, res.Body, rw)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16*1024*1024))
	if err != nil {
		WriteOpenAIError(w, http.StatusBadGateway, "upstream response unavailable", "server_error", "upstream_unavailable")
		return
	}
	out, err := rw.rewriteBoundObject(r.Context(), raw, "")
	if err != nil {
		WriteOpenAIError(w, http.StatusBadGateway, "could not persist response binding", "server_error", "binding_failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// responseFollowUp resolves a bound public ID and dispatches the lifecycle
// operation on its origin endpoint — never a caller-supplied upstream ID.
func (a *App) responseFollowUp(w http.ResponseWriter, r *http.Request, principal auth.Principal, endpoint string) {
	tail := strings.TrimPrefix(endpoint, "/responses/")
	parts := strings.Split(tail, "/")
	if len(parts) > 2 || parts[0] == "" {
		WriteOpenAIError(w, http.StatusNotFound, "unknown responses route", "invalid_request_error", "not_found")
		return
	}
	publicID := parts[0]
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	switch sub {
	case "cancel":
		if r.Method != http.MethodPost {
			WriteMethodNotAllowed(w)
			return
		}
	case "input_items":
		if r.Method != http.MethodGet {
			WriteMethodNotAllowed(w)
			return
		}
	case "":
		if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			WriteMethodNotAllowed(w)
			return
		}
	default:
		WriteOpenAIError(w, http.StatusNotFound, "unknown responses route", "invalid_request_error", "not_found")
		return
	}

	b, err := a.boundResponse(r.Context(), principal, publicID)
	if err != nil {
		a.writeResponseError(w, err)
		return
	}
	if b.Kind != postgres.ResponseBindingResponse {
		a.writeResponseError(w, postgres.ErrResponseBindingNotFound)
		return
	}

	query, err := a.responsesQueryFilter(r.Context(), principal, r.URL.Query(), sub)
	if err != nil {
		a.writeResponseError(w, err)
		return
	}
	upstreamPath := "/responses/" + b.UpstreamID
	if sub != "" {
		upstreamPath += "/" + sub
	}
	stream := r.Method == http.MethodGet && sub == "" && strings.EqualFold(r.URL.Query().Get("stream"), "true")

	mres, err := a.executor.Manage(r.Context(), principal, inference.ManageRequest{
		EndpointID: b.EndpointID,
		Method:     r.Method,
		Path:       upstreamPath,
		Query:      query,
		Stream:     stream,
	})
	if err != nil {
		a.writeInferenceError(w, err)
		return
	}
	defer func() { _ = mres.Body.Close() }()

	rw := &responseRewriter{
		app:      a,
		p:        principal,
		endpoint: b.EndpointID,
		item2pub: map[string]string{},
		resp2pub: map[string]string{b.UpstreamID: b.PublicID},
	}
	if stream {
		a.proxyBoundResponsesStream(w, r, mres.Body, rw)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(mres.Body, 16*1024*1024))
	if err != nil {
		WriteOpenAIError(w, http.StatusBadGateway, "upstream response unavailable", "server_error", "upstream_unavailable")
		return
	}
	out, err := rw.rewriteBoundObject(r.Context(), raw, b.PublicID)
	if err != nil {
		WriteOpenAIError(w, http.StatusBadGateway, "could not persist response binding", "server_error", "binding_failed")
		return
	}
	if r.Method == http.MethodDelete {
		// Tombstone only after the upstream delete succeeded: the binding is
		// gone locally exactly when the retained resource is gone upstream.
		if terr := a.bindings.MarkDeleted(r.Context(), principal.TenantID, b.PublicID); terr != nil {
			WriteOpenAIError(w, http.StatusBadGateway, "could not record deletion", "server_error", "binding_failed")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// boundResponse resolves a caller-supplied public response/item ID to its
// binding under the current authority: the row must exist, be active and
// belong to the actor (or its recorded delegator). Unknown, foreign,
// tombstoned and unminted references share one indistinguishable outcome.
func (a *App) boundResponse(ctx context.Context, p auth.Principal, publicID string) (postgres.ResponseBinding, error) {
	b, err := a.bindings.Resolve(ctx, p.TenantID, publicID)
	if err != nil {
		if errors.Is(err, postgres.ErrResponseBindingNotFound) {
			return postgres.ResponseBinding{}, err
		}
		return postgres.ResponseBinding{}, fmt.Errorf("resolve binding: %w", err)
	}
	if b.OwnerActorID != p.ActorID &&
		(p.DelegatedByActorID == "" || b.OwnerActorID != p.DelegatedByActorID) {
		return postgres.ResponseBinding{}, postgres.ErrResponseBindingNotFound
	}
	return b, nil
}

// rewriteResponseRequest decodes one priced request body and translates every
// bound reference it carries — previous_response_id and any id/item_id inside
// "input" — into the raw upstream IDs of owned bindings. Anything not owned
// denies before dispatch; the pinned endpoint records the shared origin so a
// chained follow-up can never route to a different endpoint.
func (a *App) rewriteResponseRequest(ctx context.Context, p auth.Principal, raw []byte, req *inference.Request) ([]byte, error) {
	root := map[string]any{}
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			return nil, errBadBody
		}
	}
	pinned := ""
	pin := func(endpointID string) error {
		if pinned != "" && pinned != endpointID {
			return postgres.ErrResponseBindingNotFound
		}
		pinned = endpointID
		return nil
	}
	resolve := func(v any) (string, error) {
		s, ok := v.(string)
		if !ok || s == "" {
			return "", postgres.ErrResponseBindingNotFound
		}
		b, err := a.boundResponse(ctx, p, s)
		if err != nil {
			return "", err
		}
		if err := pin(b.EndpointID); err != nil {
			return "", err
		}
		mergeInputSnapshot(b.InputSnapshot, req)
		return b.UpstreamID, nil
	}
	if prev, present := root["previous_response_id"]; present {
		up, err := resolve(prev)
		if err != nil {
			return nil, err
		}
		root["previous_response_id"] = up
	}
	if input, present := root["input"]; present {
		rewritten, err := rewriteInputRefs(input, resolve)
		if err != nil {
			return nil, err
		}
		root["input"] = rewritten
	}
	req.EndpointID = pinned
	out, err := json.Marshal(root)
	if err != nil {
		return nil, errBadBody
	}
	return out, nil
}

// rewriteInputRefs resolves id/item_id references nested anywhere inside the
// "input" payload — top-level items and their nested content — against the
// caller's bindings. An unresolvable reference denies the whole request
// rather than routing a foreign upstream ID.
func rewriteInputRefs(node any, resolve func(any) (string, error)) (any, error) {
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if (k == "id" || k == "item_id") && val != nil {
				up, err := resolve(val)
				if err != nil {
					return nil, err
				}
				v[k] = up
				continue
			}
			rw, err := rewriteInputRefs(val, resolve)
			if err != nil {
				return nil, err
			}
			v[k] = rw
		}
		return v, nil
	case []any:
		for i, item := range v {
			rw, err := rewriteInputRefs(item, resolve)
			if err != nil {
				return nil, err
			}
			v[i] = rw
		}
		return v, nil
	default:
		return node, nil
	}
}

// responsesQueryFilter whitelists follow-up query parameters: only bounded
// pagination knobs cross, and cursor values resolve through the caller's own
// bindings — a foreign or unbound cursor denies before any upstream list
// reveals it exists.
func (a *App) responsesQueryFilter(ctx context.Context, p auth.Principal, q url.Values, sub string) (string, error) {
	allowed := map[string]bool{}
	switch sub {
	case "":
		allowed = map[string]bool{"stream": true, "starting_after": true}
	case "input_items":
		allowed = map[string]bool{"limit": true, "order": true, "after": true, "before": true}
	}
	out := url.Values{}
	for key, vals := range q {
		if key == "stream" && sub == "" {
			continue // handled by the Stream flag on the manage request
		}
		if !allowed[key] || len(vals) != 1 {
			return "", errBadQuery
		}
		v := vals[0]
		switch key {
		case "after", "before":
			b, err := a.boundResponse(ctx, p, v)
			if err != nil {
				return "", err
			}
			v = b.UpstreamID
		case "starting_after":
			// Sequence number, not a resource id: pass through digits only.
			for _, r := range v {
				if r < '0' || r > '9' {
					return "", errBadQuery
				}
			}
			if v == "" {
				return "", errBadQuery
			}
		case "order":
			if v != "asc" && v != "desc" {
				return "", errBadQuery
			}
		}
		out.Set(key, v)
	}
	return out.Encode(), nil
}

// responseRewriter maps upstream IDs to durable public IDs in the outbound
// direction. Every mapping is persisted before it is exposed — a persistence
// failure aborts the response rather than leaking an unbound upstream
// reference.
type responseRewriter struct {
	app      *App
	p        auth.Principal
	endpoint string
	snapshot bindingInputSnapshot

	item2pub map[string]string // upstream item id → public id
	resp2pub map[string]string // upstream response id → public id
	lastResp string            // public id of the response governing this stream
}

// rewriteBoundObject parses one upstream JSON payload and replaces every
// retained reference it carries with the bound public ID. respParent is the
// public ID of the governing response — the collision domain items bind
// under.
func (rw *responseRewriter) rewriteBoundObject(ctx context.Context, raw []byte, respParent string) ([]byte, error) {
	var root any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		// Malformed upstream payloads pass through unchanged: they carry no
		// parseable IDs, and nothing a client replays resolves anyway.
		return raw, nil
	}
	if err := rw.rewriteNode(ctx, root, respParent); err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

// rewriteNode walks one decoded value. Response-typed objects bind "id" as a
// response and become the parent for contained items; every other string
// "id"/"item_id" binds as an item, "response_id" as a response.
// "previous_response_id" re-maps to the bound public parent where known and
// "conversation" is dropped — both name upstream-internal state the client
// cannot authorize.
func (rw *responseRewriter) rewriteNode(ctx context.Context, node any, respParent string) error {
	switch v := node.(type) {
	case map[string]any:
		if id, ok := v["id"].(string); ok && id != "" {
			isResp := isResponseObject(v)
			pub, err := rw.publicFor(ctx, id, isResp, respParent)
			if err != nil {
				return err
			}
			v["id"] = pub
			if isResp {
				respParent = pub
				rw.lastResp = pub
			}
		}
		for k, val := range v {
			switch k {
			case "id":
				continue
			case "conversation":
				delete(v, k)
				continue
			case "previous_response_id":
				if s, ok := val.(string); ok && s != "" {
					if pub, known := rw.resp2pub[s]; known {
						v[k] = pub
						continue
					}
					if b, err := rw.app.bindings.GetByUpstream(ctx, rw.p.TenantID, rw.endpoint, "", s); err == nil {
						v[k] = b.PublicID
						continue
					}
				}
				delete(v, k)
				continue
			case "first_id", "last_id", "item_id", "response_id":
				if s, ok := val.(string); ok && s != "" {
					pub, err := rw.publicFor(ctx, s, k == "response_id", respParent)
					if err != nil {
						return err
					}
					v[k] = pub
					continue
				}
			}
			if err := rw.rewriteNode(ctx, val, respParent); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, item := range v {
			if err := rw.rewriteNode(ctx, item, respParent); err != nil {
				return err
			}
		}
	}
	return nil
}

func isResponseObject(m map[string]any) bool {
	if o, _ := m["object"].(string); strings.HasPrefix(o, "response") {
		return true
	}
	if t, _ := m["type"].(string); strings.HasPrefix(t, "response.") {
		return true
	}
	return false
}

// publicFor maps one raw upstream ID to its public reference, persisting the
// binding on first sight. The durable row exists before the public ID is
// emitted anywhere.
func (rw *responseRewriter) publicFor(ctx context.Context, upstreamID string, response bool, respParent string) (string, error) {
	kind := postgres.ResponseBindingItem
	known := rw.item2pub
	if response {
		kind = postgres.ResponseBindingResponse
		respParent = ""
		known = rw.resp2pub
	} else if respParent == "" {
		respParent = rw.lastResp
	}
	if pub, ok := known[upstreamID]; ok {
		return pub, nil
	}
	if b, err := rw.app.bindings.GetByUpstream(ctx, rw.p.TenantID, rw.endpoint, respParent, upstreamID); err == nil {
		known[upstreamID] = b.PublicID
		return b.PublicID, nil
	} else if !errors.Is(err, postgres.ErrResponseBindingNotFound) {
		return "", err
	}
	snap, _ := json.Marshal(rw.snapshot)
	for range 4 {
		publicID := NewID("item")
		if response {
			publicID = NewID("resp")
		}
		b, err := rw.app.bindings.Bind(ctx, postgres.ResponseBinding{
			TenantID:       rw.p.TenantID,
			PublicID:       publicID,
			Kind:           kind,
			EndpointID:     rw.endpoint,
			OwnerActorID:   rw.p.ActorID,
			DelegatedBy:    rw.p.DelegatedByActorID,
			UpstreamID:     upstreamID,
			ParentPublicID: respParent,
			InputSnapshot:  snap,
		})
		if errors.Is(err, postgres.ErrPublicIDConflict) {
			continue
		}
		if err != nil {
			return "", err
		}
		known[upstreamID] = b.PublicID
		return b.PublicID, nil
	}
	return "", errors.New("could not mint response reference")
}

// proxyBoundResponsesStream relays the executor's SSE stream to the client,
// rewriting every event's retained references before it is exposed. Malformed
// events pass through unchanged; an oversized event terminates the relay
// instead of silently dropping data.
func (a *App) proxyBoundResponsesStream(w http.ResponseWriter, r *http.Request, body io.Reader, rw *responseRewriter) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, maxSSELineBytes), maxSSELineBytes)
	var event [][]byte
	eventBytes := 0
	flushEvent := func() bool {
		if len(event) == 0 {
			return true
		}
		var data []byte
		for _, line := range event {
			if d, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, bytes.TrimLeft(d, " ")...)
			}
		}
		out := data
		if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
			rewritten, err := rw.rewriteBoundObject(ctx, data, "")
			if err != nil {
				// A binding persistence failure mid-stream ends the relay —
				// the partial event must not expose unbound upstream IDs.
				event, eventBytes = nil, 0
				return false
			}
			out = rewritten
		}
		for _, line := range event {
			if bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			if _, err := w.Write(line); err != nil {
				return false
			}
			if _, err := w.Write([]byte{'\n'}); err != nil {
				return false
			}
		}
		if len(out) > 0 {
			if _, err := io.WriteString(w, "data: "); err != nil {
				return false
			}
			if _, err := w.Write(out); err != nil {
				return false
			}
			if _, err := w.Write([]byte{'\n'}); err != nil {
				return false
			}
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return false
		}
		event, eventBytes = nil, 0
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	for {
		if ctx.Err() != nil {
			return
		}
		if !scanner.Scan() {
			return
		}
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			if !flushEvent() {
				return
			}
			continue
		}
		eventBytes += len(line) + 1
		if eventBytes > maxSSEEventBytes {
			return
		}
		event = append(event, append([]byte(nil), line...))
	}
}

// responsesEstimate declares the per-kind bound for one Responses attempt:
// input sized from the payload, output at the operation's configured
// maximum — the same shape the chat estimate uses.
func responsesEstimate(payload []byte) billing.Usage {
	return billing.Usage{
		InputUnits:  tokenEstimate(payload),
		OutputUnits: 1 << 17,
	}
}

// writeResponseError maps binding failures onto the public surface:
// unresolvable references deny without disclosing whether they exist,
// malformed bodies and unsupported cursors report honest 400s and internal
// failures never leak detail.
func (a *App) writeResponseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, postgres.ErrResponseBindingNotFound):
		WriteForbidden(w, "referenced resource is unavailable")
	case errors.Is(err, errBadBody):
		WriteOpenAIError(w, http.StatusBadRequest, "invalid JSON payload", "invalid_request_error", "invalid_json")
	case errors.Is(err, errBadQuery):
		WriteOpenAIError(w, http.StatusBadRequest, "unsupported or invalid query parameter", "invalid_request_error", "invalid_query")
	default:
		a.writeInferenceError(w, err)
	}
}

var (
	errBadBody  = errors.New("invalid request body")
	errBadQuery = errors.New("invalid responses query")
)
