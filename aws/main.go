package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usage = `instances-bundler compresses AWS price list files (EC2, RDS, ElastiCache, ...) and EC2 spot price
feeds into one small history file per offer.

Usage:
  instances-bundler compress [-host-suffix .host] [-published <time>] -o <bundle.json> <aws-pricing.json|->
  instances-bundler append   [-host-suffix .host] [-published <time>] [-o <out.json>] <bundle.json> <aws-pricing.json|->
  instances-bundler upgrade  [-o <out.json>] <bundle.json>
  instances-bundler spot-history [-aggregate min] [-interval 1h] [-published <time>] -start <time> -o <bundle.json> <spot.json> <history-dir>

compress  builds a new bundle from one AWS pricing file.
append    adds a newer AWS pricing file to an existing bundle, recording only what changed.
          The bundle is rewritten in place unless -o is given. Exits with status 3 if the
          file is not newer than the bundle's latest one.
upgrade   rewrites a bundle made by an older version in the current format.
spot-history  builds a new spot bundle from EC2's spot price history, as seed-spot.sh saves it in
          history-dir, from -start up to when spot.json was published, then adds spot.json itself.

Use - to read the AWS pricing file from stdin.

A spot price feed (spot.json) has no publication date, so -published gives it as an RFC 3339 time.
Without -published, the feed file's modification time is used.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "compress":
		err = runCompress(os.Args[2:])
	case "append":
		err = runAppend(os.Args[2:])
	case "upgrade":
		err = runUpgrade(os.Args[2:])
	case "spot-history":
		err = runSpotHistory(os.Args[2:])
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, errNotNewer) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

func runCompress(args []string) error {
	fs := flag.NewFlagSet("compress", flag.ContinueOnError)
	out := fs.String("o", "", "output bundle file (required)")
	suffix := fs.String("host-suffix", ".host", "suffix added to Dedicated Host names so they don't collide with instance types")
	published := fs.String("published", "", "when a spot price feed was published, as an RFC 3339 time (default: the file's modification time)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *out == "" {
		return fmt.Errorf("usage: instances-bundler compress [-host-suffix .host] [-published <time>] -o <bundle.json> <aws-pricing.json|->")
	}
	snap, err := loadSnapshot(pos[0], *suffix, *published)
	if err != nil {
		return err
	}
	b := &Bundle{Instances: map[string]*Instance{}}
	st, err := b.Append(snap)
	if err != nil {
		return err
	}
	report(st)
	return writeBundle(*out, b)
}

func runAppend(args []string) error {
	fs := flag.NewFlagSet("append", flag.ContinueOnError)
	out := fs.String("o", "", "output bundle file (default: rewrite the input bundle)")
	suffix := fs.String("host-suffix", ".host", "suffix added to Dedicated Host names; use the one the bundle was made with")
	published := fs.String("published", "", "when a spot price feed was published, as an RFC 3339 time (default: the file's modification time)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf("usage: instances-bundler append [-host-suffix .host] [-published <time>] [-o <out.json>] <bundle.json> <aws-pricing.json|->")
	}
	if *out == "" {
		*out = pos[0]
	}
	b, err := readBundleFile(pos[0])
	if err != nil {
		return err
	}
	snap, err := loadSnapshot(pos[1], *suffix, *published)
	if err != nil {
		return err
	}
	st, err := b.Append(snap)
	if err != nil {
		return err
	}
	report(st)
	return writeBundle(*out, b)
}

func runUpgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	out := fs.String("o", "", "output bundle file (default: rewrite the input bundle)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: instances-bundler upgrade [-o <out.json>] <bundle.json>")
	}
	if *out == "" {
		*out = pos[0]
	}
	b, err := readBundleFile(pos[0])
	if err != nil {
		return err
	}
	return writeBundle(*out, b)
}

func runSpotHistory(args []string) error {
	fs := flag.NewFlagSet("spot-history", flag.ContinueOnError)
	out := fs.String("o", "", "output bundle file (required); must not exist yet")
	startFlag := fs.String("start", "", "when the history starts, as an RFC 3339 time (required)")
	published := fs.String("published", "", "when spot.json was published, as an RFC 3339 time (default: the file's modification time)")
	aggregate := fs.String("aggregate", "min", "how availability zone prices become a region price: "+strings.Join(spotAggregateNames, ", "))
	interval := fs.Duration("interval", time.Hour, "at most one snapshot per interval; each price row still has its own time")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || *out == "" || *startFlag == "" || *interval < time.Second {
		return fmt.Errorf("usage: instances-bundler spot-history [-aggregate min] [-interval 1h] [-published <time>] -start <time> -o <bundle.json> <spot.json> <history-dir>")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s already exists; spot-history only builds new bundles, since history is older than any bundle's latest prices", *out)
	}
	start, err := parseTS(*startFlag)
	if err != nil {
		return fmt.Errorf("-start: %w", err)
	}
	feed, err := loadSnapshot(pos[0], "", *published)
	if err != nil {
		return err
	}
	if feed.Offer != spotOffer {
		return fmt.Errorf("%s is not a spot price feed", pos[0])
	}
	if feed.TS <= start {
		return fmt.Errorf("-start %s is not before the feed's publication time %s", fmtTS(start), fmtTS(feed.TS))
	}
	began := time.Now()
	h, err := readSpotHistory(pos[1], start, feed.TS, *aggregate)
	if err != nil {
		return err
	}
	if len(h.Events) == 0 {
		return fmt.Errorf("%s has no spot price history between %s and %s", pos[1], fmtTS(start), fmtTS(feed.TS))
	}
	fmt.Fprintf(os.Stderr, "read %s: %d spot price records, %d region price changes (%s)\n",
		pos[1], h.Records, len(h.Events), time.Since(began).Round(time.Second))
	fmt.Fprint(os.Stderr, compareSpot(h, feed, *aggregate))

	b := &Bundle{Instances: map[string]*Instance{}}
	st, n, err := appendSpotHistory(b, h, start, feed.TS, int64(interval.Seconds()))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "history: %d snapshots from %s, %d instance types, %d price rows\n", n, fmtTS(start), st.Created, st.ODRows)
	if st, err = b.Append(feed); err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "feed: ")
	report(st)
	return writeBundle(*out, b)
}

// readBundleFile reads a bundle, upgrading it in memory if an older version wrote it.
func readBundleFile(path string) (*Bundle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := ReadBundle(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if b.lifted.Rows > 0 {
		fmt.Fprintf(os.Stderr, "upgraded %s: moved %d region attribute rows from instances into a table of %d regions\n",
			path, b.lifted.Rows, len(b.Regions))
	}
	if b.lifted.Conflicts > 0 {
		fmt.Fprintf(os.Stderr, "warning: instances disagreed on %d region attributes; the most common value was kept\n", b.lifted.Conflicts)
	}
	return b, nil
}

// parseArgs lets flags appear before, between or after positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func loadSnapshot(path, hostSuffix, published string) (*snapshot, error) {
	var in *os.File = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		in = f
	}
	start := time.Now()
	r := bufio.NewReaderSize(in, 4<<20)
	head, _ := r.Peek(4096)
	var snap *snapshot
	var err error
	if isSpotFeed(head) {
		var ts int64
		ts, err = spotPublished(in, published)
		if err == nil {
			snap, err = readSpot(r, ts)
		}
	} else if published != "" {
		err = fmt.Errorf("-published is only for spot price feeds; price lists have their own publicationDate")
	} else {
		snap, err = readAWS(r, hostSuffix)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "read %s: %d instance products, %d instance types, published %s (%s)\n",
		path, snap.Products, len(snap.Instances), fmtTS(snap.TS), time.Since(start).Round(time.Second))
	if snap.Collisions > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d prices shared a key with another product and only one was kept\n", snap.Collisions)
	}
	return snap, nil
}

// spotPublished is when a spot price feed was published: the -published time, or else the file's modification time.
func spotPublished(f *os.File, published string) (int64, error) {
	if published != "" {
		ts, err := parseTS(published)
		if err != nil {
			return 0, fmt.Errorf("-published: %w", err)
		}
		return ts, nil
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("a spot price feed read from a pipe needs -published")
	}
	return info.ModTime().Unix(), nil
}

func report(st AppendStats) {
	fmt.Fprintf(os.Stderr, "instances: %d created, %d restored, %d deleted; rows added: %d attribute, %d on-demand, %d reserved; %d region attributes set\n",
		st.Created, st.Restored, st.Deleted, st.AttrChanges, st.ODRows, st.RSRows, st.RegionChanges)
}

// writeBundle writes to a temp file next to path and renames it over, so a failed write never leaves a broken bundle.
func writeBundle(path string, b *Bundle) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instances-bundler-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 1<<20)
	if err := b.Write(w); err != nil {
		tmp.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", path, info.Size())
	}
	return nil
}
