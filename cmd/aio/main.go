// Command aio is the AIO Toolkit server: a plugin-driven web toolbox.
package main

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aio/internal/jobs"
	"aio/internal/plugin"
	"aio/internal/server"
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

	reg := plugin.NewRegistry(pluginDirs)
	go reg.Watch(envDur("AIO_PLUGIN_POLL", 5*time.Second))

	mgr, err := jobs.NewManager(jobs.Config{
		DataDir:      dataDir,
		DownloadsDir: downloads,
		SDKDir:       sdkDir,
		MaxJobs:      envInt("AIO_MAX_JOBS", 2),
		TTL:          envDur("AIO_JOB_TTL", 24*time.Hour),
	})
	if err != nil {
		log.Fatal(err)
	}

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
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go shareMgr.RunCleanup(ctx, envDur("AIO_SHARE_CLEANUP", 10*time.Minute))
	go func() {
		log.Printf("listening on %s (plugins: %s, data: %s)", addr, strings.Join(pluginDirs, ", "), dataDir)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	log.Print("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.Shutdown(sctx)
}
