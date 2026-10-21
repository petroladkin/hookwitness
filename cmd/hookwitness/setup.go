package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/mcp"
	"github.com/petroladkin/hookwitness/internal/setup"
	"github.com/petroladkin/hookwitness/internal/store"
)

// importSetup applies the setup file of -import at start: its endpoints
// are created or their behaviour replaced, so every start gets the same
// endpoints. It logs and returns false when the file cannot be applied.
func importSetup(ctx context.Context, st *store.Store, path string, o setup.Options, log *slog.Logger) bool {
	data, err := readSetup(path)
	if err != nil {
		log.Error("cannot read the setup file", "file", path, "err", err)
		return false
	}
	f, err := setup.Parse(data, o)
	if err != nil {
		log.Error("cannot apply the setup file", "file", path, "err", err)
		return false
	}
	res, err := setup.Apply(ctx, st, f, setup.Plan{Replace: setup.Always}, o)
	if err != nil {
		log.Error("cannot apply the setup file", "file", path, "err", err)
		return false
	}
	n := map[string]int{}
	for _, r := range res {
		n[r.Result]++
		if r.Result != setup.Unchanged {
			log.Info("endpoint "+r.Result+" from a setup file", "path", r.Path, "secrets_not_set", strings.Join(r.Unset, ","))
		}
	}
	log.Info("setup applied", "file", path, "created", n[setup.Created], "replaced", n[setup.Replaced], "unchanged", n[setup.Unchanged])
	return true
}

func readSetup(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, setup.MaxSize+1))
	if err == nil && len(data) > setup.MaxSize {
		err = fmt.Errorf("over %d bytes", setup.MaxSize)
	}
	return data, err
}

// apiBase is the REST API under a server URL given to -url: the URL as is
// when it ends in /api/v1, else its path (default /admin) + /api/v1.
func apiBase(server string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(server))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("--url %q: give the server's URL, e.g. http://127.0.0.1:8080", server)
	}
	switch p := strings.TrimRight(u.Path, "/"); {
	case strings.HasSuffix(p, "/api/v1"):
		u.Path = p
	case p == "":
		u.Path = "/admin/api/v1"
	default:
		u.Path = p + "/api/v1"
	}
	return u.String(), nil
}

// apply sends a setup file to a running server: its endpoints are created
// or their behaviour replaced, so running it again changes nothing.
func apply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: hookwitness apply [-url URL] [-token-file FILE] <setup file>")
		fmt.Fprintln(stderr, "Creates the file's endpoints on the server, or replaces the behaviour of those that exist (secrets keep their values).")
		fs.PrintDefaults()
	}
	server := fs.String("url", "http://127.0.0.1:8080", "the hookwitness server (its /admin/api/v1 is used), or the API's full URL (e.g. http://127.0.0.1:8081/api/v1 for an admin at the root of its own address)")
	tokenFile := fs.String("token-file", "", "a file holding a token (from your account page) for a server with login on")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "hookwitness apply: give one setup file (hookwitness apply -h)")
		return 2
	}
	base, err := apiBase(*server)
	if err != nil {
		fmt.Fprintln(stderr, "hookwitness apply:", err)
		return 2
	}
	data, err := readSetup(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "hookwitness apply:", err)
		return 2
	}
	client := &http.Client{Timeout: time.Minute}
	if *tokenFile != "" {
		tok, err := readTokenFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(stderr, "hookwitness apply:", err)
			return 2
		}
		if client, err = mcp.WithToken(client, *server, tok); err != nil {
			fmt.Fprintln(stderr, "hookwitness apply:", err)
			return 2
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/setup?on_conflict=replace", strings.NewReader(string(data)))
	if err != nil {
		fmt.Fprintln(stderr, "hookwitness apply:", err)
		return 2
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "hookwitness apply: cannot reach %s: %v\n", base, err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var e struct {
			Error struct{ Code, Message string }
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		if resp.StatusCode == http.StatusUnauthorized && *tokenFile == "" {
			msg += " (the server has login on: give a token with -token-file)"
		}
		fmt.Fprintf(stderr, "hookwitness apply: %s: %s\n", resp.Status, msg)
		return 1
	}
	var out struct{ Endpoints []setup.Result }
	if err := json.Unmarshal(body, &out); err != nil {
		fmt.Fprintf(stderr, "hookwitness apply: %s did not answer as a hookwitness API: %v\n", base, err)
		return 1
	}
	for _, r := range out.Endpoints {
		line := r.Result + " " + r.Path
		if len(r.Unset) > 0 {
			line += " (secrets not set: " + strings.Join(r.Unset, ", ") + ")"
		}
		fmt.Fprintln(stdout, line)
	}
	return 0
}
