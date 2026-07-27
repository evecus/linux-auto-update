package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

var (
	dataDir string
	port    int
)

func init() {
	// 优先使用 cgo resolver（走 glibc getaddrinfo / NSS 解析链路），
	// 而非纯 Go resolver 直接对 /etc/resolv.conf 里的 nameserver 发 UDP 查询。
	// 这样可以和 wget/curl 等系统工具走相同的 DNS 解析路径，避免在使用
	// 透明代理、fake-ip、systemd-resolved 等环境下出现
	// "系统工具能解析、程序内 DNS 查询却解析失败" 的不一致情况。
	//
	// 注意：此设置只在编译时 CGO_ENABLED=1 才会生效；若编译时
	// CGO_ENABLED=0（本项目同时发布的纯 Go 静态版本），Go 运行时
	// 没有 cgo resolver 可用，会自动回退为纯 Go resolver，这行设置
	// 不会报错，但也不会有实际效果——因此本项目同时提供
	// CGO_ENABLED=0 和 CGO_ENABLED=1（文件名带 -cgo 后缀）两个版本
	// 的 Release 二进制，在宿主机直接部署且使用透明代理/fake-ip 时，
	// 建议下载 -cgo 版本。
	net.DefaultResolver.PreferGo = false
}

func main() {
	exePath, err := os.Executable()
	if err != nil {
		exePath = "."
	}
	defaultData := filepath.Join(filepath.Dir(exePath), "data")

	flag.StringVar(&dataDir, "dir", defaultData, "data directory")
	flag.IntVar(&port, "port", 9191, "web panel port")
	flag.Parse()

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("failed to create data dir: %v", err)
	}

	store := NewStore(dataDir)
	if err := store.Load(); err != nil {
		log.Fatalf("failed to load store: %v", err)
	}

	scheduler := NewScheduler(store)
	scheduler.Start()

	srv := NewServer(store, scheduler)
	addr := fmt.Sprintf(":%d", port)
	log.Printf("updater panel running at http://0.0.0.0%s  (data: %s)", addr, dataDir)
	if err := http.ListenAndServe(addr, srv.mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
