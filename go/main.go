// forge: the compiled core of the github-core-clone-01 entry.
//
// Lifecycle (as env/src/wire.ts settles it): bind an ephemeral port, tell
// the env the port it bound, then poll until the env says the trial is over
// and exit 0. This binary is spawned by main.ts, which only forwards signals
// and exits with this process's code.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const requestTimeout = 30 * time.Second
const retryInitial = 250 * time.Millisecond
const retryMax = 2 * time.Second

func retryDelay(attempt int) time.Duration {
	d := retryInitial
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > retryMax {
		return retryMax
	}
	return d
}

// register POSTs the bound port until the env accepts it. 200 is normal;
// 409 means a first register already won. 400/405 are contract errors;
// anything else is retried.
func register(client *http.Client, envURL string, port int) error {
	body, _ := json.Marshal(map[string]any{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done":true}.
func waitForDone(client *http.Client, envURL string) {
	for attempt := 0; ; attempt++ {
		done := func() bool {
			req, err := http.NewRequest("GET", envURL+"/done", nil)
			if err != nil {
				return false
			}
			resp, err := client.Do(req)
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return false
			}
			var decoded struct {
				Done bool `json:"done"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
				return false
			}
			return decoded.Done
		}()
		if done {
			return
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	store := NewStore()
	server := &http.Server{
		Handler:  &forgeHandler{store: store},
		ErrorLog: nil,
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		_ = server.Serve(ln)
	}()
	fmt.Fprintf(os.Stdout, "[entry] listening on port %d\n", port)

	client := &http.Client{Timeout: requestTimeout}
	if err := register(client, envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")

	waitForDone(client, envURL)
	_ = server.Close()
	fmt.Fprintln(os.Stdout, "[entry] done")
}
