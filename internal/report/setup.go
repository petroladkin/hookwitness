package report

import (
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/emulate"
)

// Setup is an endpoint's setup as a report shows it, in words: checks in
// order, schemas, rules in order, the default reply, variables, secrets.
// It is the setup when the report is generated, not a history. A secret is
// its name, its kind and whether it is set: never its value, seal, hash or
// fingerprint.
type Setup struct {
	Checks  []SetupCheck
	Schemas []SetupSchema
	Rules   []SetupRule
	Default SetupReply
	Vars    []emulate.Var
	Secrets []SetupSecret
}

// SetupCheck is one check: its number, label and what it does.
type SetupCheck struct {
	No            int
	Label, Detail string
}

// SetupSchema is one schema and the checks that use it.
type SetupSchema struct{ Name, Use string }

// SetupRule is one rule: its condition and action, and its reply when it
// answers.
type SetupRule struct {
	No         int
	When, Then string
	Reply      *SetupReply
}

// SetupReply is a reply as configured: the body is the template, or a note
// for exact bytes.
type SetupReply struct {
	Status  int
	Delay   string
	Headers []emulate.Header
	Body    string // the template; "" with Exact or for an empty body
	Exact   string // "4 B given as exact bytes"
}

// SetupSecret is a secret named, never shown.
type SetupSecret struct{ Name, Kind, State, Use string }

// describeSetup puts a behaviour in words.
func describeSetup(c emulate.Config) *Setup {
	s := &Setup{Default: setupReply(c.Default), Vars: c.Vars}
	for i, ch := range c.Checks {
		s.Checks = append(s.Checks, SetupCheck{No: i + 1, Label: ch.Label(), Detail: ch.Summary()})
	}
	for _, sc := range c.Schemas {
		s.Schemas = append(s.Schemas, SetupSchema{Name: sc.Name, Use: use(c.SchemaUsers(sc.Name))})
	}
	for i, r := range c.Rules {
		sr := SetupRule{No: i + 1, When: r.When.Label(), Then: r.Action()}
		if r.Do == emulate.ActReply {
			rep := setupReply(r.Reply)
			sr.Reply = &rep
		}
		s.Rules = append(s.Rules, sr)
	}
	for _, sc := range c.Secrets {
		state := "not set"
		if sc.Set() {
			state = "set"
		}
		s.Secrets = append(s.Secrets, SetupSecret{Name: sc.Name, Kind: emulate.SecretKindTitle(sc.Kind), State: state, Use: use(c.SecretUsers(sc.Name))})
	}
	return s
}

func setupReply(r emulate.Reply) SetupReply {
	out := SetupReply{Status: r.Status, Delay: r.DelayLabel(), Headers: r.Headers, Body: r.Body}
	if r.BodyBase64 != "" {
		n := base64.StdEncoding.DecodedLen(len(r.BodyBase64)) - strings.Count(r.BodyBase64, "=")
		out.Body, out.Exact = "", size(int64(n))+" given as exact bytes"
	}
	return out
}

func use(users []string) string {
	if len(users) == 0 {
		return "not used"
	}
	return strings.Join(users, ", ")
}

// setupIntro is the sentence under the section's heading.
func setupIntro(h *Head) string {
	return "The endpoint's setup when this report was generated, " + stamp(h.Generated) + "."
}

// ReplyTitle names a rule's reply: "Rule 1 · every 3rd → 503".
func (r SetupRule) ReplyTitle() string {
	return "Rule " + strconv.Itoa(r.No) + " · " + r.When + " → " + strconv.Itoa(r.Reply.Status)
}

// BodyLine says what a reply's body is when it is not a template shown.
func (r SetupReply) BodyLine() string {
	switch {
	case r.Exact != "":
		return r.Exact
	case r.Body == "":
		return "empty"
	}
	return ""
}
