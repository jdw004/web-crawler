# Go Web Crawler

A simple web crawler written in Go. It starts from one or more seed URLs, traverses links according to configurable rules, and collects data such as page titles, status codes, and response times.

## Features

- Concurrent crawling with worker pool
- Configurable crawl depth and domain restrictions
- Optional rate limiting to avoid overloading servers
- Basic HTML parsing for links and metadata
- Pluggable storage layer for results (stdout, JSON file, database, etc)
- Graceful shutdown with context cancellation

### Installation

```bash
git clone https://github.com/your-username/your-repo-name.git
cd your-repo-name

go mod tidy
go build -o go-crawler ./cmd/crawler
