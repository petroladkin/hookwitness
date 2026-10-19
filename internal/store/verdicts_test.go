package store

import (
	"context"
	"fmt"
	"testing"
)

func TestVerdictsStored(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/v")
	r := sample(ep.ID, "{}")
	r.Verdict, r.Verdicts = "fail", `[{"check":1,"result":"fail"}]`
	if err := s.AddRequest(ctx, r, 10, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Request(ctx, r.ID)
	if err != nil || got.Verdict != "fail" || got.Verdicts != r.Verdicts {
		t.Fatalf("%+v %v", got, err)
	}
	list, _ := s.Requests(ctx, ep.ID, 0, 10)
	if list[0].Verdict != "fail" || list[0].Verdicts != r.Verdicts {
		t.Fatalf("list: %+v", list[0])
	}
}

func TestVerdictCounts(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/c")
	other, _ := s.CreateEndpoint(ctx, "/o")
	for _, v := range []string{"pass", "fail", "pass", "n/v", ""} {
		r := sample(ep.ID, "{}")
		r.Verdict = v
		s.AddRequest(ctx, r, 100, nil)
	}
	r := sample(other.ID, "{}")
	r.Verdict = "fail"
	s.AddRequest(ctx, r, 100, nil)
	got, err := s.VerdictCounts(ctx, ep.ID)
	if err != nil || len(got) != 3 || got["pass"] != 2 || got["fail"] != 1 || got["n/v"] != 1 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestSetVerdictsLeavesTheAnswer(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/s")
	r := sample(ep.ID, "{}")
	r.Status, r.Outcome, r.Verdict = 202, `{"action":"reply"}`, "pass"
	s.AddRequest(ctx, r, 10, nil)
	before, _ := s.ChangeSeq(ctx)
	if err := s.SetVerdicts(ctx, r.ID, "fail", `[{"result":"fail"}]`); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Request(ctx, r.ID)
	if got.Verdict != "fail" || got.Verdicts != `[{"result":"fail"}]` || got.Status != 202 || got.Outcome != r.Outcome {
		t.Fatalf("%+v", got)
	}
	changed, _, _ := s.RequestsChanged(ctx, ep.ID, r.ID, before)
	if len(changed) != 1 {
		t.Fatal("a re-check must refresh live rows")
	}
}

func TestEachRequest(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/e")
	other, _ := s.CreateEndpoint(ctx, "/x")
	for i := range 450 {
		s.AddRequest(ctx, sample(ep.ID, fmt.Sprint(i)), 1000, nil)
	}
	s.AddRequest(ctx, sample(other.ID, "other"), 1000, nil)
	var seen []string
	err := s.EachRequest(ctx, ep.ID, func(r Request) error {
		seen = append(seen, string(r.Body))
		return nil
	})
	if err != nil || len(seen) != 450 || seen[0] != "0" || seen[449] != "449" {
		t.Fatalf("%d requests, %v", len(seen), err)
	}
	stop := fmt.Errorf("stop")
	n := 0
	if err := s.EachRequest(ctx, ep.ID, func(Request) error { n++; return stop }); err != stop || n != 1 {
		t.Fatalf("an error stops the walk: %v after %d", err, n)
	}
}
