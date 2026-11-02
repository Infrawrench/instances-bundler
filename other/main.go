package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const usage = `other-instances keeps the history of instance prices and attributes for providers other than AWS,
in the same bundle format as instances-bundler, one bundle per offer:

  RunpodPods  RunPod GPU and CPU pods (platform runpod)
  AlibabaECS  Alibaba Cloud ECS instance types (platform alibaba)
  AzureVM     Azure virtual machine sizes (platform azure)
  GCPCompute  Google Cloud Compute Engine machine types (platform gcp)

Usage:
  other-instances fetch runpod -o <runpod.json>
  other-instances fetch azure -o <azure.json>
  other-instances fetch gcp -o <gcp.json>
  other-instances fetch alibaba [-version 2.0.<n>|latest] -o <dir>
  other-instances versions alibaba [-after 2.0.<n>]
  other-instances compress -o <bundle.json> <source>
  other-instances append [-o <out.json>] <bundle.json> <source>
  other-instances upgrade [-o <out.json>] <bundle.json>

fetch     saves the provider's current prices as a source: a file for RunPod, Azure and GCP,
          a directory for Alibaba. GCP needs credentials: see gcpauth.go.
versions  lists Alibaba's published price versions after -after (default: all of them), oldest first.
compress  builds a new bundle from one source.
append    adds a newer source to a bundle, recording only what changed. The bundle is rewritten in
          place unless -o is given. Exits with status 3 if there is nothing to add: the source is not
          newer than the bundle's latest snapshot, or (for RunPod, Azure and GCP) nothing changed since it.
upgrade   rewrites a bundle in the current format.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "fetch":
		err = runFetch(os.Args[2:])
	case "versions":
		err = runVersions(os.Args[2:])
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
		if errors.Is(err, errNothingNew) {
			os.Exit(3)
		}
		os.Exit(1)
	}
}

var httpClient = &http.Client{Timeout: 5 * time.Minute}

func runFetch(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: other-instances fetch <runpod|alibaba|azure|gcp> ...")
	}
	provider := args[0]
	fs := flag.NewFlagSet("fetch "+provider, flag.ContinueOnError)
	out := fs.String("o", "", "where to save the source (required)")
	version := fs.String("version", "latest", "Alibaba price version to fetch, such as 2.0.426")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 || *out == "" {
		return fmt.Errorf("usage: other-instances fetch runpod|azure|gcp -o <file> | fetch alibaba [-version 2.0.<n>|latest] -o <dir>")
	}
	ctx := context.Background()
	switch provider {
	case "runpod":
		src, err := fetchRunpod(ctx, httpClient, runpodGraphQL)
		if err != nil {
			return err
		}
		return writeAtomic(*out, func(w *bufio.Writer) error { return writeJSON(w, src) })
	case "azure":
		src, err := fetchAzure(ctx, httpClient, azureRetailURL, azureCalculatorURL)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "fetched %d Azure prices and %d calculator sizes\n", len(src.Items), len(src.Specs))
		return writeAtomic(*out, func(w *bufio.Writer) error { return writeJSON(w, src) })
	case "gcp":
		creds, err := gcpCredentials(ctx, httpClient)
		if err != nil {
			return err
		}
		src, err := fetchGCP(ctx, httpClient, gcpBillingURL, gcpComputeURL, creds)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "fetched %d Compute Engine SKUs and %d machine types (by zone) from project %s\n", len(src.Skus), len(src.MachineTypes), creds.Project)
		return writeAtomic(*out, func(w *bufio.Writer) error { return writeJSON(w, src) })
	case "alibaba":
		v := *version
		if v == "latest" {
			found, err := alibabaVersions(ctx, httpClient, alibabaBase, "", 10)
			if err != nil {
				return err
			}
			if len(found) == 0 {
				return fmt.Errorf("found no published Alibaba price versions")
			}
			v = found[len(found)-1]
		} else if _, err := alibabaPatch(v); err != nil {
			return err
		}
		if err := fetchAlibaba(ctx, httpClient, alibabaBase, v, *out); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "fetched Alibaba price version %s into %s\n", v, *out)
		return nil
	default:
		return fmt.Errorf("unknown provider %q; fetch supports runpod, alibaba, azure and gcp", provider)
	}
}

func runVersions(args []string) error {
	if len(args) == 0 || args[0] != "alibaba" {
		return fmt.Errorf("usage: other-instances versions alibaba [-after 2.0.<n>]")
	}
	fs := flag.NewFlagSet("versions alibaba", flag.ContinueOnError)
	after := fs.String("after", "", "only list versions after this one")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return fmt.Errorf("usage: other-instances versions alibaba [-after 2.0.<n>]")
	}
	found, err := alibabaVersions(context.Background(), httpClient, alibabaBase, *after, 10)
	if err != nil {
		return err
	}
	for _, v := range found {
		fmt.Println(v)
	}
	return nil
}

func runCompress(args []string) error {
	fs := flag.NewFlagSet("compress", flag.ContinueOnError)
	out := fs.String("o", "", "output bundle file (required)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *out == "" {
		return fmt.Errorf("usage: other-instances compress -o <bundle.json> <source>")
	}
	snap, err := loadSource(pos[0])
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
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf("usage: other-instances append [-o <out.json>] <bundle.json> <source>")
	}
	if *out == "" {
		*out = pos[0]
	}
	b, err := readBundleFile(pos[0])
	if err != nil {
		return err
	}
	snap, err := loadSource(pos[1])
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
		return fmt.Errorf("usage: other-instances upgrade [-o <out.json>] <bundle.json>")
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

// loadSource reads a fetched source: a directory of Alibaba price files, or a RunPod, Azure or GCP file.
func loadSource(path string) (*snapshot, error) {
	start := time.Now()
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var snap *snapshot
	switch {
	case info.IsDir() && isAlibabaSource(path):
		snap, err = readAlibaba(path)
	case info.IsDir():
		err = fmt.Errorf("directory has no %s", aliPriceFile)
	default:
		var f *os.File
		if f, err = os.Open(path); err != nil {
			return nil, err
		}
		defer f.Close()
		r := bufio.NewReaderSize(f, 1<<20)
		head, _ := r.Peek(256)
		switch sourceKind(head) {
		case runpodSourceKind:
			snap, err = readRunpod(r)
		case azureSourceKind:
			snap, err = readAzure(r)
		case gcpSourceKind:
			snap, err = readGCP(r)
		default:
			return nil, fmt.Errorf("%s: not a source; use other-instances fetch to make one", path)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	version := ""
	if snap.Version != "" {
		version = " version " + snap.Version + ","
	}
	fmt.Fprintf(os.Stderr, "read %s: %s,%s %d products, %d instance types, published %s (%s)\n",
		path, snap.Offer, version, snap.Products, len(snap.Instances), fmtTS(snap.TS), time.Since(start).Round(time.Millisecond))
	if snap.Collisions > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d prices shared a key with another product and only one was kept\n", snap.Collisions)
	}
	if snap.Skipped > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d products were left out because the bundle has no variant for them\n", snap.Skipped)
	}
	if snap.Unpriced > 0 {
		fmt.Fprintf(os.Stderr, "note: %d instance types have no price in any region and were left out\n", snap.Unpriced)
	}
	if snap.Unknown > 0 {
		fmt.Fprintf(os.Stderr, "note: %d prices were errors in the source (such as Throttling); the bundle keeps their last known price\n", snap.Unknown)
	}
	return snap, nil
}

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

func report(st AppendStats) {
	fmt.Fprintf(os.Stderr, "instances: %d created, %d restored, %d deleted; rows added: %d attribute, %d on-demand, %d reserved; %d region attributes set\n",
		st.Created, st.Restored, st.Deleted, st.AttrChanges, st.ODRows, st.RSRows, st.RegionChanges)
}

func writeBundle(path string, b *Bundle) error {
	if err := writeAtomic(path, func(w *bufio.Writer) error { return b.Write(w) }); err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", path, info.Size())
	}
	return nil
}

func writeJSON(w *bufio.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// writeAtomic writes to a temp file next to path and renames it over, so a failed write never leaves a broken file.
func writeAtomic(path string, write func(*bufio.Writer) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".other-instances-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 1<<20)
	if err := write(w); err != nil {
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
	return os.Rename(tmp.Name(), path)
}
