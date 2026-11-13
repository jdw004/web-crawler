# Go Web Crawler

Concurrent crawler with URL deduplication, depth limits, and JSON Lines output.
Requires Go 1.26+.

```sh
go build -o go-crawler ./cmd/crawler
./go-crawler https://example.com
./go-crawler -workers 16 -depth 3 -max-pages 500 -output results.jsonl https://example.com
./go-crawler -interval 100ms https://example.com https://example.org
```

Defaults: 8 workers, depth 2, 100 pages, 10s timeout, 2 MiB per HTML response.
Seeds are depth 0. Flags go before URLs; `-help` lists them all.

Only seed hostnames are followed, including redirects. Use `-allow-external`
to cross domains. `-interval` spaces requests globally; the default is unlimited.
The page limit counts distinct requested URLs; redirects can make extra requests.

Each result includes the URL, status, title, links, and elapsed milliseconds.
Page failures appear in the output; configuration and output errors exit nonzero.
Results stream as pages finish. Output files must not already exist.
Ctrl+C cancels pending work and keeps results already written.

For custom storage, pass a `crawler.Sink` to `crawler.Crawl`.

```sh
go test -race ./...
```
