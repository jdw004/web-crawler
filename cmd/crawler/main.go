package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"web-crawler/crawler"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "crawler:", err)
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	opts := crawler.DefaultOptions()
	fs := flag.NewFlagSet("crawler", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&opts.Workers, "workers", opts.Workers, "maximum concurrent requests")
	fs.IntVar(&opts.MaxDepth, "depth", opts.MaxDepth, "maximum link depth (seeds are depth 0)")
	fs.IntVar(&opts.MaxPages, "max-pages", opts.MaxPages, "maximum distinct requested URLs, including seeds")
	fs.DurationVar(&opts.Timeout, "timeout", opts.Timeout, "timeout per page, including redirects and rate-limit waits")
	fs.DurationVar(&opts.Interval, "interval", opts.Interval, "minimum time between request starts, e.g. 100ms (0 disables)")
	fs.Int64Var(&opts.MaxBodyBytes, "max-body-bytes", opts.MaxBodyBytes, "maximum HTML response size in bytes")
	fs.BoolVar(&opts.AllowExternal, "allow-external", false, "follow links and redirects to other hostnames")
	fs.StringVar(&opts.UserAgent, "user-agent", opts.UserAgent, "HTTP User-Agent header")
	output := fs.String("output", "-", "JSON Lines output file, or - for stdout")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: crawler [flags] URL [URL ...]\n\nFlags must appear before URLs. Results are streamed as JSON Lines.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("provide at least one http:// or https:// seed URL")
	}
	if *output != "-" {
		file, openErr := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, file.Close()) }()
		stdout = file
	}
	encoder := json.NewEncoder(stdout)
	return crawler.Crawl(ctx, fs.Args(), opts, func(r crawler.Result) error {
		return encoder.Encode(r)
	})
}
