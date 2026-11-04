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

// Datasets live under <prefix>/data/ and are shared by all backends and rounds:
//
//	<prefix>/data/read/<size>/<i>                read, range, open and strided sources (benchCase.sources() each)
//	<prefix>/data/list-<n>/<dir>/<i>             n empty objects spread over listDirs(n) directories
//
// Written objects go to <prefix>/write/... and are deleted by the process that wrote them.
// setup and cleanup always use the pure-Go JSON client, never a client under test.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// datasetCount is the number of source objects per read size: enough for the concurrency the
// profiles use, so concurrent workers read different objects.
func datasetCount(size int64) int {
	switch {
	case size <= 1*mib:
		return 16
	case size <= 64*mib:
		return 8
	default:
		return 4
	}
}

func readObjectName(prefix string, size int64, i int) string {
	return fmt.Sprintf("%s/data/read/%s/%03d", prefix, humanBytes(size), i)
}

func listPrefix(prefix string, n int) string { return fmt.Sprintf("%s/data/list-%d/", prefix, n) }

// listDirs is the number of directories (delimiter prefixes) of the n-object list dataset.
func listDirs(n int) int { return min(100, n) }

func listObjectName(prefix string, n, i int) string {
	return fmt.Sprintf("%s%03d/%06d", listPrefix(prefix, n), i%listDirs(n), i)
}

// datasetPlan returns every dataset object a profile needs, with its size.
func datasetPlan(prefix string, cases []benchCase) map[string]int64 {
	plan := map[string]int64{}
	for _, c := range cases {
		switch c.Workload {
		case wRead, wRange, wOpen, wStrided:
			for i := range c.sources() {
				plan[readObjectName(prefix, c.Size, i)] = c.Size
			}
		case wList:
			for i := range c.Objects {
				plan[listObjectName(prefix, c.Objects, i)] = 0
			}
		}
	}
	return plan
}

func checkPrefix(prefix string) error {
	if strings.Trim(prefix, "/") == "" {
		return errors.New("-prefix must not be empty")
	}
	return nil
}

// setupDatasets creates the missing dataset objects of a profile (idempotent: objects that
// exist with the right size are kept).
func setupDatasets(ctx context.Context, bucket, prefix string, cases []benchCase) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	c, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	bkt := c.Bucket(bucket)
	plan := datasetPlan(prefix, cases)
	existing := map[string]int64{}
	it := bkt.Objects(ctx, &storage.Query{Prefix: prefix + "/data/"})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return fmt.Errorf("listing existing datasets: %w", err)
		}
		existing[a.Name] = a.Size
	}
	var todo []string
	var total, missing int64
	for name, size := range plan {
		total += size
		if s, ok := existing[name]; !ok || s != size {
			todo = append(todo, name)
			missing += size
		}
	}
	sort.Strings(todo)
	log.Printf("datasets under gs://%s/%s/data/: %d objects (%.1f MiB); creating %d (%.1f MiB)",
		bucket, prefix, len(plan), float64(total)/mib, len(todo), float64(missing)/mib)
	if len(todo) == 0 {
		return nil
	}
	var maxSize int64
	for _, n := range todo {
		maxSize = max(maxSize, plan[n])
	}
	data := randomBytes(int(maxSize), 2)
	start := time.Now()
	failed := parallelDo(ctx, len(todo), 64, func(ctx context.Context, i int) error {
		name := todo[i]
		w := bkt.Object(name).NewWriter(ctx)
		if _, err := w.Write(data[:plan[name]]); err != nil {
			w.Close()
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	})
	if failed > 0 {
		return fmt.Errorf("could not create %d dataset objects", failed)
	}
	log.Printf("created %d dataset objects in %v", len(todo), time.Since(start).Round(time.Second))
	return nil
}

// cleanupPrefix deletes every object under prefix/ (datasets and leftovers of interrupted runs).
func cleanupPrefix(ctx context.Context, bucket, prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	c, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	bkt := c.Bucket(bucket)
	var names []string
	it := bkt.Objects(ctx, &storage.Query{Prefix: strings.TrimSuffix(prefix, "/") + "/"})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return err
		}
		names = append(names, a.Name)
	}
	log.Printf("deleting %d objects under gs://%s/%s/", len(names), bucket, prefix)
	failed := parallelDo(ctx, len(names), 64, func(ctx context.Context, i int) error {
		err := bkt.Object(names[i]).Delete(ctx)
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil
		}
		return err
	})
	if failed > 0 {
		return fmt.Errorf("could not delete %d objects", failed)
	}
	return nil
}
