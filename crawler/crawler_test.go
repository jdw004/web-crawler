package crawler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func collect(t *testing.T, seeds []string, opts Options) map[string]Result {
	t.Helper()
	results := make(map[string]Result)
	if err := Crawl(t.Context(), seeds, opts, func(r Result) error {
		if _, exists := results[r.URL]; exists {
			t.Errorf("duplicate result: %s", r.URL)
		}
		results[r.URL] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return results
}

func TestDepthAndDeduplication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<title> Home &amp; links </title><a href="/a#one">a</a><a href="/a#two">a</a><a href="/b">b</a><a href="https://outside.invalid/">outside</a><a href="mailto:x@y.com">mail</a>`)
		case "/a", "/b":
			fmt.Fprint(w, `<a href="/">home</a><a href="/c">c</a>`)
		case "/c":
			fmt.Fprint(w, `<a href="/too-deep">stop</a>`)
		default:
			t.Errorf("unexpected request: %s", r.URL)
		}
	}))
	defer server.Close()
	for depth, want := range []int{1, 3, 4} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			opts := DefaultOptions()
			opts.MaxDepth = depth
			results := collect(t, []string{server.URL, server.URL + "/#duplicate"}, opts)
			if len(results) != want {
				t.Fatalf("got %d pages, want %d: %v", len(results), want, results)
			}
			if results[server.URL+"/"].Title != "Home & links" {
				t.Fatal("title was not decoded")
			}
			if depth == 2 && results[server.URL+"/c"].Depth != 2 {
				t.Fatal("wrong depth")
			}
		})
	}
}

func TestPageLimit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/html")
		for i := 0; i < 100; i++ {
			fmt.Fprintf(w, `<a href="/%d">link</a>`, i)
		}
	}))
	defer server.Close()
	opts := DefaultOptions()
	opts.MaxPages = 7
	results := collect(t, []string{server.URL}, opts)
	if len(results) != 7 || requests.Load() != 7 {
		t.Fatalf("results=%d requests=%d", len(results), requests.Load())
	}
}

func TestWorkerConcurrency(t *testing.T) {
	arrived := make(chan struct{}, 3)
	release := make(chan struct{})
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<a href="/a">a</a><a href="/b">b</a><a href="/c">c</a>`)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	var once sync.Once
	defer once.Do(func() { close(release) })
	opts := DefaultOptions()
	opts.Workers = 2
	done := make(chan error, 1)
	go func() { done <- Crawl(t.Context(), []string{server.URL}, opts, func(Result) error { return nil }) }()
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatal("workers did not fetch concurrently")
		}
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak.Load())
	}
}

func TestShortestDepthWins(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<a href="/fast">fast</a><a href="/slow">slow</a>`)
		case "/fast":
			fmt.Fprint(w, `<a href="/detour">detour</a>`)
		case "/slow":
			time.Sleep(30 * time.Millisecond)
			fmt.Fprint(w, `<a href="/target">target</a>`)
		case "/detour":
			fmt.Fprint(w, `<a href="/target">target</a>`)
		case "/target":
			fmt.Fprint(w, `<a href="/leaf">leaf</a>`)
		}
	}))
	defer server.Close()
	opts := DefaultOptions()
	opts.MaxDepth = 3
	results := collect(t, []string{server.URL}, opts)
	if r, ok := results[server.URL+"/leaf"]; !ok || r.Depth != 3 {
		t.Fatalf("short route was lost: %v", results)
	}
}

func TestRedirectScope(t *testing.T) {
	var externalHits atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalHits.Add(1)
		fmt.Fprint(w, "<title>External</title>")
	}))
	defer external.Close()
	target := strings.Replace(external.URL, "127.0.0.1", "localhost", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target, http.StatusFound) }))
	defer server.Close()
	opts := DefaultOptions()
	results := collect(t, []string{server.URL}, opts)
	if !strings.Contains(results[server.URL+"/"].Error, "outside allowed") || externalHits.Load() != 0 {
		t.Fatalf("external redirect was not blocked: %v", results)
	}
	opts.AllowExternal = true
	results = collect(t, []string{server.URL}, opts)
	if results[server.URL+"/"].Title != "External" || externalHits.Load() != 1 {
		t.Fatalf("external redirect was not followed: %v", results)
	}
}

func TestRedirectRelativeLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/dir/index", http.StatusFound)
		case "/dir/index":
			fmt.Fprint(w, `<a href="child">child</a>`)
		case "/dir/child":
			fmt.Fprint(w, "<title>Child</title>")
		default:
			t.Errorf("wrong relative URL: %s", r.URL)
		}
	}))
	defer server.Close()
	results := collect(t, []string{server.URL}, DefaultOptions())
	if results[server.URL+"/"].FinalURL != server.URL+"/dir/index" || results[server.URL+"/dir/child"].Title != "Child" {
		t.Fatalf("wrong redirect results: %v", results)
	}
}

func TestResponseFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/large":
			fmt.Fprint(w, strings.Repeat("x", 129))
		case "/timeout":
			<-r.Context().Done()
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/binary":
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, `<a href="/unexpected">ignored</a>`)
		}
	}))
	defer server.Close()
	opts := DefaultOptions()
	opts.Timeout = 100 * time.Millisecond
	opts.MaxBodyBytes = 128
	results := collect(t, []string{server.URL + "/large", server.URL + "/timeout", server.URL + "/missing", server.URL + "/binary"}, opts)
	if len(results) != 4 || !strings.Contains(results[server.URL+"/large"].Error, "exceeds") || results[server.URL+"/timeout"].Error == "" || results[server.URL+"/missing"].Status != 404 || len(results[server.URL+"/binary"].Links) != 0 {
		t.Fatalf("unexpected responses: %v", results)
	}
}

func TestCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Crawl(ctx, []string{server.URL}, DefaultOptions(), func(Result) error { return nil }) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop workers")
	}
}

func TestSinkFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<a href="/next">next</a>`) }))
	defer server.Close()
	want := errors.New("storage unavailable")
	err := Crawl(t.Context(), []string{server.URL}, DefaultOptions(), func(Result) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want sink failure", err)
	}
}

func TestRateLimitIncludesRedirects(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/next", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<a href="/a">a</a><a href="/b">b</a>`)
	}))
	defer server.Close()
	opts := DefaultOptions()
	opts.Interval = 30 * time.Millisecond
	collect(t, []string{server.URL}, opts)
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 4 {
		t.Fatalf("got %d requests", len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < opts.Interval-10*time.Millisecond {
			t.Fatalf("requests too close: %s", gap)
		}
	}
}

func TestParseHTML(t *testing.T) {
	u, _ := url.Parse("https://example.com/dir/page")
	title, links := parseHTML(strings.NewReader(`<title> A &amp; B </title><script>const x = '<a href="/fake">';</script><a href="child?x=1&amp;y=2#frag">child</a><base href="/base/"><a href="child?x=1&amp;y=2">duplicate</a><area href="/map"><a href="javascript:void(0)">skip</a>`), u)
	want := []string{"https://example.com/base/child?x=1&y=2", "https://example.com/map"}
	if title != "A & B" || !reflect.DeepEqual(links, want) {
		t.Fatalf("title=%q links=%v", title, links)
	}
}

func TestNormalize(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTP://EXAMPLE.COM:80#frag":         "http://example.com/",
		"https://Example.com:443/a?q=1#frag": "https://example.com/a?q=1",
		"http://[::1]:80":                    "http://[::1]/",
	} {
		u, err := normalize(raw, nil)
		if err != nil || u.String() != want {
			t.Errorf("normalize(%q) = %v, %v; want %q", raw, u, err, want)
		}
	}
	for _, raw := range []string{"example.com", "ftp://example.com", "http:///path", "https://user:pass@example.com", "http://example.com:bad"} {
		if _, err := normalize(raw, nil); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestInvalidOptions(t *testing.T) {
	for _, change := range []func(*Options){
		func(o *Options) { o.Workers = 0 },
		func(o *Options) { o.MaxDepth = -1 },
		func(o *Options) { o.MaxPages = 0 },
		func(o *Options) { o.Timeout = 0 },
		func(o *Options) { o.Interval = -1 },
		func(o *Options) { o.MaxBodyBytes = 0 },
	} {
		opts := DefaultOptions()
		change(&opts)
		if err := Crawl(t.Context(), []string{"https://example.com"}, opts, func(Result) error { return nil }); err == nil {
			t.Errorf("accepted invalid options: %+v", opts)
		}
	}
}

func BenchmarkCrawl(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			for i := 0; i < 64; i++ {
				fmt.Fprintf(w, `<a href="/%d">page</a>`, i)
			}
		} else {
			fmt.Fprint(w, "<title>Page</title>")
		}
	}))
	defer server.Close()
	for _, workers := range []int{1, 8, 16} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			opts := DefaultOptions()
			opts.Workers = workers
			for b.Loop() {
				count := 0
				err := Crawl(b.Context(), []string{server.URL}, opts, func(Result) error {
					count++
					return nil
				})
				if err != nil || count != 65 {
					b.Fatalf("pages=%d error=%v", count, err)
				}
			}
		})
	}
}
