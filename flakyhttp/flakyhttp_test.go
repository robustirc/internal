package flakyhttp_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robustirc/internal/flakyhttp"
)

type testEnv struct {
	tmpdir string
	fm     string
	ts     *httptest.Server
	rt     *flakyhttp.RoundTripper
	host   string
}

// TODO(go1.14): use t.Cleanup to streamline newTestEnv usage
func newTestEnv(pairs ...string) (*testEnv, error) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "okay")
	}))
	u, err := url.Parse(ts.URL)
	if err != nil {
		return nil, err
	}
	tmpdir, err := ioutil.TempDir("", "flakyhttp")
	if err != nil {
		return nil, err
	}

	fm := filepath.Join(tmpdir, "flakyhttp.rules")
	rt, err := flakyhttp.NewRoundTripper(fm, pairs...)
	if err != nil {
		return nil, err
	}
	return &testEnv{
		tmpdir: tmpdir,
		fm:     fm,
		ts:     ts,
		rt:     rt,
		host:   u.Host,
	}, nil
}

func (te *testEnv) configure(rules ...string) error {
	w := te.rt.ConfigChangeAwaiter()
	if err := ioutil.WriteFile(te.fm, []byte(strings.Join(rules, "\n")), 0644); err != nil {
		return err
	}
	ctx, canc := context.WithTimeout(context.Background(), 5*time.Second)
	defer canc()
	if err := w.Await(ctx); err != nil {
		return fmt.Errorf("flakyhttp.RoundTripper did not pick up config change: %v", err)
	}
	return nil
}

func (te *testEnv) works() bool {
	cl := &http.Client{Transport: te.rt}
	resp, err := cl.Get(te.ts.URL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	ioutil.ReadAll(resp.Body) // for keep-alive
	return true
}

func (te *testEnv) Close() {
	os.RemoveAll(te.tmpdir)
	te.ts.Close()
}

func TestFailAll(t *testing.T) {
	te, err := newTestEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer te.Close()

	if !te.works() {
		t.Fatalf("HTTP requests unexpectedly not working before configuring any rules")
	}

	if err := te.configure(fmt.Sprintf("dest=%s rate=100%%\n", te.host)); err != nil {
		t.Fatal(err)
	}

	if te.works() {
		t.Fatal("request unexpectedly did not fail")
	}
}

func TestFailAllConnections(t *testing.T) {
	te, err := newTestEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer te.Close()

	if err := te.configure(fmt.Sprintf("dest=%s rate=100%% stage=dial\n", te.host)); err != nil {
		t.Fatal(err)
	}

	if te.works() {
		t.Fatal("request unexpectedly did not fail")
	}
}

func TestFailNewConnections(t *testing.T) {
	te, err := newTestEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer te.Close()

	if !te.works() {
		t.Fatalf("HTTP requests unexpectedly not working before configuring any rules")
	}

	rules := []string{
		fmt.Sprintf("dest=%s rate=100%% stage=dial", te.host),
		fmt.Sprintf("dest=%s stage=request redial=force", te.host),
	}
	if err := te.configure(rules...); err != nil {
		t.Fatal(err)
	}

	if te.works() {
		t.Fatal("request unexpectedly did not fail")
	}
}

func TestApplicationSpecific(t *testing.T) {
	te, err := newTestEnv("robustirc=node2")
	if err != nil {
		t.Fatal(err)
	}
	defer te.Close()

	if !te.works() {
		t.Fatalf("HTTP requests unexpectedly not working before configuring any rules")
	}

	if err := te.configure("robustirc=node1 rate=100% stage=request"); err != nil {
		t.Fatal(err)
	}

	if !te.works() {
		t.Fatal("request unexpectedly failed")
	}

	if err := te.configure("robustirc=node2 rate=100% stage=request"); err != nil {
		t.Fatal(err)
	}

	if te.works() {
		t.Fatal("request unexpectedly did not fail")
	}
}

func TestFailRate(t *testing.T) {
	te, err := newTestEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer te.Close()

	if err := te.configure(fmt.Sprintf("dest=%s rate=50%% stage=request", te.host)); err != nil {
		t.Fatal(err)
	}

	failing := 0
	for i := 0; i < 100; i++ {
		if !te.works() {
			failing++
		}
	}
	const pm = 5
	if got, want := failing, 50; got < want-pm || got > want+pm {
		t.Fatalf("unexpected rate of failing requests: got %v, want %v ± %d", got, want, pm)
	}
}
