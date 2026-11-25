package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"aio/internal/automation"
	"aio/internal/events"
	"aio/internal/jobs"
	"aio/internal/plugin"
	"aio/internal/services"
)

// TestHelperService is not a real test: the service test runs the test
// binary itself as a plugin service with AIO_HELPER_SERVICE=1.
func TestHelperService(t *testing.T) {
	if os.Getenv("AIO_HELPER_SERVICE") != "1" {
		t.Skip("helper process")
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	http.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s from %s prefix=%s", r.URL.Query().Get("name"), os.Getenv("AIO_PLUGIN_ID"), r.Header.Get("X-Forwarded-Prefix"))
	})
	http.ListenAndServe("127.0.0.1:"+os.Getenv("AIO_SERVICE_PORT"), nil)
	os.Exit(0)
}

type fullEnv struct {
	ts   *httptest.Server
	bus  *events.Bus
	reg  *plugin.Registry
	jobs *jobs.Manager
}

// newFullServer wires jobs, events, automation and services like main does.
func newFullServer(t *testing.T, maxJobs int, setup func(root string)) *fullEnv {
	t.Helper()
	root, data := t.TempDir(), t.TempDir()
	setup(root)
	ts := httptest.NewUnstartedServer(nil)
	ts.Start()
	t.Cleanup(ts.Close)
	bus := events.New()
	mgr, err := jobs.NewManager(jobs.Config{DataDir: data, DownloadsDir: data, MaxJobs: maxJobs, APIURL: ts.URL, Events: bus})
	if err != nil {
		t.Fatal(err)
	}
	reg := plugin.NewRegistry([]string{root})
	ctx, cancel := context.WithCancel(context.Background())
	sup := &services.Supervisor{Jobs: mgr, Events: bus}
	sup.Start(ctx)
	auto := &automation.Runner{Jobs: mgr, Events: bus}
	auto.Start(ctx)
	reg.OnReload(sup.Sync)
	reg.OnReload(auto.Sync)
	t.Cleanup(func() { cancel(); sup.Sync(nil) })
	s := &Server{Registry: reg, Jobs: mgr, Static: os.DirFS(t.TempDir()), MaxUpload: 1 << 20, Services: sup, Events: bus}
	ts.Config.Handler = s.Handler()
	return &fullEnv{ts: ts, bus: bus, reg: reg, jobs: mgr}
}

func waitJob(t *testing.T, ts *httptest.Server, id string) map[string]any {
	t.Helper()
	res, err := http.Get(ts.URL + "/api/jobs/" + id + "?wait=20s")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var j map[string]any
	json.NewDecoder(res.Body).Decode(&j)
	return j
}

// A job calling another plugin must not deadlock when it holds the only slot.
func TestChildJobBypassesQueue(t *testing.T) {
	e := newFullServer(t, 1, func(root string) {
		writePlugin(t, root, "child", "run: [sh, run.sh]\n", `echo child-out > "$AIO_OUT/c.txt"`+"\n")
		writePlugin(t, root, "parent", "run: [sh, run.sh]\n",
			`wget -q -O "$AIO_OUT/child.json" --header "X-AIO-Job-Token: $AIO_JOB_TOKEN" --header "Content-Type: application/json" --post-data '{}' "$AIO_API/api/plugins/child/run?wait=10s"`+"\n")
	})
	res, b := post(t, e.ts.URL+"/api/plugins/parent/run?wait=20s&output=file", "application/json", strings.NewReader("{}"))
	if res.StatusCode != 200 {
		t.Fatalf("parent: %d %s", res.StatusCode, b)
	}
	var child map[string]any
	if err := json.Unmarshal(b, &child); err != nil || child["state"] != "done" {
		t.Fatalf("child job: %s", b)
	}
	if child["parent"] == "" || child["parent"] == nil {
		t.Fatalf("child should record its parent: %s", b)
	}
	// An unknown token is rejected.
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/plugins/child/run", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AIO-Job-Token", "nope")
	r2, _ := http.DefaultClient.Do(req)
	if r2.StatusCode != 400 {
		t.Fatalf("bad token: %d", r2.StatusCode)
	}
}

func TestEventHooksAndStream(t *testing.T) {
	e := newFullServer(t, 2, func(root string) {
		writePlugin(t, root, "src", "run: [sh, run.sh]\n", "echo hi\n")
		// Reacts to src finishing, and to its own custom topic (loop guard).
		writePlugin(t, root, "hook", `
run: [sh, run.sh]
events:
  - {on: job.done, filter: {plugin: src}}
  - {on: hook.ping}
`, `cp "$AIO_EVENT" "$AIO_OUT/event.json"
wget -q -O - --header "X-AIO-Job-Token: $AIO_JOB_TOKEN" --header "Content-Type: application/json" --post-data '{"topic":"hook.ping","data":{"n":1}}' "$AIO_API/api/events"
`)
	})
	// Subscribe over SSE first.
	sse, err := http.Get(e.ts.URL + "/api/events?topic=job.done,hook.*")
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Body.Close()
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(sse.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				lines <- strings.TrimPrefix(sc.Text(), "data: ")
			}
		}
	}()
	res, b := post(t, e.ts.URL+"/api/plugins/src/run?wait=10s", "application/json", strings.NewReader("{}"))
	if res.StatusCode != 200 {
		t.Fatalf("src: %d %s", res.StatusCode, b)
	}
	var hookJob, ping, hookDone bool
	deadline := time.After(15 * time.Second)
	for !(hookJob && ping && hookDone) {
		select {
		case l := <-lines:
			var ev events.Event
			json.Unmarshal([]byte(l), &ev)
			if ev.Topic == "hook.ping" && ev.Data["from_plugin"] == "hook" && ev.Source == "hook" {
				ping = true
			}
			if ev.Topic == "job.done" && ev.Data["plugin"] == "hook" && ev.Data["kind"] == "event" {
				hookDone = true
				hookJob = true
			}
		case <-deadline:
			t.Fatalf("timeout: hookJob=%v ping=%v", hookJob, ping)
		}
	}
	time.Sleep(500 * time.Millisecond)
	// The hook must have run exactly once: its own hook.ping is not fed back to it.
	n := 0
	for _, j := range e.jobs.List() {
		if j.Plugin == "hook" {
			n++
			full := waitJob(t, e.ts, j.ID)
			if full["state"] != "done" {
				t.Fatalf("hook job: %v", full)
			}
		}
	}
	if n != 1 {
		t.Fatalf("hook ran %d times, want 1", n)
	}
	// Reserved topics can't be published.
	r, _ := post(t, e.ts.URL+"/api/events", "application/json", strings.NewReader(`{"topic":"job.done"}`))
	if r.StatusCode != 400 {
		t.Fatalf("reserved topic accepted: %d", r.StatusCode)
	}
}

func TestServiceProxy(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("AIO_HELPER_SERVICE", "1") // inherited by the service process
	e := newFullServer(t, 1, func(root string) {
		writePlugin(t, root, "svc", fmt.Sprintf("service: {run: [%q, -test.run=^TestHelperService$], health: /health}\n", exe), "")
	})
	var body string
	for i := 0; i < 100; i++ {
		res, err := http.Get(e.ts.URL + "/s/svc/hello?name=bob")
		if err == nil {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode == 200 {
				body = string(b)
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if body != "hello bob from svc prefix=/s/svc" {
		t.Fatalf("proxy: %q", body)
	}
	res, err := http.Get(e.ts.URL + "/api/services")
	if err != nil {
		t.Fatal(err)
	}
	var list struct{ Services []services.Status }
	json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if len(list.Services) != 1 || list.Services[0].ID != "svc" {
		t.Fatalf("services: %+v", list)
	}
}
