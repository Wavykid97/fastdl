// Laneway (formerly FastDL): a multi-connection file downloader.
//
// It splits a file into byte ranges, downloads them in parallel over
// separate HTTP connections, writes each piece straight into its place in
// the output file, and can pause/resume.
//
// Double-click (no arguments)  -> opens the app window.
// Command line                 -> Laneway [-n 8] [-o filename] URL
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	// Started by Chrome/Edge for the Laneway extension: talk over stdin/stdout.
	if isNativeHostLaunch() {
		runNativeHost()
		return
	}
	conns := flag.Int("n", 8, "number of parallel connections")
	out := flag.String("o", "", "output file name (default: taken from the URL)")
	retries := flag.Int("retries", 5, "retries per connection before giving up")
	limit := flag.Float64("limit", 0, "maximum speed in MB/s (0 = no limit)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: Laneway [options] URL\n       Laneway            (no arguments opens the app)\n\nOptions:\n")
		flag.PrintDefaults()
	}
	if len(os.Args) > 1 {
		useParentConsole() // so -h and errors are visible from a terminal
	}
	flag.Parse()

	if flag.NArg() == 0 {
		runGUI()
		return
	}
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := cliUpdateCheck(appDataDir()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	lim := &Limiter{}
	lim.SetRate(int64(*limit * 1024 * 1024))
	d := &Download{URL: flag.Arg(0), Out: *out, Conns: *conns, Retries: *retries, Limit: lim}
	if d.Out == "" {
		d.Out = nameFromURL(d.URL)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; cancel() }()

	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	var last int64
	lastT := time.Now()
	var speed float64
	announced := false
	for {
		select {
		case err := <-done:
			printBar(d, speed)
			fmt.Println()
			switch {
			case err == ErrPaused:
				fmt.Println("Paused. Run the same command again to resume.")
				os.Exit(130)
			case err != nil:
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Done: %s\n", d.Out)
			return
		case now := <-tick.C:
			if !announced && d.Size() > 0 {
				announced = true
				if d.Resumed() {
					fmt.Println("Resuming previous download.")
				}
				fmt.Printf("Downloading %s (%s) with %d connection(s)\n", d.Out, human(d.Size()), d.Conns)
			}
			cur := d.Downloaded()
			inst := float64(cur-last) / now.Sub(lastT).Seconds()
			speed = 0.7*speed + 0.3*inst
			last, lastT = cur, now
			printBar(d, speed)
		}
	}
}

func printBar(d *Download, speed float64) {
	done, size := d.Downloaded(), d.Size()
	if size <= 0 {
		fmt.Printf("\r%s downloaded  %s/s      ", human(done), human(int64(speed)))
		return
	}
	pct := float64(done) / float64(size)
	const width = 30
	filled := int(pct * width)
	bar := strings.Repeat("=", filled) + strings.Repeat(" ", width-filled)
	fmt.Printf("\r[%s] %5.1f%%  %s / %s  %s/s  ETA %s   ",
		bar, pct*100, human(done), human(size), human(int64(speed)), eta(size-done, speed))
}
