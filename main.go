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

const dir = "repo"

var (
	// repo URLs
	from = os.Getenv("FROM_REPO")
	to   = os.Getenv("TO_REPO")
)

var (
	statusMu   sync.Mutex
	statusTime time.Time // time status was set
	statusOK   = true    // normal state?
	statusText []byte    // current status
)

func main() {
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

	go mirror()

	http.HandleFunc("/status", statusz)

	appengine.Main()
}

func mirror() {
	ok("Cloning")

	for {
		cmd := exec.Command("git", "clone", from, dir)
		out, err := cmd.CombinedOutput()
		if err == nil {
			ok("Cloned", out)
			break
		}
		statusErr("Cloning", err, out)
		os.RemoveAll(dir)
		time.Sleep(10 * time.Second)
		continue
	}

	for {
		ok("Setting remote")
		cmd := exec.Command("git", "remote", "add", "to", to)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err == nil {
			ok("Added remote", out)
			break
		}
		statusErr("Adding remote", err, out)
		time.Sleep(time.Second)
	}

	limit := rate.NewLimiter(rate.Every(2*time.Minute), 1)

	var oldSHA string

	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		limit.Wait(ctx)

		log.Printf("Pulling")
		cmd := exec.Command("git", "pull") // TODO: CommandContext once Flex is on 1.7
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			statusErr("Pull", err, out)
			continue
		}
		log.Printf("Pulled: %s", out)

		sha, err := ioutil.ReadFile(dir + "/.git/refs/heads/master")
		if err != nil {
			statusErr("parse HEAD", err)
			continue
		}

		if string(sha) == oldSHA {
			log.Printf("Nothing to push")
			continue
		}
		oldSHA = string(sha)

		log.Printf("Pushing")
		cmd = exec.Command("git", "push", "--all", "to") // TODO: CommandContext once Flex is on 1.7
		cmd.Dir = dir
		out, err = cmd.CombinedOutput()
		if err != nil {
			statusErr("Push", err, out)
			continue
		}

		ok("Synced", out)
	}
}

func statusz(w http.ResponseWriter, r *http.Request) {
	statusMu.Lock()
	defer statusMu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	if !statusOK {
		// Use a 500 for the status to indicate bad health.
		w.WriteHeader(500)
	}
	fmt.Fprintln(w, "OK", statusOK)
	fmt.Fprintln(w, statusTime)
	w.Write(statusText)
}

func ok(msg string, v ...interface{}) {
	status(true, msg, v...)
}

func statusErr(msg string, v ...interface{}) {
	status(false, msg, v...)
}

func status(ok bool, msg string, v ...interface{}) {
	statusMu.Lock()
	defer statusMu.Unlock()

	statusOK = ok
	statusTime = time.Now()

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

	statusText = buf.Bytes()

	// Redact the from/to, just in case there are secrets in the URL (e.g., GitHub token)
	statusText = bytes.Replace(statusText, []byte(from), []byte("<REDACTED (FROM)>"), -1)
	statusText = bytes.Replace(statusText, []byte(to), []byte("<REDACTED (TO)>"), -1)

	if ok {
		log.Printf("OK: %s", statusText)
	} else {
		log.Printf("FAIL: %s", statusText)
	}
}
