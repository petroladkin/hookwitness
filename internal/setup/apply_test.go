package setup

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

func TestApply(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f, err := Parse(Export(Source{Path: "/orders", Config: sample()}, Source{Path: "/refunds", Config: emulate.DefaultConfig()}).JSON(), opts)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := Apply(ctx, st, f, Plan{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Result != Created || rs[1].Result != Created || !reflect.DeepEqual(rs[0].Unset, []string{"GH", "TOK"}) || len(rs[1].Unset) != 0 {
		t.Fatalf("first import %+v", rs)
	}
	// Taken paths: refused without replace, nothing changes.
	if _, err := Apply(ctx, st, f, Plan{}, opts); !errors.Is(err, store.ErrDuplicatePath) {
		t.Fatalf("taken: %v", err)
	}
	// Someone sets GH's value; the same file applied again keeps it.
	ep, _ := st.Endpoint(ctx, rs[0].ID)
	c, _ := emulate.Parse(ep.Config)
	c.Secrets[0] = sample().Secrets[0]
	if err := st.SetConfig(ctx, ep.ID, c.String()); err != nil {
		t.Fatal(err)
	}
	before, _ := st.Endpoint(ctx, rs[0].ID)
	rs, err = Apply(ctx, st, f, Plan{Replace: Always}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rs[0].Result != Unchanged || rs[1].Result != Unchanged || !reflect.DeepEqual(rs[0].Unset, []string{"TOK"}) {
		t.Fatalf("second import %+v", rs)
	}
	after, _ := st.Endpoint(ctx, rs[0].ID)
	if after.Config != before.Config {
		t.Fatalf("a repeated import changed the behaviour:\n%s\n%s", before.Config, after.Config)
	}
	// A changed file replaces the behaviour.
	changed := sample()
	changed.Default.Status = 204
	g, _ := Parse(Export(Source{Path: "/orders", Config: changed}).JSON(), opts)
	rs, err = Apply(ctx, st, g, Plan{Replace: Always}, opts)
	if err != nil || rs[0].Result != Replaced {
		t.Fatalf("changed import %+v %v", rs, err)
	}
	// A standard check over a kept key that is not a whsec_ key cannot apply.
	bad := emulate.Config{Default: emulate.Reply{Status: 200}, Secrets: []emulate.Secret{{Name: "GH", Kind: emulate.SecretKey}},
		Checks: []emulate.Check{{Kind: emulate.KindStandard, Secret: "GH"}}}
	h, err := Parse(Export(Source{Path: "/orders", Config: bad}).JSON(), opts)
	if err != nil {
		t.Fatal(err)
	}
	var inv *InvalidError
	if _, err := Apply(ctx, st, h, Plan{Replace: Always}, opts); !errors.As(err, &inv) {
		t.Fatalf("standard over a plain key: %v", err)
	}
}
