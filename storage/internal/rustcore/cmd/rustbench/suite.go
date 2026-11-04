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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

type suiteOptions struct {
	bucket, prefix, profile, out string
	backends, cases, runID       string
	rounds                       int
	seed                         int64
	setup                        bool
	timeout, cooldown            time.Duration
}

type pair struct {
	c benchCase
	b backend
}

// runSuite runs every (case, backend) pair of the profile once per round, each in a fresh
// child process, in a new random order every round (so that slow drifts of the network or the
// service spread over all backends instead of biasing one).
func runSuite(ctx context.Context, o suiteOptions) error {
	cases := mustProfile(o.profile)
	if o.cases != "" {
		want := strings.Split(o.cases, ",")
		cases = slices.DeleteFunc(slices.Clone(cases), func(c benchCase) bool { return !slices.Contains(want, c.Name) })
	}
	bes := backends
	if o.backends != "" {
		bes = nil
		for _, name := range strings.Split(o.backends, ",") {
			b, err := lookupBackend(strings.TrimSpace(name))
			if err != nil {
				return err
			}
			bes = append(bes, b)
		}
	}
	var pairs []pair
	for _, c := range cases {
		for _, b := range bes {
			if _, skip := b.skip[c.Workload]; !skip {
				pairs = append(pairs, pair{c, b})
			}
		}
	}
	if len(pairs) == 0 {
		return errors.New("no (case, backend) pairs selected")
	}

	// The Rust SDK has no DirectPath; keep Go's gRPC client on CloudPath too, unless asked.
	if os.Getenv("RUSTBENCH_ALLOW_DIRECTPATH") == "" {
		os.Setenv("GOOGLE_CLOUD_DISABLE_DIRECT_PATH", "true")
	}
	env := collectEnv(ctx, o)
	if env.MachineType == "" {
		log.Printf("warning: not on GCE (or no metadata server): results are not comparable with VM runs")
	}
	if o.setup {
		if err := setupDatasets(ctx, o.bucket, o.prefix, cases); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(o.out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	log.Printf("run %s: %d cases x %d backends = %d pairs, %d rounds, seed %d, machine %q, output %s",
		o.runID, len(cases), len(bes), len(pairs), o.rounds, o.seed, env.MachineType, o.out)
	rng := rand.New(rand.NewPCG(uint64(o.seed), 0x5eed))
	enc := json.NewEncoder(f)
	failures := 0
	for round := 1; round <= o.rounds; round++ {
		for seq, idx := range rng.Perm(len(pairs)) {
			p := pairs[idx]
			res, err := runChild(ctx, exe, o, p, round)
			if err != nil {
				failures++
				log.Printf("[round %d/%d %d/%d] %s %s: FAILED: %v", round, o.rounds, seq+1, len(pairs), p.c.Name, p.b.name, err)
				res = &result{Schema: resultSchema, Time: time.Now().UTC(), Backend: p.b.name, Engine: p.b.engine,
					Transport: p.b.transport, Case: p.c, Failed: true, Error: err.Error()}
			} else {
				log.Printf("[round %d/%d %d/%d] %s %s: %s", round, o.rounds, seq+1, len(pairs), p.c.Name, p.b.name, oneLine(res))
			}
			res.RunID, res.Round, res.Seq, res.Env = o.runID, round, seq+1, env
			if err := enc.Encode(res); err != nil {
				return err
			}
			if err := f.Sync(); err != nil {
				return err
			}
			time.Sleep(o.cooldown)
		}
	}
	log.Printf("run %s done: %d results, %d failed; summarize with: rustbench-report %s", o.runID, len(pairs)*o.rounds, failures, o.out)
	return nil
}

func runChild(ctx context.Context, exe string, o suiteOptions, p pair, round int) (*result, error) {
	cj, err := json.Marshal(p.c)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, exe, "run", "-bucket", o.bucket, "-prefix", o.prefix, "-backend", p.b.name,
		"-case-json", string(cj), "-run-id", o.runID, "-round", strconv.Itoa(round))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("child process: %w", err)
	}
	var res result
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		return nil, fmt.Errorf("decoding the child's result %q: %w", out, err)
	}
	return &res, nil
}

func oneLine(r *result) string {
	var b strings.Builder
	switch {
	case r.Bytes > 0:
		fmt.Fprintf(&b, "%.1f MiB/s", r.MiBPerS)
	case r.Entries > 0:
		fmt.Fprintf(&b, "%.0f entries/s", r.EntriesPerS)
	}
	fmt.Fprintf(&b, ", %.1f ops/s, p50 %.1f ms, p99 %.1f ms, cpu %.2f ms/op", r.OpsPerS, r.LatencyMs.P50, r.LatencyMs.P99, r.CPUMsPerOp)
	if r.CPUSPerGiB > 0 {
		fmt.Fprintf(&b, " (%.2f s/GiB)", r.CPUSPerGiB)
	}
	fmt.Fprintf(&b, ", peak rss %.0f MiB, threads %d", r.RSSPeakMiB, r.ThreadsMax)
	if r.Errors > 0 {
		fmt.Fprintf(&b, ", %d ERRORS (first: %s)", r.Errors, r.FirstError)
	}
	return b.String()
}
