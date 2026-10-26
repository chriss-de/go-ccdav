package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
)

// Request-level errors (RFC 8620 §3.6.1), sent as RFC 7807 problems.
const (
	probUnknownCapability = "urn:ietf:params:jmap:error:unknownCapability"
	probNotJSON           = "urn:ietf:params:jmap:error:notJSON"
	probNotRequest        = "urn:ietf:params:jmap:error:notRequest"
	probLimit             = "urn:ietf:params:jmap:error:limit"
)

type problem struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Limit  string `json:"limit,omitempty"`
}

func writeProblem(w http.ResponseWriter, p problem) {
	if p.Status == 0 {
		p.Status = http.StatusBadRequest
	}
	b, _ := json.Marshal(p)
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	w.Write(b)
}

// Method-level error types (RFC 8620 §3.6.2 and §5).
const (
	ErrUnknownMethod               = "unknownMethod"
	ErrInvalidArguments            = "invalidArguments"
	ErrInvalidResultReference      = "invalidResultReference"
	ErrForbidden                   = "forbidden"
	ErrAccountNotFound             = "accountNotFound"
	ErrAccountNotSupportedByMethod = "accountNotSupportedByMethod"
	ErrAccountReadOnly             = "accountReadOnly"
	ErrServerFail                  = "serverFail"
	ErrServerUnavailable           = "serverUnavailable"
	ErrRequestTooLarge             = "requestTooLarge"
	ErrStateMismatch               = "stateMismatch"
	ErrCannotCalculateChanges      = "cannotCalculateChanges"
	ErrAnchorNotFound              = "anchorNotFound"
	ErrUnsupportedSort             = "unsupportedSort"
	ErrUnsupportedFilter           = "unsupportedFilter"
	ErrFromAccountNotFound         = "fromAccountNotFound"
)

// SetError types (RFC 8620 §5.3, RFC 9610).
const (
	SetForbidden              = "forbidden"
	SetNotFound               = "notFound"
	SetInvalidProperties      = "invalidProperties"
	SetInvalidPatch           = "invalidPatch"
	SetAlreadyExists          = "alreadyExists"
	SetTooLarge               = "tooLarge"
	SetOverQuota              = "overQuota"
	SetAddressBookHasContents = "addressBookHasContents"
)

// MethodError is a method-level error, sent as an "error" response.
type MethodError struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

func (e *MethodError) Error() string { return e.Type + ": " + e.Description }

// Errorf returns a MethodError of the given type.
func Errorf(typ, format string, args ...any) *MethodError {
	return &MethodError{Type: typ, Description: fmt.Sprintf(format, args...)}
}

// invocation is one method call or response: [name, arguments, call id].
type invocation struct {
	Name string
	Args json.RawMessage
	ID   string
}

func (iv *invocation) UnmarshalJSON(b []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}
	if len(parts) != 3 {
		return errors.New("an invocation has 3 elements")
	}
	if err := json.Unmarshal(parts[0], &iv.Name); err != nil {
		return err
	}
	if err := json.Unmarshal(parts[2], &iv.ID); err != nil {
		return err
	}
	if t := bytes.TrimSpace(parts[1]); len(t) == 0 || t[0] != '{' {
		return errors.New("arguments must be an object")
	}
	iv.Args = parts[1]
	return nil
}

func (iv invocation) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{iv.Name, iv.Args, iv.ID})
}

type request struct {
	Using       []string           `json:"using"`
	MethodCalls []invocation       `json:"methodCalls"`
	CreatedIDs  *map[string]string `json:"createdIds"`
}

type response struct {
	MethodResponses []invocation       `json:"methodResponses"`
	CreatedIDs      *map[string]string `json:"createdIds,omitempty"`
	SessionState    string             `json:"sessionState"`
}

// Call is the context of one method call.
type Call struct {
	h    *Handler
	User string
	// created maps creation ids ("#x" without the "#") to the ids of
	// objects created earlier in the same request.
	created  map[string]string
	id       string       // the current method call id
	implicit []invocation // responses of implicit calls (RFC 8620 §5.4)
	// refBudget is what result references may still copy (resolveRefs).
	refBudget int
	// ctx is the request's, with workTimeout: expansions stop when it is
	// done, and calls still running then fail (run).
	ctx context.Context
}

// Respond adds the response of an implicit method call, made on behalf
// of the current one (like /copy's destroy of the originals). It follows
// the current method's response.
func (c *Call) Respond(name string, args any) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	c.implicit = append(c.implicit, invocation{Name: name, Args: b, ID: c.id})
	return nil
}

// Handler returns the handler serving the call.
func (c *Call) Handler() *Handler { return c.h }

// Created records the id of an object created under a creation id.
func (c *Call) Created(creationID, id string) { c.created[creationID] = id }

// ResolveID turns "#creationId" into the id of the object created under
// it earlier in the request. Other ids are returned unchanged.
func (c *Call) ResolveID(id string) (string, bool) {
	cid, ok := strings.CutPrefix(id, "#")
	if !ok {
		return id, true
	}
	real, ok := c.created[cid]
	return real, ok
}

func (h *Handler) api(w http.ResponseWriter, r *http.Request, user string) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeProblem(w, problem{Type: probNotJSON, Detail: "Content-Type must be application/json"})
		return
	}
	release, ok := slot(&h.busy, user, maxConcurrentRequests)
	if !ok {
		writeProblem(w, problem{Type: probLimit, Limit: "maxConcurrentRequests", Detail: "too many concurrent requests"})
		return
	}
	defer release()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSizeRequest))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeProblem(w, problem{Type: probLimit, Limit: "maxSizeRequest", Detail: "request too large"})
		} else {
			writeProblem(w, problem{Type: probNotJSON, Detail: err.Error()})
		}
		return
	}
	if !json.Valid(body) {
		writeProblem(w, problem{Type: probNotJSON, Detail: "body is not valid JSON"})
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil || req.Using == nil || req.MethodCalls == nil {
		detail := "using and methodCalls are required"
		if err != nil {
			detail = err.Error()
		}
		writeProblem(w, problem{Type: probNotRequest, Detail: detail})
		return
	}
	using := map[string]bool{}
	for _, u := range req.Using {
		if _, ok := h.caps[u]; !ok {
			writeProblem(w, problem{Type: probUnknownCapability, Detail: "unknown capability " + u})
			return
		}
		using[u] = true
	}
	if len(req.MethodCalls) > maxCallsInRequest {
		writeProblem(w, problem{Type: probLimit, Limit: "maxCallsInRequest", Detail: "too many method calls"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), workTimeout)
	defer cancel()
	c := &Call{h: h, User: user, created: map[string]string{}, refBudget: maxSizeRequest, ctx: ctx}
	if req.CreatedIDs != nil {
		for k, v := range *req.CreatedIDs {
			c.created[k] = v
		}
	}
	resp := response{MethodResponses: []invocation{}}
	for _, iv := range req.MethodCalls {
		c.id, c.implicit = iv.ID, nil
		resp.MethodResponses = append(resp.MethodResponses, h.run(c, using, iv, resp.MethodResponses))
		resp.MethodResponses = append(resp.MethodResponses, c.implicit...)
	}
	if req.CreatedIDs != nil {
		resp.CreatedIDs = &c.created
	}
	resp.SessionState = h.sessionState(user)
	writeJSON(w, http.StatusOK, resp)
}

// run executes one method call; done holds the responses so far, for
// result references.
func (h *Handler) run(c *Call, using map[string]bool, iv invocation, done []invocation) invocation {
	fail := func(e *MethodError) invocation {
		b, _ := json.Marshal(e)
		return invocation{Name: "error", Args: b, ID: iv.ID}
	}
	m, ok := h.methods[iv.Name]
	if !ok || !using[m.capability] {
		return fail(Errorf(ErrUnknownMethod, "unknown method %s", iv.Name))
	}
	args, merr := resolveRefs(iv.Args, done, &c.refBudget)
	if merr != nil {
		return fail(merr)
	}
	tooSlow := &MethodError{Type: ErrServerUnavailable, Description: "the request took too long"}
	if c.ctx.Err() != nil {
		return fail(tooSlow)
	}
	res, err := h.call(m.fn, c, args)
	if c.ctx.Err() != nil {
		return fail(tooSlow) // expansions may have stopped early
	}
	if err != nil {
		var me *MethodError
		if errors.As(err, &me) {
			return fail(me)
		}
		h.log.Error("jmap method failed", "method", iv.Name, "user", c.User, "err", err)
		return fail(&MethodError{Type: ErrServerFail})
	}
	b, err := json.Marshal(res)
	if err != nil {
		h.log.Error("jmap response", "method", iv.Name, "err", err)
		return fail(&MethodError{Type: ErrServerFail})
	}
	return invocation{Name: iv.Name, Args: b, ID: iv.ID}
}

// call runs fn, turning a panic into an error.
func (h *Handler) call(fn MethodFunc, c *Call, args json.RawMessage) (res any, err error) {
	defer func() {
		if v := recover(); v != nil {
			h.log.Error("jmap method panic", "user", c.User, "err", v, "stack", string(debug.Stack()))
			res, err = nil, &MethodError{Type: ErrServerFail}
		}
	}()
	return fn(c, args)
}

// resolveRefs replaces "#name" arguments, which are result references
// (RFC 8620 §3.7), with the values they point at. Each reference can copy
// a whole earlier response, so chained calls could grow exponentially: the
// copied values of one request together may not exceed *budget bytes.
func resolveRefs(raw json.RawMessage, done []invocation, budget *int) (json.RawMessage, *MethodError) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, Errorf(ErrInvalidArguments, "arguments must be an object")
	}
	var refs []string
	for k := range args {
		if strings.HasPrefix(k, "#") {
			refs = append(refs, k)
		}
	}
	if len(refs) == 0 {
		return raw, nil
	}
	slices.Sort(refs)
	for _, k := range refs {
		name := k[1:]
		if _, dup := args[name]; dup {
			return nil, Errorf(ErrInvalidArguments, "both %s and %s given", name, k)
		}
		var ref struct {
			ResultOf *string `json:"resultOf"`
			Name     *string `json:"name"`
			Path     *string `json:"path"`
		}
		if err := json.Unmarshal(args[k], &ref); err != nil || ref.ResultOf == nil || ref.Name == nil || ref.Path == nil {
			return nil, Errorf(ErrInvalidResultReference, "%s is not a result reference", k)
		}
		i := slices.IndexFunc(done, func(iv invocation) bool { return iv.ID == *ref.ResultOf })
		if i < 0 || done[i].Name != *ref.Name {
			return nil, Errorf(ErrInvalidResultReference, "%s: no %s response with call id %q", k, *ref.Name, *ref.ResultOf)
		}
		dec := json.NewDecoder(bytes.NewReader(done[i].Args))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, Errorf(ErrInvalidResultReference, "%s: %v", k, err)
		}
		val, err := evalPointer(v, *ref.Path)
		if err != nil {
			return nil, Errorf(ErrInvalidResultReference, "%s: %v", k, err)
		}
		b, err := json.Marshal(val)
		if err != nil {
			return nil, Errorf(ErrInvalidResultReference, "%s: %v", k, err)
		}
		if *budget -= len(b); *budget < 0 {
			return nil, Errorf(ErrRequestTooLarge, "result references copy more than %d bytes", maxSizeRequest)
		}
		delete(args, k)
		args[name] = b
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, Errorf(ErrInvalidArguments, "%v", err)
	}
	return b, nil
}

// evalPointer evaluates a JSON Pointer (RFC 6901) with JMAP's "*"
// extension: on an array, the rest of the path is applied to every item,
// and array results are flattened into one array.
func evalPointer(v any, path string) (any, error) {
	if path == "" {
		return v, nil
	}
	if path[0] != '/' {
		return nil, fmt.Errorf("path %q must start with /", path)
	}
	return walkPointer(v, strings.Split(path[1:], "/"))
}

var pointerUnescape = strings.NewReplacer("~1", "/", "~0", "~")

func walkPointer(v any, toks []string) (any, error) {
	if len(toks) == 0 {
		return v, nil
	}
	tok := pointerUnescape.Replace(toks[0])
	switch x := v.(type) {
	case map[string]any:
		child, ok := x[tok]
		if !ok {
			return nil, fmt.Errorf("no property %q", tok)
		}
		return walkPointer(child, toks[1:])
	case []any:
		if tok == "*" {
			out := []any{}
			for _, item := range x {
				r, err := walkPointer(item, toks[1:])
				if err != nil {
					return nil, err
				}
				if arr, ok := r.([]any); ok {
					out = append(out, arr...)
				} else {
					out = append(out, r)
				}
			}
			return out, nil
		}
		i, err := strconv.Atoi(tok)
		if err != nil || i < 0 || i >= len(x) || tok != strconv.Itoa(i) {
			return nil, fmt.Errorf("bad array index %q", tok)
		}
		return walkPointer(x[i], toks[1:])
	default:
		return nil, fmt.Errorf("cannot descend into %q", tok)
	}
}

// DecodeArgs unmarshals method arguments, reporting failures as
// invalidArguments.
func DecodeArgs(raw json.RawMessage, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return Errorf(ErrInvalidArguments, "%v", err)
	}
	return nil
}

// echo is Core/echo (RFC 8620 §4.1).
func echo(_ *Call, args json.RawMessage) (any, error) { return args, nil }
