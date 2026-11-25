// Command aio is the AIO Toolkit server: a plugin-driven web toolbox.
package main

import (
	"context"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // so TZ works for plugin schedules without tzdata in the image

	"aio/internal/automation"
	"aio/internal/community"
	"aio/internal/events"
	"aio/internal/jobs"
	"aio/internal/plugin"
	"aio/internal/server"
	"aio/internal/services"
	"aio/internal/shares"
	"aio/web"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("aio: ")

	addr := env("AIO_ADDR", ":"+env("PORT", "8080"))
	dataDir := env("AIO_DATA_DIR", "./data")
	pluginDirs := filepath.SplitList(env("AIO_PLUGIN_DIRS", "./plugins"))
	downloads := env("AIO_DOWNLOADS_DIR", filepath.Join(dataDir, "downloads"))
	sdkDir, _ := filepath.Abs(env("AIO_SDK_DIR", "./sdk/python"))
	for i, d := range pluginDirs {
		if abs, err := filepath.Abs(d); err == nil {
			pluginDirs[i] = abs
		}
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	os.MkdirAll(downloads, 0o755)

	// Community plugins live in their own root, between the built-ins and
	// the user's local folder: built-in < community < local overrides.
	communityOn := !strings.EqualFold(os.Getenv("AIO_COMMUNITY"), "off")
	communityDir := os.Getenv("AIO_COMMUNITY_DIR")
	if communityDir == "" {
		if last := pluginDirs[len(pluginDirs)-1]; len(pluginDirs) > 1 {
			communityDir = filepath.Join(last, "community")
		} else {
			communityDir = filepath.Join(dataDir, "community-plugins")
		}
	}
	if abs, err := filepath.Abs(communityDir); err == nil {
		communityDir = abs
	}
	roots := pluginDirs
	if communityOn {
		if len(pluginDirs) > 1 {
			roots = append(append(append([]string{}, pluginDirs[:len(pluginDirs)-1]...), communityDir), pluginDirs[len(pluginDirs)-1])
		} else {
			roots = append(append([]string{}, pluginDirs...), communityDir)
		}
	}

	bus := events.New()
	reg := plugin.NewRegistry(roots)

	mgr, err := jobs.NewManager(jobs.Config{
		DataDir:      dataDir,
		DownloadsDir: downloads,
		SDKDir:       sdkDir,
		MaxJobs:      envInt("AIO_MAX_JOBS", 2),
		TTL:          envDur("AIO_JOB_TTL", 24*time.Hour),
		APIURL:       env("AIO_INTERNAL_URL", internalURL(addr)),
		Events:       bus,
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var comm *community.Manager
	checkEvery := envDur("AIO_COMMUNITY_CHECK", 6*time.Hour)
	if communityOn {
		comm, err = community.NewManager(community.Config{
			Dir:      communityDir,
			MaxBytes: int64(envInt("AIO_COMMUNITY_MAX_MB", 200)) << 20,
			Interval: checkEvery,
		}, reg, mgr, bus)
		if err != nil {
			log.Fatal(err)
		}
		for kind, key := range map[community.Kind]string{community.GitHub: "AIO_GITHUB_TOKEN", community.GitLab: "AIO_GITLAB_TOKEN", community.Gitea: "AIO_GITEA_TOKEN"} {
			if v := os.Getenv(key); v != "" {
				comm.Fetcher.Tokens[kind] = v
			}
		}
		go comm.Run(ctx)
	}

	sup := &services.Supervisor{Jobs: mgr, Events: bus}
	sup.Start(ctx)
	auto := &automation.Runner{Jobs: mgr, Events: bus}
	auto.Start(ctx)
	reg.OnReload(sup.Sync)
	reg.OnReload(auto.Sync)
	go reg.Watch(envDur("AIO_PLUGIN_POLL", 5*time.Second))

	shareMgr, err := shares.NewManager(dataDir)
	if err != nil {
		log.Fatal(err)
	}

	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		log.Fatal(err)
	}
	srv := &server.Server{
		Registry:  reg,
		Jobs:      mgr,
		Static:    static,
		MaxUpload: int64(envInt("AIO_MAX_UPLOAD_MB", 4096)) << 20,
		Version:   version,

		Shares:         shareMgr,
		ShareMaxUpload: int64(envInt("AIO_SHARE_MAX_MB", 2048)) << 20,
		PublicURL:      os.Getenv("AIO_PUBLIC_URL"),

		Community:         comm,
		CommunityInterval: checkEvery,
		Services:          sup,
		Events:            bus,
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go shareMgr.RunCleanup(ctx, envDur("AIO_SHARE_CLEANUP", 10*time.Minute))
	go func() {
		log.Printf("listening on %s (plugins: %s, data: %s)", addr, strings.Join(roots, ", "), dataDir)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	log.Print("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.Shutdown(sctx)
	time.Sleep(500 * time.Millisecond) // let services receive SIGTERM
}

// internalURL is how plugins reach this server from inside the container.
func internalURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
