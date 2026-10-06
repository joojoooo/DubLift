package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dublift/internal/dublift"
)

func main() {
	path := flag.String("config", ".local/config.json", "private settings file")
	listen := flag.String("listen", "", "override listen address")
	appListen := flag.String("app-listen", "", "application-managed listen address")
	appFFmpeg := flag.String("app-ffmpeg", "", "application-managed FFmpeg executable")
	appFFprobe := flag.String("app-ffprobe", "", "application-managed ffprobe executable")
	defaultCacheMB := flag.Int("default-cache-mb", 0, "rolling cache default for new or reset config")
	flag.Parse()
	if *defaultCacheMB != 0 && (*defaultCacheMB < 32 || *defaultCacheMB > 2048) {
		log.Fatal("default cache must be 32–2048 MiB")
	}
	c, err := dublift.OpenConfigWithOptions(*path, dublift.ConfigOptions{
		AppListen:      *appListen,
		AppFFmpeg:      *appFFmpeg,
		AppFFprobe:     *appFFprobe,
		DefaultCacheMB: *defaultCacheMB,
	})
	if err != nil {
		log.Fatal(err)
	}
	s, err := dublift.NewServer(c)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	addr := c.Get().Listen
	if *listen != "" {
		addr = *listen
	}
	srv := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		s.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Printf("DubLift dashboard and addon listening on %s (config: %s)", addr, *path)
	if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
