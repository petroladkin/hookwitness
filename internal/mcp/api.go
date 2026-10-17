package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// client calls the REST API in-process: every tool goes through the same
// validation, errors and store access as an HTTP client of the API would.
type client struct {
	api  http.Handler // internal/api's handler
	base string       // the API's path, e.g. /admin/api/v1
	host string       // the host the MCP client addressed: catch URLs use it
}

// apiError is an API error answer, passed to the agent as a tool error.
type apiError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field"`
}

func (e *apiError) Error() string {
	s := e.Code + ": " + e.Message
	if e.Field != "" && !strings.Contains(e.Message, e.Field) {
		s += " (" + e.Field + ")"
	}
	return s
}

// recorder is the smallest ResponseWriter that keeps an answer.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

// do calls the API and returns the answer's headers and body; an API error
// answer comes back as *apiError.
func (c *client) do(ctx context.Context, method, path string, q url.Values, in any) (http.Header, []byte, error) {
	var body *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, nil, err
		}
		body = bytes.NewReader(b)
	} else if method != http.MethodGet && method != http.MethodDelete {
		body = bytes.NewReader([]byte("{}"))
	}
	target := c.base + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var req *http.Request
	var err error
	if body != nil {
		req, err = http.NewRequestWithContext(ctx, method, target, body)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, target, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	req.Host = c.host
	req.RemoteAddr = "127.0.0.1:0" // in-process
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &recorder{header: http.Header{}}
	c.api.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.status >= 400 {
		var e struct {
			Error apiError `json:"error"`
		}
		if json.Unmarshal(rec.body.Bytes(), &e) != nil || e.Error.Code == "" {
			return nil, nil, fmt.Errorf("the API answered %d", rec.status)
		}
		e.Error.Status = rec.status
		return nil, nil, &e.Error
	}
	return rec.header, rec.body.Bytes(), nil
}

// get decodes a GET answer into a generic JSON object.
func (c *client) get(ctx context.Context, path string, q url.Values) (map[string]any, error) {
	return c.call(ctx, http.MethodGet, path, q, nil)
}

func (c *client) call(ctx context.Context, method, path string, q url.Values, in any) (map[string]any, error) {
	_, b, err := c.do(ctx, method, path, q, in)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// endpointPath names an endpoint by id or exact path for the API's routes:
// "/endpoints/7"; "unmatched" is the Unmatched register.
func (c *client) endpointPath(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return "", fmt.Errorf("name the endpoint: its id or its path")
	case strings.EqualFold(ref, "unmatched"):
		return "/unmatched", nil
	case strings.HasPrefix(ref, "/"):
		list, err := c.get(ctx, "/endpoints", url.Values{"path": {ref}})
		if err != nil {
			return "", err
		}
		eps, _ := list["endpoints"].([]any)
		if len(eps) == 0 {
			return "", &apiError{Code: "not_found", Message: "there is no endpoint with the path " + ref + "; create_endpoint makes one"}
		}
		id, _ := eps[0].(map[string]any)["id"].(float64)
		return "/endpoints/" + strconv.FormatInt(int64(id), 10), nil
	}
	if _, err := strconv.ParseInt(ref, 10, 64); err != nil {
		return "", fmt.Errorf("%q is neither an endpoint id nor a path starting with /", ref)
	}
	return "/endpoints/" + ref, nil
}
