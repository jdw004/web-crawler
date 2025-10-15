package crawler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type Options struct {
	Workers       int
	MaxDepth      int
	MaxPages      int
	Timeout       time.Duration
	Interval      time.Duration // Global request spacing, including redirects.
	MaxBodyBytes  int64
	AllowExternal bool
	UserAgent     string
}

func DefaultOptions() Options {
	return Options{
		Workers: 8, MaxDepth: 2, MaxPages: 100,
		Timeout: 10 * time.Second, MaxBodyBytes: 2 << 20,
		UserAgent: "go-crawler/1.0",
	}
}

type Result struct {
	URL         string   `json:"url"`
	FinalURL    string   `json:"final_url,omitempty"`
	Depth       int      `json:"depth"`
	Status      int      `json:"status"`
	Title       string   `json:"title,omitempty"`
	ContentType string   `json:"content_type,omitempty"`
	DurationMS  int64    `json:"duration_ms"`
	Links       []string `json:"links,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// Sink runs serially; returning an error stops the crawl.
type Sink func(Result) error

type job struct {
	url   string
	depth int
}

// Crawl streams page results to sink. Seeds start at depth zero.
func Crawl(ctx context.Context, seeds []string, opts Options, sink Sink) error {
	if opts.Workers < 1 || opts.MaxPages < 1 || opts.MaxDepth < 0 || opts.Timeout <= 0 || opts.Interval < 0 || opts.MaxBodyBytes < 1 || opts.MaxBodyBytes == 1<<63-1 {
		return errors.New("workers, max pages, timeout, and max body bytes must be positive; depth and interval must be nonnegative")
	}
	if len(seeds) == 0 || sink == nil {
		return errors.New("at least one seed URL and a result sink are required")
	}
	allowed := make(map[string]bool)
	queue := make([]job, 0, len(seeds))
	seen := make(map[string]bool)
	for _, seed := range seeds {
		u, err := normalize(seed, nil)
		if err != nil {
			return fmt.Errorf("seed %q: %w", seed, err)
		}
		allowed[u.Hostname()] = true
		if !seen[u.String()] {
			seen[u.String()] = true
			queue = append(queue, job{url: u.String()})
		}
	}
	if len(queue) > opts.MaxPages {
		return errors.New("max pages must be at least the number of distinct seeds")
	}
	inScope := func(u *url.URL) bool { return opts.AllowExternal || allowed[u.Hostname()] }
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = opts.Workers * 2
	transport.MaxIdleConnsPerHost = opts.Workers
	transport.MaxConnsPerHost = opts.Workers
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   opts.Timeout,
		Transport: &pacedTransport{base: transport, interval: opts.Interval},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			u, err := normalize(req.URL.String(), nil)
			if err != nil {
				return err
			}
			if !inScope(u) {
				return fmt.Errorf("redirect outside allowed hostnames: %s", u.Hostname())
			}
			return nil
		},
	}
	jobs := make(chan job)
	results := make(chan Result, opts.Workers)
	var wg sync.WaitGroup
	for i := 0; i < opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j, ok := <-jobs:
					if !ok {
						return
					}
					r := fetch(ctx, client, j, opts)
					select {
					case results <- r:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	defer func() { cancel(); close(jobs); wg.Wait() }()
	active, depth := 0, 0
	for len(queue) > 0 || active > 0 {
		var send chan job
		var next job
		if len(queue) > 0 {
			if active == 0 {
				depth = queue[0].depth
			}
			// Finish each depth first so the shortest route wins.
			if queue[0].depth == depth {
				send, next = jobs, queue[0]
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case send <- next:
			queue[0] = job{}
			queue = queue[1:]
			active++
		case r := <-results:
			active--
			if err := sink(r); err != nil {
				return fmt.Errorf("write result: %w", err)
			}
			if r.Depth >= opts.MaxDepth {
				continue
			}
			for _, link := range r.Links {
				if len(seen) >= opts.MaxPages {
					break
				}
				u, err := normalize(link, nil)
				if err == nil && inScope(u) && !seen[link] {
					seen[link] = true
					queue = append(queue, job{url: link, depth: r.Depth + 1})
				}
			}
		}
	}
	return ctx.Err()
}

type pacedTransport struct {
	base     http.RoundTripper
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

func (p *pacedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if p.interval > 0 {
		if err := p.wait(req.Context()); err != nil {
			return nil, err
		}
	}
	return p.base.RoundTrip(req)
}

func (p *pacedTransport) wait(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if delay := time.Until(p.next); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	p.next = time.Now().Add(p.interval)
	return ctx.Err()
}

func fetch(ctx context.Context, client *http.Client, j job, opts Options) (r Result) {
	start := time.Now()
	r = Result{URL: j.url, Depth: j.depth}
	defer func() { r.DurationMS = time.Since(start).Milliseconds() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		r.Error = err.Error()
		return
	}
	req.Header.Set("User-Agent", opts.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.1")
	resp, err := client.Do(req)
	if resp != nil {
		r.Status = resp.StatusCode
		r.FinalURL = resp.Request.URL.String()
		r.ContentType = resp.Header.Get("Content-Type")
	}
	if err != nil {
		r.Error = err.Error()
		return
	}
	defer resp.Body.Close()
	mediaType, _, _ := mime.ParseMediaType(r.ContentType)
	if mediaType != "" && mediaType != "text/html" && mediaType != "application/xhtml+xml" {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, opts.MaxBodyBytes+1))
	if err != nil {
		r.Error = err.Error()
		return
	}
	if int64(len(body)) > opts.MaxBodyBytes {
		r.Error = fmt.Sprintf("response exceeds %d bytes", opts.MaxBodyBytes)
		return
	}
	if mediaType == "" && !strings.HasPrefix(http.DetectContentType(body), "text/html") {
		return
	}
	r.Title, r.Links = parseHTML(bytes.NewReader(body), resp.Request.URL)
	return
}

func normalize(raw string, base *url.URL) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("URL must use http or https, have a hostname, and contain no credentials")
	}
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}

func parseHTML(body io.Reader, pageURL *url.URL) (string, []string) {
	z := html.NewTokenizer(body)
	base := pageURL
	baseFound, inTitle, titleFound := false, false, false
	var title strings.Builder
	var refs []string
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			if t.Data == "title" && !titleFound {
				inTitle, titleFound = true, true
			}
			if t.Data != "a" && t.Data != "area" && t.Data != "base" {
				continue
			}
			for _, attr := range t.Attr {
				if attr.Key != "href" {
					continue
				}
				if t.Data == "base" {
					if !baseFound {
						baseFound = true
						if u, err := normalize(attr.Val, pageURL); err == nil {
							base = u
						}
					}
				} else {
					refs = append(refs, attr.Val)
				}
				break
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "title" {
				inTitle = false
			}
		case html.TextToken:
			if inTitle {
				title.Write(z.Text())
			}
		}
	}
	var links []string
	seen := make(map[string]bool)
	for _, ref := range refs {
		if u, err := normalize(ref, base); err == nil && !seen[u.String()] {
			seen[u.String()] = true
			links = append(links, u.String())
		}
	}
	return strings.Join(strings.Fields(title.String()), " "), links
}
