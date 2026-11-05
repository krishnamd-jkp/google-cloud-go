// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command rustbench benchmarks the Go SDK's own transports against the shared Rust core
// (experimental.WithRustCore) on the data-path APIs: object writes (WriteObject), full and
// ranged reads (ReadObject) and listing (ListObjects), over the JSON API and gRPC (CloudPath;
// the Rust SDK has no DirectPath).
//
// Build (cgo, linux/amd64; build the Rust core first with ../../build.sh):
//
//	go build -tags gcsrust -o rustbench ./internal/rustcore/cmd/rustbench
//
// Subcommands:
//
//	rustbench matrix                                   backends x workloads, skips and reasons
//	rustbench setup   -bucket B -prefix P              create the read and list datasets
//	rustbench suite   -bucket B -prefix P -out F.jsonl run the whole profile (see below)
//	rustbench run     -bucket B -prefix P -backend X -case NAME   one case in this process
//	rustbench cleanup -bucket B -prefix P              delete every object under the prefix
//
// suite and run also take -zonal-bucket (appendable uploads, the "append" workload) and
// -project (the "list-buckets" workload; default: the VM's project).
//
// suite runs every (case, backend) pair of a profile in its own child process (`rustbench run`),
// in a new random order every round, and appends one JSON line per pair and round to -out.
// CPU time and memory are per process, so they cover everything a client does (Go and Rust
// threads alike). Summarize the results with ../rustbench-report. To run on a GCE VM, see
// ../../bench/README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage: rustbench <matrix|setup|suite|run|cleanup> [flags]

  matrix    print the backend x workload matrix (with the reasons for skipped pairs)
  setup     create the datasets of a profile under -prefix (idempotent)
  suite     run every (case, backend) pair of a profile, one child process each, -rounds times
  run       run one case with one backend in this process and print a JSON result line
  cleanup   delete every object under -prefix

Run "rustbench <subcommand> -h" for the flags of a subcommand.
`)
	os.Exit(2)
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("rustbench: ")
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "matrix":
		fs := flag.NewFlagSet("matrix", flag.ExitOnError)
		profile := fs.String("profile", "default", "profile: "+strings.Join(profileNames(), " | "))
		fs.Parse(args)
		printMatrix(os.Stdout, *profile)
	case "setup":
		fs := flag.NewFlagSet("setup", flag.ExitOnError)
		bucket, prefix := bucketFlags(fs)
		profile := fs.String("profile", "default", "profile whose datasets to create")
		fs.Parse(args)
		if err := setupDatasets(ctx, mustBucket(*bucket), *prefix, mustProfile(*profile)); err != nil {
			log.Fatal(err)
		}
	case "cleanup":
		fs := flag.NewFlagSet("cleanup", flag.ExitOnError)
		bucket, prefix := bucketFlags(fs)
		fs.Parse(args)
		if err := cleanupPrefix(ctx, mustBucket(*bucket), *prefix); err != nil {
			log.Fatal(err)
		}
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		tf := targetFlags(fs)
		backend := fs.String("backend", "", "backend name (see `rustbench matrix`)")
		caseName := fs.String("case", "", "case name from -profile (or use -case-json)")
		caseJSON := fs.String("case-json", "", "the case as JSON (used by suite)")
		profile := fs.String("profile", "default", "profile containing -case")
		runID := fs.String("run-id", "", "run id recorded in the result")
		round := fs.Int("round", 0, "round number recorded in the result")
		fs.Parse(args)
		c, err := resolveCase(*caseName, *caseJSON, *profile)
		if err != nil {
			log.Fatal(err)
		}
		res, err := runCase(ctx, tf.resolve(ctx), *backend, c)
		if err != nil {
			log.Fatalf("%s/%s: %v", c.Name, *backend, err)
		}
		res.RunID, res.Round = *runID, *round
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
			log.Fatal(err)
		}
	case "suite":
		fs := flag.NewFlagSet("suite", flag.ExitOnError)
		tf := targetFlags(fs)
		var o suiteOptions
		fs.StringVar(&o.profile, "profile", "default", "profile: "+strings.Join(profileNames(), " | "))
		fs.IntVar(&o.rounds, "rounds", 5, "rounds; every round runs every pair once, in a new random order")
		fs.StringVar(&o.out, "out", "results.jsonl", "JSON-lines output file (appended to)")
		fs.StringVar(&o.backends, "backends", "", "comma-separated backends (default: all)")
		fs.StringVar(&o.cases, "cases", "", "comma-separated case names (default: all in the profile)")
		fs.StringVar(&o.runID, "run-id", time.Now().UTC().Format("20060102T150405Z"), "run id recorded in every result")
		fs.Int64Var(&o.seed, "seed", time.Now().UnixNano(), "shuffle seed (recorded in the log)")
		fs.BoolVar(&o.setup, "setup", true, "create the datasets first (idempotent)")
		fs.DurationVar(&o.timeout, "timeout", 30*time.Minute, "timeout per child process")
		fs.DurationVar(&o.cooldown, "cooldown", 2*time.Second, "pause between child processes")
		fs.Parse(args)
		o.target = tf.resolve(ctx)
		if err := runSuite(ctx, o); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
	}
}

func bucketFlags(fs *flag.FlagSet) (bucket, prefix *string) {
	bucket = fs.String("bucket", os.Getenv("RUSTBENCH_BUCKET"), "bucket (default $RUSTBENCH_BUCKET)")
	prefix = fs.String("prefix", "rustbench", "object name prefix for datasets and written objects")
	return bucket, prefix
}

type targetFlagValues struct {
	bucket, prefix, zonalBucket, project *string
}

// targetFlags adds the flags of a target: the bucket and prefix, the zonal bucket of appendable
// uploads and the project of bucket listing.
func targetFlags(fs *flag.FlagSet) targetFlagValues {
	var v targetFlagValues
	v.bucket, v.prefix = bucketFlags(fs)
	v.zonalBucket = fs.String("zonal-bucket", os.Getenv("RUSTBENCH_ZONAL_BUCKET"),
		"zonal bucket for appendable uploads (default $RUSTBENCH_ZONAL_BUCKET)")
	v.project = fs.String("project", os.Getenv("RUSTBENCH_PROJECT"),
		"project whose buckets list-buckets lists (default $RUSTBENCH_PROJECT, else the VM's project)")
	return v
}

func (v targetFlagValues) resolve(ctx context.Context) target {
	t := target{bucket: mustBucket(*v.bucket), prefix: *v.prefix, zonalBucket: *v.zonalBucket, project: *v.project}
	if t.project == "" && metadata.OnGCE() {
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		t.project, _ = metadata.ProjectIDWithContext(mctx)
	}
	return t
}

func mustBucket(b string) string {
	if b == "" {
		log.Fatal("-bucket (or $RUSTBENCH_BUCKET) is required")
	}
	return b
}

func mustProfile(name string) []benchCase {
	p, ok := profiles[name]
	if !ok {
		log.Fatalf("unknown profile %q (have %s)", name, strings.Join(profileNames(), ", "))
	}
	return p
}

func resolveCase(name, js, profile string) (benchCase, error) {
	if js != "" {
		var c benchCase
		err := json.Unmarshal([]byte(js), &c)
		return c, err
	}
	for _, c := range mustProfile(profile) {
		if c.Name == name {
			return c, nil
		}
	}
	return benchCase{}, fmt.Errorf("no case %q in profile %q (see `rustbench matrix -profile %s`)", name, profile, profile)
}
