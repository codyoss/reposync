// Copyright 2016 Google Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/time/rate"

	"google.golang.org/appengine"

	"cloud.google.com/go/compute/metadata"
)

var mainJob *job

type job struct {
	name string
	from string
	to   string
	dir  string // staging dir

	// Status reporting
	mu            sync.Mutex
	statusTime    time.Time // time status was set
	statusOK      bool      // normal state?
	statusMessage string    // status indicator, suitable for public use
}

func main() {
	var (
		// repo URLs
		from = os.Getenv("FROM_REPO")
		to   = os.Getenv("TO_REPO")
	)
	if from == "" || to == "" {
		log.Fatalf("FROM_REPO and TO_REPO must be set.")
	}

	// If prefixed with "metadata:", pull it from the GCE metadata server.
	reconcile := func(s string) string {
		if !strings.HasPrefix(s, "metadata:") {
			return s
		}
		val, err := metadata.ProjectAttributeValue(s[len("metadata:"):])
		if err != nil {
			log.Fatalf("Could not get project metadata value %q: %v", s, err)
		}
		return val
	}
	from = reconcile(from)
	to = reconcile(to)

	mainJob = &job{from: from, to: to, dir: "repo", statusOK: true}
	go mainJob.mirror()

	http.HandleFunc("/status", statusz)

	appengine.Main()
}

func (j *job) mirror() {
	j.ok("Cloning")

	for {
		cmd := exec.Command("git", "clone", j.from, j.dir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			j.ok("Cloned", out)
			break
		}
		j.statusErr("Cloning", err, out)
		os.RemoveAll(j.dir)
		time.Sleep(10 * time.Second)
		continue
	}

	for {
		j.ok("Setting remote")
		cmd := exec.Command("git", "remote", "add", "to", j.to)
		cmd.Dir = j.dir
		out, err := cmd.CombinedOutput()
		if err == nil {
			j.ok("Added remote", out)
			break
		}
		j.statusErr("Adding remote", err, out)
		time.Sleep(time.Second)
	}

	limit := rate.NewLimiter(rate.Every(2*time.Minute), 1)

	var oldSHA string

	for {
		ctx := context.Background()
		limit.Wait(ctx)

		log.Printf("Pulling")
		cmd := exec.Command("git", "pull") // TODO: CommandContext once Flex is on 1.7
		cmd.Dir = j.dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			j.statusErr("Pull", err, out)
			continue
		}
		log.Printf("Pulled: %s", out)

		sha, err := ioutil.ReadFile(j.dir + "/.git/refs/heads/master")
		if err != nil {
			j.statusErr("parse HEAD", err)
			continue
		}

		if string(sha) == oldSHA {
			j.ok("Synced - nothing to push: " + oldSHA)
			continue
		}

		log.Printf("Pushing")
		cmd = exec.CommandContext(ctx, "git", "push", "--all", "to")
		cmd.Dir = j.dir
		out, err = cmd.CombinedOutput()
		if err != nil {
			j.statusErr("Push", err, out)
			continue
		}

		log.Printf("Pushing tags")
		cmd = exec.CommandContext(ctx, "git", "push", "--tags", "to")
		cmd.Dir = j.dir
		out, err = cmd.CombinedOutput()
		if err != nil {
			j.statusErr("Push tags", err, out)
			continue
		}

		j.ok("Synced - pushed", out)
		oldSHA = string(sha)
	}
}

func statusz(w http.ResponseWriter, r *http.Request) {
	mainJob.mu.Lock()
	defer mainJob.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	if time.Now().After(mainJob.statusTime.Add(15 * time.Minute)) {
		// Stale. Why? Stalled pull?
		w.WriteHeader(500)
		fmt.Fprintln(w, "Possibly not fresh")
	}
	if !mainJob.statusOK {
		// Use a 500 for the status to indicate bad health.
		w.WriteHeader(500)
	}
	fmt.Fprintln(w, "OK", mainJob.statusOK)
	fmt.Fprintln(w, mainJob.statusTime)
	fmt.Fprintln(w, mainJob.statusMessage)
}

func (j *job) ok(msg string, v ...interface{}) {
	j.status(true, msg, v...)
}

func (j *job) statusErr(msg string, v ...interface{}) {
	j.status(false, msg, v...)
}

func (j *job) status(ok bool, msg string, v ...interface{}) {
	j.mu.Lock()

	j.statusOK = ok
	j.statusMessage = msg
	j.statusTime = time.Now()

	j.mu.Unlock()

	// Log potentially sensitive output.

	buf := &bytes.Buffer{}
	fmt.Fprintln(buf, msg)
	for _, vv := range v {
		switch vv.(type) {
		case []byte:
			fmt.Fprintf(buf, "%s\n", vv)
		default:
			fmt.Fprintf(buf, "%v\n", vv)
		}
	}

	b := buf.Bytes()

	// Redact the from/to, just in case there are secrets in the URL (e.g., GitHub token)
	b = bytes.Replace(b, []byte(j.from), []byte("<REDACTED (FROM)>"), -1)
	b = bytes.Replace(b, []byte(j.to), []byte("<REDACTED (TO)>"), -1)

	if ok {
		log.Printf("OK: %s", b)
	} else {
		log.Printf("FAIL: %s", b)
	}
}
