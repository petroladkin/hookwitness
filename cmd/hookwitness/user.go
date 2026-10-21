package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/config"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

// Exit codes of hookwitness user, for scripts.
const (
	exitOK      = 0
	exitFailed  = 1 // the database or the key
	exitUsage   = 2 // bad flags, a bad name, a short password
	exitNoUser  = 3 // no such user
	exitRefused = 4 // the name is taken, or the last admin
)

const userUsage = `Usage: hookwitness user <command> [flags]

Manage who may log in, on the database directly (the server may be running).
Reads -db / HOOKWITNESS_DB and the secret key (HOOKWITNESS_SECRET_KEY or the
.key file next to the database) as serve does.

Commands:
  add <name> [-admin] [-password-file f]   add a user; the password from the file, else from stdin
  passwd <name> [-password-file f]         set a new password; every login of that user ends
  rm <name>                                remove a user with their tokens
  list [-json]                             list the users

Exit codes: 0 done, 1 failed (database, key), 2 usage, 3 no such user,
4 refused (the name is taken, or the last admin).
`

// stdin of hookwitness user (tests replace it) and whether it is a terminal.
var (
	userIn       io.Reader = os.Stdin
	userTerminal           = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
)

func userCmd(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, userUsage)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("user "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	db := fs.String("db", envOr(getenv, "HOOKWITNESS_DB", config.DefaultDBPath), "path to the sqlite database file (env HOOKWITNESS_DB)")
	admin := fs.Bool("admin", false, "add the user as an admin")
	pwFile := fs.String("password-file", "", "read the password from this file (one line) instead of stdin")
	asJSON := fs.Bool("json", false, "list as JSON")
	// The name comes first, flags after it (or before: both work).
	var name string
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		fmt.Fprintf(stderr, "hookwitness user %s: %v\n", cmd, err)
		return exitUsage
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	} else if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "hookwitness user %s: unexpected argument %q\n", cmd, fs.Arg(0))
		return exitUsage
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "hookwitness user %s: %s\n", cmd, fmt.Sprintf(format, a...))
		return code
	}
	switch cmd {
	case "add", "passwd", "rm":
		if name == "" {
			return fail(exitUsage, "name the user")
		}
		n, err := access.CheckName(name)
		if err != nil {
			return fail(exitUsage, "%v", err)
		}
		name = n
	case "list":
		if name != "" {
			return fail(exitUsage, "list takes no name")
		}
	default:
		fmt.Fprintf(stderr, "hookwitness user: unknown command %q\n\n%s", cmd, userUsage)
		return exitUsage
	}

	// The key must exist already: hashes made with a new one would never
	// match on the server.
	keyEnv := getenv("HOOKWITNESS_SECRET_KEY")
	if keyEnv == "" {
		if _, err := os.Stat(*db + ".key"); err != nil {
			return fail(exitFailed, "no secret key (%s.key): start the server once, or set HOOKWITNESS_SECRET_KEY", *db)
		}
	}
	if _, err := os.Stat(*db); err != nil {
		return fail(exitFailed, "no database at %s: start the server once, or give -db", *db)
	}
	keys, err := secret.Load(keyEnv, *db+".key")
	if err != nil {
		return fail(exitFailed, "%v", err)
	}
	st, err := store.Open(ctx, *db)
	if err != nil {
		return fail(exitFailed, "%v", err)
	}
	defer st.Close()
	auth := access.New(st, keys, "")

	switch cmd {
	case "list":
		return listUsers(ctx, st, *asJSON, stdout, stderr)
	case "rm":
		_, err := st.RemoveUser(ctx, name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return fail(exitNoUser, "no user %s", name)
		case errors.Is(err, store.ErrLastAdmin):
			return fail(exitRefused, "%s is the last admin: make another user an admin first", name)
		case err != nil:
			return fail(exitFailed, "%v", err)
		}
		fmt.Fprintf(stderr, "removed %s\n", name)
		return exitOK
	}

	if cmd == "passwd" {
		if _, err := st.UserByName(ctx, name); errors.Is(err, store.ErrNotFound) {
			return fail(exitNoUser, "no user %s", name)
		} else if err != nil {
			return fail(exitFailed, "%v", err)
		}
	}
	pw, err := readPassword(*pwFile, stderr)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if err := access.CheckPassword(pw); err != nil {
		return fail(exitUsage, "%v", err)
	}
	if cmd == "passwd" {
		if _, err := st.SetPassword(ctx, name, auth.HashPassword(pw), 0); err != nil {
			return fail(exitFailed, "%v", err)
		}
		fmt.Fprintf(stderr, "password of %s set; their logins have ended\n", name)
		return exitOK
	}
	role := store.RoleUser
	if *admin {
		role = store.RoleAdmin
	}
	_, err = api.AddUser(ctx, st, auth, name, role, pw)
	if e, ok := access.IsInvalid(err); ok {
		if e.Field == "role" {
			return fail(exitRefused, "%s (add -admin)", e.Message)
		}
		return fail(exitUsage, "%s", e.Message)
	}
	switch {
	case errors.Is(err, store.ErrExists):
		return fail(exitRefused, "a user named %s exists", name)
	case err != nil:
		return fail(exitFailed, "%v", err)
	}
	fmt.Fprintf(stderr, "added %s (%s)\n", name, role)
	return exitOK
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// readPassword reads a password from a file (its first line), from a
// terminal without echo (asked twice), or from piped stdin (its first line).
func readPassword(file string, prompt io.Writer) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		line, _, _ := strings.Cut(string(b), "\n")
		return strings.TrimRight(line, "\r"), nil
	}
	if userTerminal() {
		fmt.Fprint(prompt, "Password: ")
		a, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		fmt.Fprint(prompt, "Again: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("the two passwords differ")
		}
		return string(a), nil
	}
	line, err := bufio.NewReader(userIn).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func listUsers(ctx context.Context, st *store.Store, asJSON bool, stdout, stderr io.Writer) int {
	us, err := st.Users(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "hookwitness user list:", err)
		return exitFailed
	}
	if asJSON {
		type row struct {
			Name    string `json:"name"`
			Role    string `json:"role"`
			Created string `json:"created_at"`
			Tokens  int    `json:"tokens"`
		}
		out := []row{}
		for _, u := range us {
			out = append(out, row{u.Name, u.Role, u.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"), u.Tokens})
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tADDED\tTOKENS")
	for _, u := range us {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", u.Name, u.Role, u.CreatedAt.UTC().Format(time.DateTime), u.Tokens)
	}
	tw.Flush()
	return exitOK
}
