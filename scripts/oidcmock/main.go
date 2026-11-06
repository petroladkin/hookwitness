// Command oidcmock runs the test OpenID Connect provider of
// internal/oidc/oidctest for scripts (the API scenario, make media): it
// approves every sign-in at once with the identity set by flags, or later
// by POST <issuer>/_test/next {"sub", "email", "email_verified"}. It prints
// its issuer URL and runs until interrupted. Never part of the server.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/petroladkin/hookwitness/internal/oidc/oidctest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "address to listen on (loopback)")
	clientID := flag.String("client-id", "hookwitness", "the client id hookwitness is configured with")
	secretFile := flag.String("client-secret-file", "", "a file with the client secret (required)")
	sub := flag.String("sub", "mock-user-1", "the subject of the identity returned")
	email := flag.String("email", "", "the email of the identity returned")
	verified := flag.Bool("verified", true, "whether the email is verified")
	flag.Parse()
	b, err := os.ReadFile(*secretFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "oidcmock: -client-secret-file:", err)
		os.Exit(2)
	}
	p, err := oidctest.Listen(*addr, *clientID, strings.TrimSpace(string(b)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "oidcmock:", err)
		os.Exit(1)
	}
	p.Next(oidctest.Claims{Subject: *sub, Email: *email, EmailVerified: oidctest.Ptr(*verified)})
	fmt.Println(p.URL)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	p.Close()
}
