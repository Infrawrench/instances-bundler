package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const usage = `instances-bundler compresses AWS price list files (EC2, RDS, ElastiCache, ...) into one small history file per offer.

Usage:
  instances-bundler compress [-host-suffix .host] -o <bundle.json> <aws-pricing.json|->
  instances-bundler append   [-host-suffix .host] [-o <out.json>] <bundle.json> <aws-pricing.json|->
  instances-bundler upgrade  [-o <out.json>] <bundle.json>

compress  builds a new bundle from one AWS pricing file.
append    adds a newer AWS pricing file to an existing bundle, recording only what changed.
          The bundle is rewritten in place unless -o is given. Exits with status 3 if the
          file is not newer than the bundle's latest one.
upgrade   rewrites a bundle made by an older version in the current format.

Use - to read the AWS pricing file from stdin.
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
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *out == "" {
		return fmt.Errorf("usage: instances-bundler compress [-host-suffix .host] -o <bundle.json> <aws-pricing.json|->")
	}
	snap, err := loadSnapshot(pos[0], *suffix)
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
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf("usage: instances-bundler append [-host-suffix .host] [-o <out.json>] <bundle.json> <aws-pricing.json|->")
	}
	if *out == "" {
		*out = pos[0]
	}
	b, err := readBundleFile(pos[0])
	if err != nil {
		return err
	}
	snap, err := loadSnapshot(pos[1], *suffix)
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

func loadSnapshot(path, hostSuffix string) (*snapshot, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	start := time.Now()
	snap, err := readAWS(r, hostSuffix)
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
